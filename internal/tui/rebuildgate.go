package tui

import "errors"

// One cluster rebuild at a time. A 7a context switch (switchContextCmd) and
// a 4c/10b rebuild (internal/app's rebuildCluster, landing as
// ReplaceRootMsg) both replace what the Session's cluster is connected to
// from inside a tea.Cmd. Run together they fight over the same cluster: a
// switch still in flight when a 4c rebuild commits restarts the cluster that
// rebuild just stopped, and its SwitchContextMsg then relabels the session
// for a context the new cluster isn't on; a switch started while a rebuild
// runs is switching a cluster the rebuild is about to stop. So whichever
// starts second is refused before it touches anything, and the gate
// reopens only when the one that holds it has landed.
//
// Every method runs on the Update loop: each rebuild begins in the
// constructor of its tea.Cmd and ends in the message handler its Cmd's
// result reaches — never inside the Cmd itself.

// ErrClusterRebuildInFlight is why a context switch or reconnect was refused:
// another one hasn't finished yet.
var ErrClusterRebuildInFlight = errors.New("another context switch or reconnect is still running")

// rebuildGate is the Session's one-rebuild-at-a-time latch. held is the
// running rebuild's token (0 when none is running).
type rebuildGate struct {
	held, last uint64
}

// BeginClusterRebuild claims the gate for a new context switch or rebuild,
// returning the token its completion message must carry back to
// EndClusterRebuild. ok is false while another one is running: the caller
// must then not start, nor write anything to the Session.
func (s *Session) BeginClusterRebuild() (token uint64, ok bool) {
	if s.rebuild.held != 0 {
		return 0, false
	}
	s.rebuild.last++
	s.rebuild.held = s.rebuild.last
	return s.rebuild.held, true
}

// EndClusterRebuild releases the gate if token holds it. A zero or stale
// token (a refused attempt's own failure message) leaves it alone.
func (s *Session) EndClusterRebuild(token uint64) {
	if token != 0 && s.rebuild.held == token {
		s.rebuild.held = 0
	}
}

// ClusterRebuildInFlight reports whether a context switch or rebuild holds
// the gate.
func (s *Session) ClusterRebuildInFlight() bool { return s.rebuild.held != 0 }

// ClusterRebuildFailedMsg is a 4c/10b rebuild that didn't produce a cluster
// (or was refused): the current one stays. Token releases the gate it was
// started under; the root does that on the way past, whoever the message is
// routed to, so a setup screen that has since been replaced can't leave the
// gate held for good. tasks/setup's RetryFailedMsg is this type.
type ClusterRebuildFailedMsg struct {
	Token uint64
	Err   error
}

// settleRebuild releases the rebuild gate for a message that ends a context
// switch or rebuild — called for owned messages too, before they're routed
// to (or dropped for) their task.
func (m *Model) settleRebuild(msg any) {
	if m.session == nil {
		return
	}
	switch msg := msg.(type) {
	case SwitchContextMsg:
		m.session.EndClusterRebuild(msg.token)
	case ReplaceRootMsg:
		m.session.EndClusterRebuild(msg.Token)
	case ClusterRebuildFailedMsg:
		m.session.EndClusterRebuild(msg.Token)
	}
}
