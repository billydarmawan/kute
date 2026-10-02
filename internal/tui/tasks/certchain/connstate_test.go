package certchain

import (
	"testing"

	"github.com/kute-dev/kute/internal/kube"
)

// TestConnStateMsgMarksTheScreenOffline sends the message type the root
// actually fans out (kube.ConnStateMsg, not kube.ConnState): the screen
// used to switch on the wrong type, so the renew verb stayed offered and
// the header kept saying "connected" through an outage.
func TestConnStateMsgMarksTheScreenOffline(t *testing.T) {
	m := demoModel(t, "default", "web-tls")
	upd, _ := m.Update(kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	got := upd.(*Model)
	if !got.conn.Offline() {
		t.Fatalf("conn not offline after ConnStateMsg{reconnecting}; conn = %+v", got.conn)
	}
}
