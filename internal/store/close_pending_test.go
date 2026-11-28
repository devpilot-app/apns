package store

import (
	"testing"

	"github.com/devpilot-app/apns/pkg/wire"
)

// TestClosePendingFlipsOnlyPendingRows 钉 daemon 自发 close 的落账语义：
// 只翻 pending、跨 daemon 拒写、非法 status 拒写、已终态不被覆盖。
func TestClosePendingFlipsOnlyPendingRows(t *testing.T) {
	s := newStore(t)
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	seedPairing(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", daemonID)
	event := approvalEvent("ev_1", "apv_1")
	event.DaemonID = daemonID
	if _, _, err := s.AppendInbox(daemonID, event, testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPending(Pending{
		ApprovalID: "apv_1", DaemonID: daemonID, Event: event, Status: "pending",
		CreatedAt: testNow, ExpiresAt: testNow + 300,
	}); err != nil {
		t.Fatal(err)
	}

	flipped, err := s.ClosePending("apv_1", daemonID, "cancelled", testNow+1)
	if err != nil || !flipped {
		t.Fatalf("close pending flipped=%v err=%v", flipped, err)
	}
	p, err := s.GetPending("apv_1")
	if err != nil || p.Status != "cancelled" || p.DecidedAt != testNow+1 {
		t.Fatalf("after close p=%+v err=%v", p, err)
	}
	if p.Decision != "" || p.DeliverUntil != 0 {
		t.Fatalf("close must not look like a phone decision: %+v", p)
	}

	// 已终态：再翻无效（first-wins，expired 不覆盖 cancelled）。
	flipped, err = s.ClosePending("apv_1", daemonID, "expired", testNow+2)
	if err != nil || flipped {
		t.Fatalf("re-close flipped=%v err=%v", flipped, err)
	}
	if p, _ = s.GetPending("apv_1"); p.Status != "cancelled" {
		t.Fatalf("status overwritten: %+v", p)
	}

	// 跨 daemon 不泄露存在性。
	if err := s.PutPending(Pending{
		ApprovalID: "apv_2", DaemonID: daemonID, Event: event, Status: "pending",
		CreatedAt: testNow, ExpiresAt: testNow + 300,
	}); err != nil {
		t.Fatal(err)
	}
	if flipped, _ := s.ClosePending("apv_2", "dd_other", "cancelled", testNow); flipped {
		t.Fatal("cross-daemon close must not flip")
	}

	// 未知 id 静默。
	if flipped, err := s.ClosePending("apv_missing", daemonID, "cancelled", testNow); flipped || err != nil {
		t.Fatalf("unknown id flipped=%v err=%v", flipped, err)
	}

	// 非法 status 拒写。
	if flipped, err := s.ClosePending("apv_2", daemonID, "pending", testNow); err == nil || flipped {
		t.Fatalf("invalid status accepted flipped=%v err=%v", flipped, err)
	}
	if p, _ := s.GetPending("apv_2"); p.Status != "pending" {
		t.Fatalf("invalid status wrote through: %+v", p)
	}

	// 翻转后不再出现在 pending 集合（resync /v1/pending 的数据源）。
	items, err := s.PendingForDaemon(daemonID, testNow+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ApprovalID != "apv_2" {
		t.Fatalf("PendingForDaemon after close = %+v", items)
	}

	// 手机迟到决策落到已 cancelled 的行：AckExpired，不作为新决策转发。
	if status, _, _ := s.ApplyDecision("apv_1", daemonID, "approved", "", testNow+3); status != wire.AckExpired {
		t.Fatalf("late decision status=%v", status)
	}
}
