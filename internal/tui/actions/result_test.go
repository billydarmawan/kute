package actions

import (
	"errors"
	"strings"
	"testing"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

func cordonAction() tui.TaskAction {
	return tui.TaskAction{
		ID:    "cordon-node-a",
		Label: "Cordon node-a",
		Scope: tui.TaskScope{ResourceKind: string(kube.KindNode), ResourceName: "node-a", Verb: "cordon", IsMutating: true},
	}
}

// TestFailedResultIsAFailedResultLine pins review 10 H1's render seam: a
// failure becomes a red Result line naming the action and the cause, with
// the confirm label's trailing "?" dropped (10 L3).
func TestFailedResultIsAFailedResultLine(t *testing.T) {
	c := New(&fakeMutator{err: errors.New("forbidden")})
	beginAt(&c, TierInline, deleteAction())
	if !c.Result().Empty() {
		t.Fatal("no result while the confirm is showing")
	}
	c.HandleResult(c.Confirm()().(ResultMsg))
	r := c.Result()
	if !r.Failed || !strings.Contains(r.Text, "forbidden") || strings.Contains(r.Text, "?") {
		t.Fatalf("result = %+v, want a failed line with the cause and no question mark", r)
	}
	c.DismissResult()
	if !c.Result().Empty() {
		t.Fatal("DismissResult should clear the line")
	}
}

func TestSuccessResultIsAGreenLine(t *testing.T) {
	c := New(&fakeMutator{})
	beginAt(&c, TierInline, deleteAction())
	c.HandleResult(c.Confirm()().(ResultMsg))
	if r := c.Result(); r.Failed || r.Empty() {
		t.Fatalf("result = %+v, want a success line", r)
	}
}

// TestBeginRefusalIsAResultLine: Begin's own refusals (offline, missing
// target metadata) used to set a message nothing rendered.
func TestBeginRefusalIsAResultLine(t *testing.T) {
	c := New(&fakeMutator{})
	c.SetOffline(true)
	beginAt(&c, TierInline, deleteAction())
	if r := c.Result(); !r.Failed || !strings.Contains(r.Text, "offline") {
		t.Fatalf("result = %+v, want the offline refusal", r)
	}
}

// TestStaleResultKeepsNewerConfirm pins review 10 M3: a cordon (TierNone,
// in flight) whose result lands after the user opened a delete confirm
// must not wipe that confirm — before the fix the y/N vanished and the
// next y fell through to the YAML key.
func TestStaleResultKeepsNewerConfirm(t *testing.T) {
	mut := &fakeMutator{err: errors.New("nodes is forbidden")}
	c := New(mut)
	cordon := beginAt(&c, TierNone, cordonAction())
	if cordon == nil {
		t.Fatal("TierNone should execute immediately")
	}
	beginAt(&c, TierInline, deleteAction())
	if !c.Active() {
		t.Fatal("expected the delete confirm to be showing")
	}
	c.HandleResult(cordon().(ResultMsg))
	if !c.Active() || c.Pending() == nil || c.Pending().Scope.Verb != "delete" {
		t.Fatalf("a stale result cleared the newer confirm: active=%v pending=%v", c.Active(), c.Pending())
	}
	// Its outcome is not lost: it shows once the confirm closes.
	c.Cancel()
	if r := c.Result(); !r.Failed || !strings.Contains(r.Text, "forbidden") {
		t.Fatalf("result = %+v, want the stale cordon's failure", r)
	}
}

// TestStaleBulkResultKeepsNewerConfirm is the bulk counterpart.
func TestStaleBulkResultKeepsNewerConfirm(t *testing.T) {
	c := New(&fakeMutator{})
	beginAt(&c, TierInline, deleteAction())
	c.HandleBulkResult(BulkResultMsg{ActionID: "old", Label: "Suspend 2", Seq: c.seq + 100})
	if !c.Active() {
		t.Fatal("a stale bulk result cleared the newer confirm")
	}
}
