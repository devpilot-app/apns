// Package deliver 实现 relay 的 APNs 投递语义。
//
// 规则（与设计文档 §4.5 一致,v0.3.0 增补）：
//   - approval_required：始终发 alert（priority 10、time-sensitive、过期=等待窗），
//     同时驱动两类 Live Activity（本地会话进度卡 + 云审批卡 push-to-start）；
//   - 审批 alert 只在卡面没交出去时才发：新版 App 一审批一卡，push-to-start
//     受理即抑制 alert；未受理（无 token/410/传输失败）才回落通知；
//   - approval_closed：给这条审批自己的卡发 end（dismissal-date 在过去 =
//     立即移除）；「替换通知」只发给确实收到过审批 alert、且不是自己做的
//     决策的设备，且不带动作按钮；
//   - task_complete：始终发 alert（priority 5、24h 过期）与 Live Activity end；
//   - tool_* / session_started / session_ended：不单独发 alert；
//   - Live Activity 按 (device, session) 1 秒节流、latest-wins（仅旧版 App 的
//     会话卡路径）；
//   - 审批范围推送（approval alert + 云审批 LA）默认豁免 UTC 日配额；
//   - 410 表示 token 失效，立即剪枝，不做重试。
package deliver

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/devpilot-app/apns/internal/apns"
	"github.com/devpilot-app/apns/internal/metrics"
	"github.com/devpilot-app/apns/internal/quota"
	"github.com/devpilot-app/apns/internal/store"
	"github.com/devpilot-app/apns/pkg/wire"
)

// 投递超时与节流参数。
const (
	pushTimeout      = 10 * time.Second
	defaultThrottle  = time.Second
	throttleIdle     = 60 * time.Second
	taskExpiry       = 24 * time.Hour
	approvalExpiry   = wire.APNSApprovalExpirySeconds
	liveActivityTTL  = wire.LiveActivityStaleSeconds
	closeExpiry      = 60 // close 替换推送的短过期:只服务「变脸」,过期即无声消失
	approvalPushKind = "alert"
	laPushKind       = "liveactivity"
	cloudLAPushKind  = "liveactivity:cloud"
	// legacyApprovalCategory 是 v0.2 及之前的审批 category(拼写笔误)。
	legacyApprovalCategory = "DEVPLOT_APPROVAL"
	// defaultQuestionCategory 是问题类审批(AskUserQuestion)的 category:
	// App 注册空动作,通知本体点击直达收件箱对应卡;绝不复用审批 category
	// (那串带着 Approve/Deny 动作,对问题就是「替用户拍板」)。
	defaultQuestionCategory = "DEVPILOT_QUESTION"
	// defaultResolvedCategory 是审批收场「替换通知」的 category：**不带任何
	// 动作**。审批已终结，横幅上再留 Approve/Deny 就是让用户点一个撒谎的
	// 按钮（旧实现沿用审批 category，Approved 通知展开仍可点）。
	defaultResolvedCategory = "DEVPILOT_RESOLVED"
	// alertMemoryTTL 是「这台设备收到过这条审批的 alert」记忆的保留期。
	// 只需覆盖等待窗（最长 6h 的一半够用），过期条目在下次写入时顺带清理。
	alertMemoryTTL = 2 * time.Hour
)

// Pusher 是 APNs 客户端的窄接口，便于测试注入。
type Pusher interface {
	Send(ctx context.Context, req apns.Request) (apns.Result, error)
	TopicFor() string
	LiveActivityTopic() string
}

// Config 是 Deliverer 依赖。
type Config struct {
	Store    *store.Store
	Pusher   Pusher
	Metrics  *metrics.Metrics
	Logger   *slog.Logger
	Quota    *quota.Engine
	Now      func() time.Time
	Throttle time.Duration
	// APNSCategory 是审批通知 category;空值回落旧串(拼写修正的分期切换见 config)。
	APNSCategory string
	// APNSQuestionCategory 是问题类审批(AskUserQuestion)的 category;空值
	// 回落 defaultQuestionCategory。与审批 category 分开,见 categoryFor。
	APNSQuestionCategory string
	// APNSResolvedCategory 是审批收场替换通知的 category（无动作按钮）；
	// 空值回落 defaultResolvedCategory。见 categoryForResolved。
	APNSResolvedCategory string
	// QuotaExemptApprovalPushes 时审批范围推送豁免日配额(见 push())。
	QuotaExemptApprovalPushes bool
}

// Deliverer 实现 wss.Notifier。
type Deliverer struct {
	cfg      Config
	log      *slog.Logger
	now      func() time.Time
	throttle *Throttler

	// alerted 记「某设备确实收到过某条审批的 alert」（approval|device → 时刻）。
	// 收场替换通知只发给这张表里的设备：没发过审批 alert，就没有横幅可
	// 「变脸」，照发等于凭空造出一条 Approved/Denied 通知（2026-11-23 真机：
	// 锁屏卡批完，通知中心多出两条 Approved）。进程内存态，relay 重启后
	// 最坏结果是某条已决审批的旧横幅没被替换——用户点它只会得到
	// already_decided，无害。
	alertMu sync.Mutex
	alerted map[string]time.Time
}

// New 构造 Deliverer。
func New(cfg Config) *Deliverer {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New()
	}
	if cfg.Throttle <= 0 {
		cfg.Throttle = defaultThrottle
	}
	return &Deliverer{
		cfg:      cfg,
		log:      cfg.Logger,
		now:      cfg.Now,
		throttle: NewThrottler(cfg.Throttle, throttleIdle),
		alerted:  make(map[string]time.Time),
	}
}

// Notify 处理一条事件对单台设备的投递。由 Hub 每设备起一个 goroutine 调用。
func (d *Deliverer) Notify(ctx context.Context, device store.Device, env wire.Envelope) {
	d.NotifyResolved(ctx, device, env, "")
}

// NotifyResolved 与 Notify 相同，originDeviceID 非空表示这条 approval_closed
// 由该设备的决策合成：它已经自己撤掉了审批面（系统在点按动作时移除通知，
// 决策管线以 .immediate 收卡），再补一条收场通知就是凭空多出一条
// 「Approved」。实现 wss.OriginAwareNotifier（Hub 按能力探测调用）。
func (d *Deliverer) NotifyResolved(ctx context.Context, device store.Device, env wire.Envelope, originDeviceID string) {
	if d.cfg.Pusher == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 云审批卡先行（sendCloudActivity 返回是否对该设备出了卡面推送）：
	// approval_required 且卡面已覆盖（起卡 / 旧版 App 的会话卡 update）时
	// 跳过审批 alert——锁屏同时出现卡+通知纯属重复（2026-11-11 真机
	// 反馈）。卡起不来（无 push-to-start token；APNs 未受理）时这里自然
	// 回落通知层，审批可达性不依赖本次抑制。
	cardPushed := d.sendCloudActivity(d.tierOf(env.DaemonID), device, env)
	if device.APNSToken != "" &&
		!(env.Type == wire.EventApprovalRequired && cardPushed) {
		d.sendAlert(d.tierOf(env.DaemonID), device, env, originDeviceID)
	}
	// 没有 alert token 的设备仍可能注册了 Live Activity token。
	d.sendLiveActivity(d.tierOf(env.DaemonID), device, env)
}

// tierOf 读取 daemon 档位；记录不存在（已注销级联）按 free——
// 该 daemon 本就不该再推送，free 恰是最严档。
func (d *Deliverer) tierOf(daemonID string) store.Tier {
	daemon, err := d.cfg.Store.Daemon(daemonID)
	if err != nil {
		return store.TierFree
	}
	return daemon.Tier
}

// categoryFor 选取通知的 UNNotificationCategory。
//
// 问题类审批(AskUserQuestion)单独一串:审批 category 在 App 侧注册了
// Approve/Deny 快捷动作,而问题没有 yes/no 语义——误点 Approve 就是替用户
// 放行 + 让模型猜答案(2026-11-11 拍板)。问题 category 在 App 侧注册为空
// 动作,点通知本体走默认路径直达收件箱对应卡。
// 存量 App 未注册该串时 iOS 也不展示任何自定义动作,降级天然安全。
func (d *Deliverer) categoryFor(env wire.Envelope) string {
	category := d.cfg.APNSCategory
	if category == "" {
		category = legacyApprovalCategory
	}
	if env.Payload.ToolName != "AskUserQuestion" {
		return category
	}
	if q := d.cfg.APNSQuestionCategory; q != "" {
		return q
	}
	return defaultQuestionCategory
}

// sendAlert 只对审批、审批关闭与任务完成发通知。
//
// originDeviceID 非空 = 这条 approval_closed 由该设备的决策合成（见
// NotifyResolved）；对其余事件无意义，传空即可。
func (d *Deliverer) sendAlert(tier store.Tier, device store.Device, env wire.Envelope, originDeviceID string) {
	now := d.now().Unix()
	var (
		payload    map[string]any
		expiration int64
		priority   int
		collapseID string
	)
	category := d.categoryFor(env)
	switch env.Type {
	case wire.EventApprovalRequired:
		payload = map[string]any{
			"aps": map[string]any{
				"alert":              map[string]string{"title": wire.ApprovalTitle(env), "body": wire.ApprovalBody(env)},
				"sound":              "default",
				"interruption-level": "time-sensitive",
				"thread-id":          "approval",
				"category":           category,
			},
			"type":         string(env.Type),
			"event_id":     env.EventID,
			"approval_id":  env.ApprovalID,
			"daemon_id":    env.DaemonID,
			"session_id":   env.SessionID,
			"agent":        string(env.Agent),
			"hostname":     env.Payload.Hostname,
			"tmux_session": env.Payload.TmuxSession,
			"cwd":          env.Payload.Cwd,
			"expires_at":   env.ExpiresAt,
			"ts":           env.TS,
		}
		// APNs 过期对齐等待窗:窗内设备上线仍能收到;旧 daemon 无 expires_at 回落旧值。
		expiration = now + approvalExpiry
		if env.ExpiresAt > now {
			expiration = env.ExpiresAt
		}
		priority = 10
		collapseID = "apv-" + env.ApprovalID
	case wire.EventApprovalClosed:
		// 问题类（AskUserQuestion）在 daemon 侧是「只通知不阻塞」：用户在终端
		// 答题不会产生决策回流，relay 只能等 pending 过期。这种 expired 不
		// 代表用户错过——再推一条 "Missed approval" 纯属误报。静默收场：
		// LA 由 sendCloudActivity 的 end 推送收，App 收件箱卡由 close 事件
		// 正常转态（本地已按「终端已答」记账的会保持 Answered）。
		if env.Payload.ToolName == "AskUserQuestion" &&
			env.Payload.StatusText == wire.CloseReasonExpired {
			return
		}
		// 替换通知的前置条件（2026-11-23 真机：两条都不满足时照发，通知中心
		// 凭空多出两条 Approved）：
		//   ① 不是这台设备自己的决策合成的——系统在点按动作时已经移除那条
		//      通知、决策管线已收卡，再推就是新造一条；
		//   ② 这台设备确实收到过这条审批的 alert——没有横幅就无从「变脸」，
		//      走 Live Activity 的审批收场靠 end 推送，不归这里管。
		// 例外：expired（用户错过）哪怕没发过 alert 也要提醒——审批是 LA 卡
		// 承载的时候，卡收走后用户需要一条「Missed approval」知道终端在等。
		missed := env.Payload.StatusText == wire.CloseReasonExpired
		if originDeviceID != "" && originDeviceID == device.ID {
			d.forgetAlerted(env.ApprovalID, device.ID)
			return
		}
		if !missed && !d.wasAlerted(env.ApprovalID, device.ID) {
			d.log.Info("skipping close replacement push: no approval alert was delivered to this device",
				"device_id", device.ID, "approval_id", env.ApprovalID)
			return
		}
		d.forgetAlerted(env.ApprovalID, device.ID)
		// 替换推送:同 collapse-id 让已投递的审批横幅「变脸」为终态。
		// 静默(无声音、无打断级别)——用户已在别处处理,不再打扰。
		title := env.Payload.SessionTitle
		if title == "" {
			title = env.Payload.Title
		}
		if title == "" {
			title = "Approval Resolved"
		}
		payload = map[string]any{
			"aps": map[string]any{
				"alert":     map[string]string{"title": wire.Truncate(title, wire.MaxTitleRunes), "body": closedBody(env)},
				"thread-id": "approval",
				// 无动作 category：已终结的审批不再提供 Approve/Deny。
				"category": d.categoryForResolved(),
			},
			"type":        string(env.Type),
			"event_id":    env.EventID,
			"approval_id": env.ApprovalID,
			"daemon_id":   env.DaemonID,
			"session_id":  env.SessionID,
			"agent":       string(env.Agent),
			"ts":          env.TS,
		}
		expiration = now + closeExpiry
		priority = 10
		collapseID = "apv-" + env.ApprovalID
	case wire.EventTaskComplete:
		title := env.Payload.SessionTitle
		if title == "" {
			title = env.Payload.Title
		}
		if title == "" {
			title = "Task Complete"
		}
		body := wire.Summarize(env.Payload.ReplySummary, wire.MaxReplyRunes)
		if body == "" {
			body = "Agent completed the task"
		}
		payload = map[string]any{
			"aps": map[string]any{
				"alert":     map[string]string{"title": wire.Truncate(title, wire.MaxTitleRunes), "body": body},
				"sound":     "default",
				"thread-id": "task",
			},
			"type":       string(env.Type),
			"event_id":   env.EventID,
			"daemon_id":  env.DaemonID,
			"session_id": env.SessionID,
			"agent":      string(env.Agent),
			"ts":         env.TS,
		}
		expiration = now + int64(taskExpiry.Seconds())
		priority = 5
		collapseID = "task-" + env.SessionID
	default:
		return
	}

	res := d.push(tier, approvalPushKind, device.ID, env, apns.Request{
		DeviceToken: device.APNSToken,
		Environment: device.APNSEnv,
		Topic:       d.cfg.Pusher.TopicFor(),
		PushType:    "alert",
		Priority:    priority,
		Expiration:  expiration,
		CollapseID:  collapseID,
		Payload:     payload,
	})
	if env.Type == wire.EventApprovalRequired && res.Accepted() {
		// 审批 alert 确实发出去了：收场时才有横幅可替换成终态文案。
		d.markAlerted(env.ApprovalID, device.ID)
	}
	if res.Prune {
		// device token 已失效：清空，等待 App 下次启动重新上报。
		if err := d.cfg.Store.ClearDeviceAPNSToken(device.ID); err != nil {
			d.log.Warn("failed to clear stale device token", "device_id", device.ID, "err", err)
		}
	}
}

// categoryForResolved 返回收场替换通知的 category（配置优先，回落默认串）。
func (d *Deliverer) categoryForResolved() string {
	if c := d.cfg.APNSResolvedCategory; c != "" {
		return c
	}
	return defaultResolvedCategory
}

func alertedKey(approvalID, deviceID string) string { return approvalID + "|" + deviceID }

// markAlerted 记一次「审批 alert 已被 APNs 受理」。表过长时顺带清掉过期项，
// 不设后台协程。
func (d *Deliverer) markAlerted(approvalID, deviceID string) {
	d.alertMu.Lock()
	defer d.alertMu.Unlock()
	now := d.now()
	if len(d.alerted) > 256 {
		cutoff := now.Add(-alertMemoryTTL)
		for k, at := range d.alerted {
			if at.Before(cutoff) {
				delete(d.alerted, k)
			}
		}
	}
	d.alerted[alertedKey(approvalID, deviceID)] = now
}

func (d *Deliverer) wasAlerted(approvalID, deviceID string) bool {
	d.alertMu.Lock()
	defer d.alertMu.Unlock()
	key := alertedKey(approvalID, deviceID)
	at, ok := d.alerted[key]
	if !ok {
		return false
	}
	if d.now().Sub(at) > alertMemoryTTL {
		delete(d.alerted, key)
		return false
	}
	return true
}

func (d *Deliverer) forgetAlerted(approvalID, deviceID string) {
	d.alertMu.Lock()
	delete(d.alerted, alertedKey(approvalID, deviceID))
	d.alertMu.Unlock()
}

// closedBody 生成审批关闭替换推送的正文,按 close reason 映射终态文案。
func closedBody(env wire.Envelope) string {
	switch env.Payload.StatusText {
	case wire.CloseReasonApproved:
		return "Approved"
	case wire.CloseReasonDenied:
		return "Denied"
	case wire.CloseReasonExpired:
		return "Missed approval — open the session to continue"
	case wire.CloseReasonCancelled:
		return "Answered at terminal"
	default:
		return "Resolved"
	}
}

// sendLiveActivity 把事件转发到该设备当前会话的 LA token。
func (d *Deliverer) sendLiveActivity(tier store.Tier, device store.Device, env wire.Envelope) {
	tokens, err := d.cfg.Store.LATokensForDaemon(env.DaemonID)
	if err != nil {
		d.log.Warn("failed to list Live Activity tokens", "daemon_id", env.DaemonID, "err", err)
		return
	}
	if len(tokens) == 0 {
		return
	}
	now := d.now().Unix()
	push := wire.LiveActivityPushFor(env, now)
	priority := 5
	if env.Type == wire.EventApprovalRequired {
		priority = 10
	}
	for _, tok := range tokens {
		// 审批级 token 属于云审批卡（CloudApprovalAttributes），不是本地会话
		// 进度卡（AgentActivityAttributes）：把 L1 载荷推到它上面是隐性串线
		// （content-state 形状对不上，轻则被丢、重则 410 误剪），一律跳过。
		if tok.ApprovalID != "" ||
			tok.DeviceID != device.ID || tok.SessionID != env.SessionID || tok.PushToken == "" {
			continue
		}
		token, session := tok.PushToken, tok.SessionID
		key := device.ID + "/" + session
		d.throttle.Do(key, func() {
			res := d.push(tier, laPushKind, device.ID, env, apns.Request{
				DeviceToken: token,
				Environment: device.APNSEnv,
				Topic:       d.cfg.Pusher.LiveActivityTopic(),
				PushType:    "liveactivity",
				Priority:    priority,
				Expiration:  now + liveActivityTTL,
				Payload:     push,
			})
			if res.Prune {
				// token 已失效：剪枝，后续事件不再尝试。
				if err := d.cfg.Store.DeleteLAToken(device.ID, session); err != nil {
					d.log.Warn("failed to delete stale LA token", "device_id", device.ID, "session_id", session, "err", err)
				}
			}
		})
	}
}

// push 发送一条 APNs 请求并记录指标。
func (d *Deliverer) push(tier store.Tier, kind, deviceID string, env wire.Envelope, req apns.Request) apns.Result {
	// 日配额闸在 APNs 发送之前：这是 APNs 成本唯一漏斗。被拒只计数，
	// 不触碰 token；实时 WSS/inbox/resync 不受影响，App 前台仍可审批（fail-safe）。
	// 审批范围推送默认豁免:LA 更新+替换推送会把免费档 10 条/日烧穿,审批不能被配额哑掉。
	if d.cfg.Quota != nil &&
		!(d.cfg.QuotaExemptApprovalPushes && approvalScoped(env)) &&
		!d.cfg.Quota.AllowPush(tier, env.DaemonID) {
		d.cfg.Metrics.IncQuotaRejected("push_" + kind)
		d.log.Info("push blocked by daily quota", "kind", kind, "daemon_id", env.DaemonID,
			"device_id", deviceID, "event_id", env.EventID)
		return apns.Result{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	res, err := d.cfg.Pusher.Send(ctx, req)
	status := strconv.Itoa(res.Status)
	switch {
	case err != nil:
		status = "error"
		d.log.Warn("APNs push failed", "kind", kind, "device_id", deviceID,
			"event_id", env.EventID, "status", res.Status, "reason", res.Reason, "err", err)
	case res.Skipped:
		status = "skipped"
		d.log.Debug("APNs push skipped (topic backoff)", "kind", kind, "device_id", deviceID, "event_id", env.EventID)
	case !res.Accepted():
		level := slog.LevelWarn
		if res.Status == 400 || res.Status == 403 {
			level = slog.LevelError
		}
		d.log.Log(context.Background(), level, "APNs rejected push", "kind", kind, "device_id", deviceID,
			"event_id", env.EventID, "status", res.Status, "reason", res.Reason, "prune", res.Prune)
	}
	d.cfg.Metrics.IncPush(kind, status)
	return res
}

// sendCloudActivity 驱动云审批 LA(CloudApprovalAttributes)。两代 App 并存，
// 按设备手里登记的是哪一种 token 分流：
//
//   - **一审批一卡**（新版 App，只上报审批级 token）：每条 approval_required
//     同步发一次 push-to-start start 起它自己的卡；approval_closed 给这张卡
//     发 end（dismissal-date 在过去 = 立即移除）。卡的身份是审批，批一条
//     收一张，不存在「批完一条卡面换脸」。
//   - **一会话一卡**（旧版 App，上报会话级 token）：保持原有行为——会话
//     token 发 update/end、节流 latest-wins、收场时把卡面分流给同会话下一条
//     待决。发版窗口内旧 App 不受影响。
//
// 返回本次是否对该设备出了卡面推送（approval_required 的 start/update），
// Notify 据此抑制同内容的审批 alert。
func (d *Deliverer) sendCloudActivity(tier store.Tier, device store.Device, env wire.Envelope) bool {
	isEnd := env.Type == wire.EventApprovalClosed ||
		env.Type == wire.EventTaskComplete || env.Type == wire.EventSessionEnded
	if env.Type != wire.EventApprovalRequired && !isEnd {
		return false
	}
	tokens, err := d.cfg.Store.LATokensForDaemon(env.DaemonID)
	if err != nil {
		d.log.Warn("failed to list Live Activity tokens", "daemon_id", env.DaemonID, "err", err)
		return false
	}
	var sessionToken string
	for _, tok := range tokens {
		if tok.ApprovalID == "" && tok.DeviceID == device.ID &&
			tok.SessionID == env.SessionID && tok.PushToken != "" {
			sessionToken = tok.PushToken
			break
		}
	}

	// 新版 App：没有会话级 token → 一审批一卡。
	if sessionToken == "" {
		switch env.Type {
		case wire.EventApprovalRequired:
			return d.startApprovalCard(tier, device, env)
		case wire.EventApprovalClosed:
			d.endApprovalCard(tier, device, env, nil)
		case wire.EventSessionEnded:
			// 会话收场是安全网：残留的审批卡（审批已被 Ctrl-C 取消等）一并收走。
			for i := range tokens {
				tok := &tokens[i]
				if tok.ApprovalID == "" || tok.DeviceID != device.ID || tok.SessionID != env.SessionID {
					continue
				}
				d.endApprovalCard(tier, device, env, tok)
			}
		}
		return false
	}
	return d.sendSessionCard(tier, device, env, sessionToken)
}

// startApprovalCard 为一条审批起它自己的卡（push-to-start），同步等 APNs
// 结果：受理 = 卡面已交出去（抑制同内容的审批 alert）；未受理（设备没有
// push-to-start token / 410 / 传输失败）= 回落 alert。出卡裁决不再经过节流
// 闭包——旧实现调用方只等 Throttle+100ms，APNs 稍慢就判「没出卡」，alert
// 照发而卡也在路上，同一审批卡+通知双份。
func (d *Deliverer) startApprovalCard(tier store.Tier, device store.Device, env wire.Envelope) bool {
	if device.PushToStartToken == "" {
		return false
	}
	now := d.now().Unix()
	res := d.push(tier, cloudLAPushKind, device.ID, env, apns.Request{
		DeviceToken: device.PushToStartToken,
		Environment: device.APNSEnv,
		Topic:       d.cfg.Pusher.LiveActivityTopic(),
		PushType:    "liveactivity",
		Priority:    10,
		Expiration:  now + liveActivityTTL,
		Payload:     wire.CloudApprovalPushFor(env, now, true),
	})
	if res.Prune {
		if err := d.cfg.Store.SetPushToStartToken(device.ID, "", now); err != nil {
			d.log.Warn("failed to clear stale push-to-start token", "device_id", device.ID, "err", err)
		}
		return false
	}
	if !res.Accepted() {
		d.log.Info("cloud approval card not accepted by APNs; falling back to alert",
			"device_id", device.ID, "approval_id", env.ApprovalID, "status", res.Status, "reason", res.Reason)
	}
	return res.Accepted()
}

// endApprovalCard 给一条审批自己的卡发收场 end 推送。
//
// tok 可为 nil（按 approval_id 现查）。卡 token 由 App 在卡出现时上报
// （/v1/la-tokens 带 approval_id）；没上报就无卡可收，卡面清理由 App 侧对账
// 兜底。发完即剪：审批已终结，这条 token 不再有用途；仅传输类失败保留，
// 留给 cleanup 的 TTL。
func (d *Deliverer) endApprovalCard(tier store.Tier, device store.Device, env wire.Envelope, tok *store.LAToken) {
	if tok == nil {
		found, ok, err := d.cfg.Store.LATokenForApproval(device.ID, env.ApprovalID)
		if err != nil {
			d.log.Warn("failed to read approval LA token", "device_id", device.ID, "approval_id", env.ApprovalID, "err", err)
			return
		}
		if !ok {
			return
		}
		tok = &found
	}
	now := d.now().Unix()
	res := d.push(tier, cloudLAPushKind, device.ID, env, apns.Request{
		DeviceToken: tok.PushToken,
		Environment: device.APNSEnv,
		Topic:       d.cfg.Pusher.LiveActivityTopic(),
		PushType:    "liveactivity",
		Priority:    10,
		Expiration:  now + liveActivityTTL,
		Payload:     wire.CloudApprovalPushFor(env, now, false),
	})
	// Status 0 = 没发出去（传输失败/配额拦截），不剪 token。
	if res.Status != 0 {
		if err := d.cfg.Store.DeleteApprovalLAToken(device.ID, tok.ApprovalID); err != nil {
			d.log.Warn("failed to delete approval LA token", "device_id", device.ID, "approval_id", tok.ApprovalID, "err", err)
		}
	}
}

// sendSessionCard 是旧版 App（上报会话级 token）的会话卡路径，行为与重构前
// 一致：会话 token 发 update/end，节流 latest-wins，close 时把卡面分流给同
// 会话下一条待决。sessionToken 由 sendCloudActivity 查好传入（非空）。
func (d *Deliverer) sendSessionCard(tier store.Tier, device store.Device, env wire.Envelope, sessionToken string) bool {
	now := d.now().Unix()
	priority := 5
	if env.Type == wire.EventApprovalRequired {
		priority = 10
	}

	// —— 会话待决集合：卡面计数与收场分流共用（2026-11-22 真机案）——
	//
	// 计数：approval_required 推送带同会话待决总数（含本条），iOS 侧卡面
	// 出「N pending」徽标——并行工具调用同秒多条审批时，用户批掉卡面
	// 那条会以为全批完了，实际另一条的 hook 还挂在终端等。
	//
	// 收场分流：close 时同会话**还有**待决 → 不推 end（会把整张卡收走，
	// 剩下的审批在锁屏零可见——它的 alert 当初又被「卡面曾覆盖」抑制过，
	// 等于双重抹除），改推「下一条待决」的 update：卡面自动切到它并带
	// 最新计数。查询排除本条 approval_id：daemon 自发 close 走 onEvent
	// 常规路径，relay 侧 pending 标记未落地，不排除会选中刚收场的自己。
	faceEnv := env
	pendingCount := 0
	switch env.Type {
	case wire.EventApprovalRequired:
		if pendings, err := d.cfg.Store.PendingForSession(env.DaemonID, env.SessionID, "", now); err != nil {
			d.log.Warn("failed to list session pendings", "daemon_id", env.DaemonID, "session_id", env.SessionID, "err", err)
		} else {
			pendingCount = len(pendings)
		}
	case wire.EventApprovalClosed:
		if rest, err := d.cfg.Store.PendingForSession(env.DaemonID, env.SessionID, env.ApprovalID, now); err != nil {
			d.log.Warn("failed to list session pendings for close handoff", "daemon_id", env.DaemonID, "session_id", env.SessionID, "err", err)
		} else if len(rest) > 0 {
			faceEnv = rest[len(rest)-1].Event // ts 升序末位 = 最新到场
			pendingCount = len(rest)
		}
	}
	// CloudApprovalPushFor 按 env.Type 决定 update/end——faceEnv 已是
	// approval_required 时自然生成 update + needsYou + 倒计时。
	cloudPush := func(isStart bool) wire.CloudApprovalPush {
		push := wire.CloudApprovalPushFor(faceEnv, now, isStart)
		if pendingCount > 1 {
			push.APS.ContentState.PendingCount = pendingCount
		}
		return push
	}

	// 路径一:会话级 token(App 前台起的卡)——update/end。
	if sessionToken != "" {
		token, session := sessionToken, env.SessionID
		key := "cloud/" + device.ID + "/" + session
		push := cloudPush(false)
		// 出卡裁决(alert 抑制判据)经 channel 回传:闭包在节流 goroutine 上
		// 执行(Throttler 首次调用也是异步),旧实现直接 return 捕获变量是
		// 数据竞争(CI -race 实锤,2026-11-23 定位),且读值是掷硬币——首跑
		// 真值常被读丢,卡面已出而 alert 照发(2026-11-11「卡+通知重复」的
		// 隐性放大器)。verdict 在闭包出口单点送出(channel 手递=happens-
		// before),调用方带界等待:首跑亚毫秒拿到真值;闭包被 latest-wins
		// 挤掉(同会话更前辈换)则超时回落 false → alert 照发,与既往
		// 主导行为一致(被挤掉的卡面本就由最新调用代理)。
		surfacedCh := make(chan bool, 1)
		verdict := false
		d.throttle.Do(key, func() {
			defer func() { surfacedCh <- verdict }()
			res := d.push(tier, cloudLAPushKind, device.ID, env, apns.Request{
				DeviceToken: token,
				Environment: device.APNSEnv,
				Topic:       d.cfg.Pusher.LiveActivityTopic(),
				PushType:    "liveactivity",
				Priority:    priority,
				Expiration:  now + liveActivityTTL,
				Payload:     push,
			})
			if !res.Prune {
				verdict = faceEnv.Type == wire.EventApprovalRequired
				return
			}
			// 410 = 会话级 token 指向的卡已不在系统里。典型:锁屏按钮 end 了卡
			// 而 Intent 侧不注销 token(只有 App 侧 end(snapshot) 报 la-tokens
			// end),App 又挂起错过重报——relay 缓存的会话 token 成了死地址。
			// 剪枝(end 同样生效,原逻辑保留)。
			if err := d.cfg.Store.DeleteLAToken(device.ID, session); err != nil {
				d.log.Warn("failed to delete stale LA token", "device_id", device.ID, "session_id", session, "err", err)
			}
			// approval_required 不能就此打住:update 打在死卡上等于什么都没
			// 发,而路径一直接返回 true 会把 alert 也抑制掉——这条审批就
			// 「无卡无通知」了(2026-11-20 真机:同会话连续审批,App 挂起窗内
			// 锁屏按钮逐张 end 后,后续审批对手机完全不可见,直到 App 回前台
			// 重报 token)。落 push-to-start 起卡;起不了(强退语义)裁决 false,
			// alert 兜底。faceEnv 判定:收场分流后真正要送达的是
			// 下一条待决(approval_required),同样要兜底起卡。
			if faceEnv.Type != wire.EventApprovalRequired || device.PushToStartToken == "" {
				return
			}
			startRes := d.push(tier, cloudLAPushKind, device.ID, env, apns.Request{
				DeviceToken: device.PushToStartToken,
				Environment: device.APNSEnv,
				Topic:       d.cfg.Pusher.LiveActivityTopic(),
				PushType:    "liveactivity",
				Priority:    priority,
				Expiration:  now + liveActivityTTL,
				Payload:     cloudPush(true),
			})
			if startRes.Prune {
				if err := d.cfg.Store.SetPushToStartToken(device.ID, "", now); err != nil {
					d.log.Warn("failed to clear stale push-to-start token", "device_id", device.ID, "err", err)
				}
				return
			}
			verdict = true
		})
		select {
		case surfaced := <-surfacedCh:
			return surfaced
		case <-time.After(d.cfg.Throttle + 100*time.Millisecond):
			// 本闭包被同 key 更新调用替换、不再执行:裁决回落 false,
			// alert 照发(卡面由最新调用负责)。
			return false
		}
	}

	// 没有会话级 token 的设备由 sendCloudActivity 的一审批一卡路径负责，
	// 这里不再兜底起卡。
	return false
}

// approvalScoped 判定审批范围事件(alert 替换 + LA 驱动都豁免日配额的集合)。
func approvalScoped(env wire.Envelope) bool {
	return env.Type == wire.EventApprovalRequired || env.Type == wire.EventApprovalClosed
}

// ---------- 节流 ----------

// Throttler 对同一 key 的投递做 latest-wins 节流。
//
// 第一次调用立即执行；随后每个 interval 至多执行一次，期间到达的
// 多个动作只保留最后一个。空闲 idle 后协程自行退出，避免泄漏。
type Throttler struct {
	interval time.Duration
	idle     time.Duration
	mu       sync.Mutex
	states   map[string]*throttleState
}

type throttleState struct {
	ch chan func()
}

// NewThrottler 创建节流器。
func NewThrottler(interval, idle time.Duration) *Throttler {
	return &Throttler{interval: interval, idle: idle, states: make(map[string]*throttleState)}
}

// Do 提交一个动作；同 key 的待执行动作会被最新调用替换。
func (t *Throttler) Do(key string, fn func()) {
	t.mu.Lock()
	st := t.states[key]
	if st == nil {
		st = &throttleState{ch: make(chan func(), 1)}
		t.states[key] = st
		go t.loop(key, st)
	}
	t.mu.Unlock()

	// 非阻塞写入；槽位已满时丢弃旧动作，保证 latest-wins。
	select {
	case st.ch <- fn:
		return
	default:
	}
	select {
	case <-st.ch:
	default:
	}
	select {
	case st.ch <- fn:
	default:
	}
}

func (t *Throttler) loop(key string, st *throttleState) {
	idle := time.NewTimer(t.idle)
	defer idle.Stop()
	for {
		select {
		case fn := <-st.ch:
			fn()
			// 节流间隔内到达的新动作会覆盖缓冲槽位，下一轮只发最新。
			select {
			case <-time.After(t.interval):
			case <-idle.C:
				t.forget(key, st)
				return
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(t.idle)
		case <-idle.C:
			t.forget(key, st)
			return
		}
	}
}

func (t *Throttler) forget(key string, st *throttleState) {
	t.mu.Lock()
	if t.states[key] == st {
		delete(t.states, key)
	}
	t.mu.Unlock()
}
