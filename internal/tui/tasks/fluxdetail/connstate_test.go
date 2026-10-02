package fluxdetail

import (
	"testing"

	"github.com/kute-dev/kute/internal/kube"
)

// TestConnStateMsgMarksTheScreenOffline sends the message type the root
// actually fans out (kube.ConnStateMsg, not kube.ConnState): the screen
// used to switch on the wrong type, so its r/s gates, keybar and header
// badge treated every outage as connected.
func TestConnStateMsgMarksTheScreenOffline(t *testing.T) {
	m := demoModel(t, "nebula-workers")
	upd, _ := m.Update(kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	got := upd.(*Model)
	if !got.offline() {
		t.Fatalf("offline() = false after ConnStateMsg{reconnecting}; conn = %+v", got.conn)
	}
}
