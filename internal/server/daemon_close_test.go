package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devpilot-app/apns/pkg/wire"
)

// TestDaemonCloseFlipsPendingAndResyncOmitsIt 钉 2026-11-28 真机滞卡案的 relay
// 侧修复：daemon 自发 approval_closed 必须翻转 pending 状态——否则 /v1/pending、
// resync pending[] 把已收场审批当 open，sweeper 到期还会补发 expired。
func TestDaemonCloseFlipsPendingAndResyncOmitsIt(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hd",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})
	app := dial(t, env.ts, "/ws/app", env.deviceCr)
	hello(t, app, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_ha",
		Role: wire.RoleApp, DeviceID: env.deviceID, TS: time.Now().Unix(),
	})

	approvalID := "apv_" + strings.Repeat("c", 26)
	now := time.Now().Unix()
	send(t, daemon, wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_req", EventID: "ev_req",
		ApprovalID: approvalID, Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: testDaemonID, SessionID: "sess_close", TS: now,
		Payload: wire.Payload{ToolName: "Bash", CommandSummary: "sed -i '' …"},
	})
	if ack, ok := recv(t, daemon).(*wire.Ack); !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("required ack = %+v", ack)
	}

	// 终端里答完：daemon 上报 cancelled 收场。
	send(t, daemon, wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_cls", EventID: "close-" + approvalID,
		ApprovalID: approvalID, Type: wire.EventApprovalClosed, Agent: wire.AgentClaude,
		DaemonID: testDaemonID, SessionID: "sess_close", TS: now + 10,
		Payload: wire.Payload{ToolName: "Bash", CommandSummary: "sed -i '' …", StatusText: wire.CloseReasonCancelled},
	})
	if ack, ok := recv(t, daemon).(*wire.Ack); !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("closed ack = %+v", ack)
	}
	// App 在线时实时收到 required + closed 两条直播事件，先排空。
	live1, ok := recv(t, app).(*wire.Envelope)
	if !ok || live1.Type != wire.EventApprovalRequired {
		t.Fatalf("live event 1 = %+v", live1)
	}
	live2, ok := recv(t, app).(*wire.Envelope)
	if !ok || live2.Type != wire.EventApprovalClosed || live2.Payload.StatusText != wire.CloseReasonCancelled {
		t.Fatalf("live event 2 = %+v", live2)
	}

	p, err := env.store.GetPending(approvalID)
	if err != nil || p.Status != "cancelled" {
		t.Fatalf("pending row after daemon close: status=%q err=%v", p.Status, err)
	}

	// resync：pending[] 不再包含该审批；events 含 required + closed 两条。
	send(t, app, wire.Resync{V: wire.Version, Kind: wire.KindResync, MsgID: "m_rs"})
	rs, ok := recv(t, app).(*wire.ResyncResult)
	if !ok {
		t.Fatal("missing resync_result")
	}
	if len(rs.Pending) != 0 {
		t.Fatalf("closed approval still in resync pending: %+v", rs.Pending)
	}
	if len(rs.Events) != 2 || rs.Events[1].Type != wire.EventApprovalClosed ||
		rs.Events[1].EventID != "close-"+approvalID {
		t.Fatalf("resync events = %+v", rs.Events)
	}

	// 手机迟到点击同一条审批：不作为新决策（App 端收到 expired ack）。
	resp, body := env.post(t, "/v1/decisions",
		map[string]any{"approval_id": approvalID, "decision": "allow"}, env.deviceCr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("late decision HTTP %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != string(wire.AckExpired) {
		t.Fatalf("late decision status = %q (want expired)", out.Status)
	}
}

// TestLateLATokenForClosedApprovalTriggersRetroactiveEnd 钉迟到 token 补发：
// push-to-start 起卡在先、App 上报 per-approval token 在后；token 落库时审批
// 已被 daemon 收场 → 必须对该设备补发 approval_closed end，卡才收得掉。
func TestLateLATokenForClosedApprovalTriggersRetroactiveEnd(t *testing.T) {
	n := newOriginNotifier()
	env := newTestEnvWithNotifier(t, n)
	env.registerAndPair(t)

	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hd",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})

	approvalID := "apv_" + strings.Repeat("l", 26)
	now := time.Now().Unix()
	send(t, daemon, wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_req", EventID: "ev_req",
		ApprovalID: approvalID, Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: testDaemonID, SessionID: "sess_late", TS: now,
		Payload: wire.Payload{ToolName: "Read", CommandSummary: "/tmp/a.jpg"},
	})
	if ack, ok := recv(t, daemon).(*wire.Ack); !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("required ack = %+v", ack)
	}
	send(t, daemon, wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_cls", EventID: "close-" + approvalID,
		ApprovalID: approvalID, Type: wire.EventApprovalClosed, Agent: wire.AgentClaude,
		DaemonID: testDaemonID, SessionID: "sess_late", TS: now + 5,
		Payload: wire.Payload{ToolName: "Read", CommandSummary: "/tmp/a.jpg", StatusText: wire.CloseReasonCancelled},
	})
	if ack, ok := recv(t, daemon).(*wire.Ack); !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("closed ack = %+v", ack)
	}
	// 第一腿：当时的 close 扇出（App 未上报 token，真机上是 zero-push，桩里
	// 表现为一次 Notify 调用）。排空它。
	<-n.ch

	// App 迟到上报一审批一卡 token（锁屏 push-to-start 唤醒后 adopt 路径）。
	resp, body := env.post(t, "/v1/la-tokens", map[string]any{
		"daemon_id": testDaemonID, "session_id": "sess_late",
		"approval_id": approvalID, "push_token": "late-token-00", "event": "start",
	}, env.deviceCr)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("la-tokens HTTP %d: %s", resp.StatusCode, body)
	}

	// 补发腿：该设备收到 approval_closed（end）。
	select {
	case got := <-n.ch:
		if got.env.Type != wire.EventApprovalClosed || got.env.ApprovalID != approvalID ||
			got.env.Payload.StatusText != wire.CloseReasonCancelled {
			t.Fatalf("retroactive close = %+v", got.env)
		}
		if got.device.ID != env.deviceID {
			t.Fatalf("retroactive close went to %s, want %s", got.device.ID, env.deviceID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no retroactive end push after late la-token")
	}

	// token 已随 end 落库（补发路径经 deliverer 剪除在真桩里不生效，这里只
	// 验证 store 里 token 存在，证明落库未被打断）。
	if _, ok, err := env.store.LATokenForApproval(env.deviceID, approvalID); err != nil || !ok {
		t.Fatalf("token not stored: ok=%v err=%v", ok, err)
	}
}
