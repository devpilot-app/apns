package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestVectorsRoundTrip 保证契约向量可以解码、通过校验、重新编码后语义不变。
// iOS 侧应加载同一批文件做 Codable 测试。
func TestVectorsRoundTrip(t *testing.T) {
	dir := filepath.Join("testdata", "vectors")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取向量目录失败: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("向量目录为空")
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatalf("读取向量失败: %v", err)
			}
			first, err := Decode(data)
			if err != nil {
				t.Fatalf("解码失败: %v", err)
			}
			encoded, err := Encode(first)
			if err != nil {
				t.Fatalf("编码失败: %v", err)
			}
			second, err := Decode(encoded)
			if err != nil {
				t.Fatalf("二次解码失败: %v", err)
			}
			// 直接比较结构体会把 nil 切片与空切片误判为不同；
			// 比较重新编码后的字节既能覆盖字段，也能覆盖序列化稳定性。
			reencoded, err := Encode(second)
			if err != nil {
				t.Fatalf("二次编码失败: %v", err)
			}
			if !reflect.DeepEqual(encoded, reencoded) {
				t.Fatalf("往返后不一致:\n第一次: %s\n第二次: %s", encoded, reencoded)
			}
		})
	}
}

// TestDecodeRejectsUnsupportedVersion 确认版本不匹配时直接拒绝。
func TestDecodeRejectsUnsupportedVersion(t *testing.T) {
	_, err := Decode([]byte(`{"v":2,"kind":"event"}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("期望版本错误，实际 %v", err)
	}
}

// TestDecodeRejectsUnknownKind 确认未知 kind 不会被静默接受。
func TestDecodeRejectsUnknownKind(t *testing.T) {
	if _, err := Decode([]byte(`{"v":1,"kind":"whatever"}`)); err == nil {
		t.Fatal("期望未知 kind 报错")
	}
}

// TestApprovalRequiredNeedsID 确认审批事件必须带 approval_id。
func TestApprovalRequiredNeedsID(t *testing.T) {
	env := Envelope{
		V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
		Type: EventApprovalRequired, Agent: AgentClaude,
		DaemonID: "dd_x", SessionID: "s", TS: 1,
	}
	if err := env.Validate(); err == nil {
		t.Fatal("缺少 approval_id 时应报错")
	}
}

// TestApprovalClosedNeedsID 确认审批关闭事件也必须带 approval_id。
func TestApprovalClosedNeedsID(t *testing.T) {
	env := Envelope{
		V: Version, Kind: KindEvent, MsgID: "m", EventID: "close-x",
		Type: EventApprovalClosed, Agent: AgentClaude,
		DaemonID: "dd_x", SessionID: "s", TS: 1,
	}
	if err := env.Validate(); err == nil {
		t.Fatal("approval_closed 缺少 approval_id 时应报错")
	}
}

// TestAgentOpenCodeValid 确认 opencode 通过 agent 白名单,锁屏抬头带品牌名。
func TestAgentOpenCodeValid(t *testing.T) {
	env := Envelope{
		V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
		Type: EventApprovalRequired, Agent: AgentOpenCode,
		DaemonID: "dd_x", SessionID: "s", ApprovalID: "apv_x", TS: 1,
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("opencode 应通过校验,实际 %v", err)
	}
	if got := DisplayAgent(AgentOpenCode); got != "OpenCode" {
		t.Fatalf("锁屏抬头应显示 OpenCode,实际 %q", got)
	}
}

// TestApprovalIDDeterministic 确认键顺序与空白不影响审批 ID。
func TestApprovalIDDeterministic(t *testing.T) {
	a, err := ApprovalID("sess", "Bash", "tu_1", json.RawMessage(`{"command":"ls","cwd":"/tmp"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ApprovalID("sess", "Bash", "tu_1", json.RawMessage("{\n  \"cwd\": \"/tmp\",\n  \"command\": \"ls\"\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("审批 ID 应确定: %s != %s", a, b)
	}
	if !strings.HasPrefix(a, "apv_") || len(a) != 4+26 {
		t.Fatalf("审批 ID 格式不符: %q", a)
	}

	c, _ := ApprovalID("sess", "Bash", "tu_2", json.RawMessage(`{"command":"ls"}`))
	if a == c {
		t.Fatal("不同 tool_use_id 不应产生相同 ID")
	}
}

// TestCanonicalJSONPreservesNumbers 确认大整数不会被浮点化。
func TestCanonicalJSONPreservesNumbers(t *testing.T) {
	out, err := CanonicalJSON([]byte(`{"b":9007199254740993,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":1,"b":9007199254740993}` {
		t.Fatalf("规范化结果不符: %s", out)
	}
}

// TestCanonicalJSONRejectsTrailing 确认多余内容被拒绝。
func TestCanonicalJSONRejectsTrailing(t *testing.T) {
	if _, err := CanonicalJSON([]byte(`{} {}`)); err == nil {
		t.Fatal("期望尾随内容报错")
	}
}

// TestTruncateCountsRunes 确认按字符而非字节截断。
func TestTruncateCountsRunes(t *testing.T) {
	s := strings.Repeat("中", 10)
	got := Truncate(s, 5)
	if len([]rune(got)) != 5 || !strings.HasSuffix(got, "…") {
		t.Fatalf("截断结果不符: %q", got)
	}
	if Truncate("短", 5) != "短" {
		t.Fatal("未超限不应改动")
	}
}

// TestContentStateUsesEpochSeconds 固化 Live Activity 时间契约。
func TestContentStateUsesEpochSeconds(t *testing.T) {
	env := Envelope{
		Type:      EventApprovalRequired,
		SessionID: "sess_0123456789",
		Payload:   Payload{SessionTitle: "重构支付模块"},
		TS:        1760770000,
	}
	cs := ContentStateFor(env)
	if cs.UpdatedAt != 1760770000 {
		t.Fatalf("updatedAt 必须是 Unix 秒，实际 %d", cs.UpdatedAt)
	}
	if !cs.IsAlert || cs.StatusText != "Waiting for approval" {
		t.Fatalf("审批态映射不符: %+v", cs)
	}

	// 序列化后必须是整数，不能是字符串或浮点。
	raw, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["updatedAt"].(float64); !ok {
		t.Fatalf("updatedAt 应为 JSON 数字: %s", raw)
	}
}

// TestLiveActivityEndsOnSessionEnd 确认结束事件映射为 end 并带 dismissal。
func TestLiveActivityEndsOnSessionEnd(t *testing.T) {
	push := LiveActivityPushFor(Envelope{Type: EventSessionEnded, SessionID: "s", TS: 100}, 200)
	if push.APS.Event != "end" {
		t.Fatalf("期望 end，实际 %q", push.APS.Event)
	}
	if push.APS.DismissalDate != 200+LiveActivityStaleSeconds {
		t.Fatalf("dismissal-date 不符: %d", push.APS.DismissalDate)
	}

	update := LiveActivityPushFor(Envelope{Type: EventToolRunning, SessionID: "s", TS: 100}, 200)
	if update.APS.Event != "update" {
		t.Fatalf("期望 update，实际 %q", update.APS.Event)
	}
}

// TestCloudApprovalPushShapes 固化云审批 LA 推送契约：
// push-to-start 带 attributes/attributes-type；needsYou 倒计时从 expires_at 派生
// stale；close/task_complete/session_end 映射为 end 且带短 dismissal。
func TestCloudApprovalPushShapes(t *testing.T) {
	approval := Envelope{
		Type: EventApprovalRequired, ApprovalID: "apv_x",
		DaemonID: "dd_1", SessionID: "sess_1", Agent: AgentClaude,
		ExpiresAt: 1000, TS: 100,
		Payload: Payload{SessionTitle: "支付重构", ToolName: "Bash",
			CommandSummary: "make deploy", Hostname: "mbp.local",
			Options: []string{"Postgres", "SQLite", "Cloud KV"}},
	}

	start := CloudApprovalPushFor(approval, 200, true)
	if start.APS.Event != "start" {
		t.Fatalf("期望 start，实际 %q", start.APS.Event)
	}
	if start.APS.AttributesType != CloudApprovalAttributesType {
		t.Fatalf("attributes-type 不符: %q", start.APS.AttributesType)
	}
	if start.APS.Attributes["sessionId"] != "sess_1" || start.APS.Attributes["daemonId"] != "dd_1" {
		t.Fatalf("push-to-start attributes 不符: %v", start.APS.Attributes)
	}
	if start.APS.StaleDate != approval.ExpiresAt+60 {
		t.Fatalf("needsYou stale 应为 expires_at+60，实际 %d", start.APS.StaleDate)
	}
	if cs := start.APS.ContentState; cs.State != CloudStateNeedsYou || cs.ExpiresAt != 1000 {
		t.Fatalf("needsYou content-state 不符: %+v", cs)
	}
	// 选项型审批的候选标签必须透传进 content-state（锁屏卡出选项钮）。
	if got := start.APS.ContentState.Options; len(got) != 3 || got[0] != "Postgres" {
		t.Fatalf("content-state options 透传不符: %v", got)
	}
	// start 必须带 alert：缺这个键 APNs 照样 200，iOS 端静默不起卡。
	if start.APS.Alert == nil {
		t.Fatal("push-to-start 必须带 alert（Apple 文档对 start 事件的要求）")
	}
	if start.APS.Alert.Title != "支付重构" || start.APS.Alert.Body != "Claude wants to run Bash: make deploy" {
		t.Fatalf("start alert 文案不符: %+v", start.APS.Alert)
	}

	update := CloudApprovalPushFor(approval, 200, false)
	if update.APS.Event != "update" || update.APS.Attributes != nil {
		t.Fatalf("update 推送不应带 attributes: %+v", update.APS)
	}
	if update.APS.Alert != nil {
		t.Fatalf("update 推送不应带 alert: %+v", update.APS.Alert)
	}

	for _, c := range []struct {
		typ    EventType
		reason string
		want   string
	}{
		{EventApprovalClosed, CloseReasonApproved, CloudStateApproved},
		{EventApprovalClosed, CloseReasonDenied, CloudStateDenied},
		{EventApprovalClosed, CloseReasonExpired, CloudStateExpired},
		{EventApprovalClosed, CloseReasonCancelled, CloudStateCancelled},
	} {
		env := approval
		env.Type = c.typ
		env.Payload.StatusText = c.reason
		end := CloudApprovalPushFor(env, 200, false)
		if end.APS.Event != "end" {
			t.Fatalf("%s 期望 end，实际 %q", c.reason, end.APS.Event)
		}
		if end.APS.DismissalDate != 199 {
			t.Fatalf("%s dismissal 应为 now-1（过去时间 = 立即移除），实际 %d", c.reason, end.APS.DismissalDate)
		}
		if end.APS.ContentState.State != c.want {
			t.Fatalf("终态映射不符: 期望 %s 实际 %s", c.want, end.APS.ContentState.State)
		}
	}

	// 非审批事件：卡片回到 working 形态，无倒计时。
	working := CloudApprovalPushFor(Envelope{Type: EventToolRunning, SessionID: "s", TS: 100}, 200, false)
	if working.APS.ContentState.State != CloudStateWorking || working.APS.ContentState.ExpiresAt != 0 {
		t.Fatalf("working content-state 不符: %+v", working.APS.ContentState)
	}
}

// TestHelloValidation 确认角色与身份字段的约束。
func TestHelloValidation(t *testing.T) {
	ok := Hello{V: Version, Kind: KindHello, MsgID: "m", Role: RoleApp, DeviceID: "d"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法 hello 被拒: %v", err)
	}
	bad := Hello{V: Version, Kind: KindHello, MsgID: "m", Role: RoleDaemon}
	if err := bad.Validate(); err == nil {
		t.Fatal("daemon 缺少 daemon_id 应报错")
	}
}

// TestPayloadTmuxWindowOmitEmpty 钉 tmux_window 的加性契约:空值不落键
// (旧 daemon/旧 App parity),非空原样往返。
func TestPayloadTmuxWindowOmitEmpty(t *testing.T) {
	without, err := Encode(&Envelope{V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
		Type: EventApprovalRequired, Agent: AgentClaude, DaemonID: "dd", SessionID: "s", Seq: 1, TS: 1,
		Payload: Payload{Title: "t"}})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if strings.Contains(string(without), "tmux_window") {
		t.Fatal("空 TmuxWindow 不应落键")
	}
	env := Envelope{V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
		Type: EventApprovalRequired, Agent: AgentClaude, DaemonID: "dd", SessionID: "s", Seq: 1, TS: 1,
		Payload: Payload{TmuxWindow: "2"}}
	with, err := Encode(&env)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if !strings.Contains(string(with), `"tmux_window":"2"`) {
		t.Fatalf("非空 TmuxWindow 应落键: %s", with)
	}
	var back Envelope
	if err := json.Unmarshal(with, &back); err != nil || back.Payload.TmuxWindow != "2" {
		t.Fatalf("往返丢失 TmuxWindow: %v %q", err, back.Payload.TmuxWindow)
	}
}

// TestCloudApprovalPendingCountOmission 固化 pendingCount 的 wire 形状
// （2026-11-22 并行双审批案）：≤1 整键不发（旧 App 解码回落 1，双向
// 兼容），>1 必须发 camelCase 键——iOS 端 ContentState 按键名解码，漂移
// 表现为锁屏徽标静默不出现。
func TestCloudApprovalPendingCountOmission(t *testing.T) {
	base := Envelope{
		Type: EventApprovalRequired, ApprovalID: "apv_x",
		DaemonID: "dd_1", SessionID: "sess_1", Agent: AgentClaude,
		ExpiresAt: 1000, TS: 100,
		Payload: Payload{SessionTitle: "Fix build", ToolName: "Read"},
	}
	single := CloudApprovalPushFor(base, 200, false)
	blob, err := json.Marshal(single)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "pendingCount") {
		t.Fatalf("单条待决不得发 pendingCount 键(旧 App 回落 1): %s", blob)
	}

	multi := CloudApprovalPushFor(base, 200, false)
	multi.APS.ContentState.PendingCount = 3
	blob, err = json.Marshal(multi)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"pendingCount":3`) {
		t.Fatalf("多待决必须发 pendingCount 键: %s", blob)
	}
}

// TestPayloadRouteFieldsOmitEmpty 钉 project_dir / tmux_pane 的加性契约：
// 空值不落键（旧 daemon / 旧 relay parity），非空原样往返。
func TestPayloadRouteFieldsOmitEmpty(t *testing.T) {
	without, err := Encode(&Envelope{V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
		Type: EventApprovalRequired, ApprovalID: "apv_1", Agent: AgentClaude, DaemonID: "dd", SessionID: "s", Seq: 1, TS: 1,
		Payload: Payload{Title: "t", Cwd: "/repo/sub"}})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if strings.Contains(string(without), "project_dir") || strings.Contains(string(without), "tmux_pane") {
		t.Fatalf("空 ProjectDir / TmuxPane 不应落键: %s", without)
	}
	with, err := Encode(&Envelope{V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
		Type: EventApprovalRequired, ApprovalID: "apv_1", Agent: AgentClaude, DaemonID: "dd", SessionID: "s", Seq: 1, TS: 1,
		Payload: Payload{Title: "t", ProjectDir: "/repo", TmuxPane: "%3"}})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if !strings.Contains(string(with), `"project_dir":"/repo"`) || !strings.Contains(string(with), `"tmux_pane":"%3"`) {
		t.Fatalf("非空 ProjectDir / TmuxPane 应落键: %s", with)
	}
	var back Envelope
	if err := json.Unmarshal(with, &back); err != nil || back.Payload.ProjectDir != "/repo" || back.Payload.TmuxPane != "%3" {
		t.Fatalf("往返丢失路由字段: %v %q %q", err, back.Payload.ProjectDir, back.Payload.TmuxPane)
	}
}

// TestLATokenApprovalIDRoundTrip 一审批一卡的 token 上报带 approval_id；
// 不带时是旧版 App 的会话级 token，语义不变。
func TestLATokenApprovalIDRoundTrip(t *testing.T) {
	raw, err := Encode(&LAToken{V: Version, Kind: KindLAToken, MsgID: "m", DaemonID: "dd", SessionID: "s",
		PushToken: "tok", Event: LAStart, TS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "approval_id") {
		t.Fatalf("会话级 token 不应落 approval_id 键: %s", raw)
	}
	with, err := Encode(&LAToken{V: Version, Kind: KindLAToken, MsgID: "m", DaemonID: "dd", SessionID: "s",
		ApprovalID: "apv_1", PushToken: "tok", Event: LAStart, TS: 1})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := Decode(with)
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := msg.(*LAToken)
	if !ok || tok.ApprovalID != "apv_1" {
		t.Fatalf("审批级 token 往返丢失 approval_id: %T %+v", msg, msg)
	}
	if err := tok.Validate(); err != nil {
		t.Fatalf("审批级 token 应通过校验: %v", err)
	}
}

// TestCloudApprovalStateVectors 三仓共享的 content-state 契约向量
// （testdata/cloud_approval_state.json；iOS TestsApp 用同一份文件做 Codable
// 测试）。每个用例：Go 结构解码后重新编码，必须与向量的 JSON 逐键一致——
// 多一个键说明 struct 多发，少一个键说明 iOS 侧期望的字段 relay 发不出。
func TestCloudApprovalStateVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "cloud_approval_state.json"))
	if err != nil {
		t.Fatalf("读取 content-state 向量失败: %v", err)
	}
	var file struct {
		Cases []struct {
			Name         string          `json:"name"`
			ContentState json.RawMessage `json:"content_state"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("content-state 向量为空")
	}
	canonical := func(b []byte) string {
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(v)
		return string(out)
	}
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var st CloudApprovalState
			if err := json.Unmarshal(c.ContentState, &st); err != nil {
				t.Fatalf("解码失败: %v", err)
			}
			if st.Title == "" || st.State == "" {
				t.Fatalf("title / state 是必填键: %+v", st)
			}
			out, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if canonical(out) != canonical(c.ContentState) {
				t.Fatalf("CloudApprovalState 与向量 JSON 不一致:\n got %s\nwant %s", canonical(out), canonical(c.ContentState))
			}
		})
	}
}

// TestCloudApprovalStateRoutingHints 路由线索从 payload 透传进 content-state；
// payload 没带时全部整键不发。
func TestCloudApprovalStateRoutingHints(t *testing.T) {
	env := Envelope{
		Type: EventApprovalRequired, ApprovalID: "apv_x", Agent: AgentClaude,
		DaemonID: "dd_1", SessionID: "sess_1", ExpiresAt: 1000, TS: 100,
		Payload: Payload{SessionTitle: "支付重构", TmuxSession: "0", TmuxWindow: "1",
			TmuxPane: "%3", Cwd: "/repo/sub", ProjectDir: "/repo", HostIP: "10.0.0.5", User: "eric"},
	}
	st := CloudApprovalStateFor(env, 200)
	if st.TmuxSession != "0" || st.TmuxWindow != "1" || st.TmuxPane != "%3" ||
		st.Cwd != "/repo/sub" || st.ProjectDir != "/repo" ||
		st.HostIP != "10.0.0.5" || st.User != "eric" || st.Agent != "claude" {
		t.Fatalf("路由线索透传不符: %+v", st)
	}
	bare, err := json.Marshal(CloudApprovalStateFor(Envelope{
		Type: EventApprovalRequired, ApprovalID: "apv_x", SessionID: "sess_1",
		Payload: Payload{SessionTitle: "t"}}, 200))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tmuxSession", "tmuxWindow", "tmuxPane", "cwd", "projectDir", "hostIP", "user", "agent"} {
		if strings.Contains(string(bare), `"`+key+`"`) {
			t.Fatalf("空路由线索 %q 不应落键: %s", key, bare)
		}
	}
}

// TestAgentBatch2Valid v0.6.0 第二批 agent 通过白名单,锁屏抬头带品牌名,
// 且向量目录里出现了对应取值(同 commit 内必须同时扩 Valid,否则
// TestVectorsRoundTrip 会先红)。
func TestAgentBatch2Valid(t *testing.T) {
	cases := []struct {
		a       Agent
		display string
	}{
		{AgentGemini, "Gemini"},
		{AgentCopilot, "Copilot"},
		{AgentCursor, "Cursor"},
		{AgentGrok, "Grok"},
		{AgentQwen, "Qwen"},
	}
	for _, c := range cases {
		env := Envelope{
			V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
			Type: EventApprovalRequired, Agent: c.a,
			DaemonID: "dd_x", SessionID: "s", ApprovalID: "apv_x", TS: 1,
		}
		if err := env.Validate(); err != nil {
			t.Fatalf("%s 应通过校验,实际 %v", c.a, err)
		}
		if got := DisplayAgent(c.a); got != c.display {
			t.Fatalf("锁屏抬头应显示 %s,实际 %q", c.display, got)
		}
	}
	if Agent("antigravity").Valid() {
		t.Fatal("未登记的 agent 不应通过白名单")
	}
}

// TestNotifyOnlyPredicate 只通知不阻塞的判定:显式 notify_only 或
// AskUserQuestion 历史链路任一命中;普通审批卡不命中。
func TestNotifyOnlyPredicate(t *testing.T) {
	base := func(p Payload) Envelope {
		return Envelope{
			V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
			Type: EventApprovalRequired, Agent: AgentGemini,
			DaemonID: "dd_x", SessionID: "s", ApprovalID: "apv_x", TS: 1,
			Payload: p,
		}
	}
	if !base(Payload{NotifyOnly: true, ToolName: "run_shell_command"}).IsNotifyOnly() {
		t.Fatal("显式 notify_only 应命中")
	}
	if !base(Payload{ToolName: "AskUserQuestion"}).IsNotifyOnly() {
		t.Fatal("AskUserQuestion 历史链路应命中")
	}
	if base(Payload{ToolName: "Bash"}).IsNotifyOnly() {
		t.Fatal("普通审批卡不应命中")
	}
}

// TestNotifyOnlyOmitEmpty 旧 daemon 不发新字段:notify_only 缺省时整键不落。
func TestNotifyOnlyOmitEmpty(t *testing.T) {
	data, err := json.Marshal(Payload{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "notify_only") {
		t.Fatalf("缺省 notify_only 不应落键: %s", data)
	}
	data, err = json.Marshal(Payload{Title: "t", NotifyOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"notify_only":true`) {
		t.Fatalf("notify_only=true 应落键: %s", data)
	}
}

// TestApprovalBodyNotifyOnly 等待卡正文:TerminalOnly agent 的通知说清
// 「在终端等你」;AskUserQuestion 的历史文案逐字不变。
func TestApprovalBodyNotifyOnly(t *testing.T) {
	env := Envelope{
		V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
		Type: EventApprovalRequired, Agent: AgentGemini,
		DaemonID: "dd_x", SessionID: "s", ApprovalID: "apv_x", TS: 1,
		Payload: Payload{NotifyOnly: true, ToolName: "run_shell_command", CommandSummary: "npm test"},
	}
	if got, want := ApprovalBody(env), "Gemini is waiting for approval: npm test"; got != want {
		t.Fatalf("等待卡正文不符:\n got %q\nwant %q", got, want)
	}
	env.Payload.CommandSummary = ""
	if got, want := ApprovalBody(env), "Gemini is waiting for approval"; got != want {
		t.Fatalf("无摘要正文不符:\n got %q\nwant %q", got, want)
	}
	q := Envelope{
		V: Version, Kind: KindEvent, MsgID: "m", EventID: "e",
		Type: EventApprovalRequired, Agent: AgentClaude,
		DaemonID: "dd_x", SessionID: "s", ApprovalID: "apv_x", TS: 1,
		Payload: Payload{ToolName: "AskUserQuestion", CommandSummary: "Which DB?"},
	}
	if got, want := ApprovalBody(q), "Claude has a question — Which DB?"; got != want {
		t.Fatalf("问题卡历史文案必须逐字不变:\n got %q\nwant %q", got, want)
	}
}

// TestCloudApprovalStateNotifyOnly notify_only 透传进 content-state,缺省不落键。
func TestCloudApprovalStateNotifyOnly(t *testing.T) {
	env := Envelope{
		Type: EventApprovalRequired, ApprovalID: "apv_x", Agent: AgentGrok,
		DaemonID: "dd_1", SessionID: "sess_1", ExpiresAt: 1000, TS: 100,
		Payload: Payload{SessionTitle: "t", NotifyOnly: true},
	}
	data, err := json.Marshal(CloudApprovalStateFor(env, 200))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"notifyOnly":true`) {
		t.Fatalf("notifyOnly 应透传进 content-state: %s", data)
	}
	bare, err := json.Marshal(CloudApprovalStateFor(Envelope{
		Type: EventApprovalRequired, ApprovalID: "apv_x", SessionID: "sess_1",
		Payload: Payload{SessionTitle: "t"}}, 200))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "notifyOnly") {
		t.Fatalf("缺省 notifyOnly 不应落键: %s", bare)
	}
}
