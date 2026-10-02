package browse

import (
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// drainCmd runs cmd the way the Bubble Tea runtime would — every leaf of a
// tea.BatchMsg on its own goroutine — and returns every message produced
// within wait. A tea.Tick blocks for its full duration, so wait must exceed
// one second for cronJobTickMsg to show up. Leaves still blocked at the
// deadline are abandoned (their goroutines finish on their own).
func drainCmd(cmd tea.Cmd, wait time.Duration) []tea.Msg {
	var (
		mu   sync.Mutex
		out  []tea.Msg
		wg   sync.WaitGroup
		done = make(chan struct{})
	)
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, inner := range batch {
					run(inner)
				}
				return
			}
			if msg != nil {
				mu.Lock()
				out = append(out, msg)
				mu.Unlock()
			}
		}()
	}
	run(cmd)
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]tea.Msg(nil), out...)
}

func clockTicks(msgs []tea.Msg) []cronJobTickMsg {
	var ticks []cronJobTickMsg
	for _, msg := range msgs {
		if tick, ok := msg.(cronJobTickMsg); ok {
			ticks = append(ticks, tick)
		}
	}
	return ticks
}

// TestCronJobClockSurvivesReloadsWithoutDuplicating pins review 06 H1: the
// §36a/§37a one-second clock used to carry reloadEpoch, so the first watch
// event for the kind or an aux kind (Pod/Job — constant on a real cluster),
// or a Reload() on pop, dropped the in-flight tick and NEXT/DURATION and
// m.now froze. The chain must survive every reload path, and no reload may
// start a second chain alongside it: exactly one tick in flight at a time.
func TestCronJobClockSurvivesReloadsWithoutDuplicating(t *testing.T) {
	for _, kind := range []kube.ResourceKind{kube.KindCronJob, kube.KindJob} {
		t.Run(string(kind), func(t *testing.T) {
			cj := cronJobObj("default", "webhook-retry")
			cj.Spec.Schedule = "*/2 * * * *"
			lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
				kube.KindCronJob: {cj},
				kube.KindJob:     {jobObj("default", "webhook-retry-1")},
			}}
			sess := cronJobBrowseSession()
			sess.Location.Kind = kind
			m := New(Config{Session: sess, Lister: lister})
			m.SetSize(120, 36)

			initMsgs := drainCmd(m.Init(), 1500*time.Millisecond)
			ticks := clockTicks(initMsgs)
			if len(ticks) != 1 {
				t.Fatalf("Init started %d clock chains, want 1", len(ticks))
			}
			inFlight := ticks[0]
			for _, msg := range initMsgs {
				if loaded, ok := msg.(rowsLoadedMsg); ok {
					updated, _ := m.Update(loaded)
					m = *updated.(*Model)
				}
			}

			// Every reload path: a watch event for the kind, one for an aux
			// kind, Reload() on pop, and the debounced reload + its rows.
			var reloadCmds []tea.Cmd
			for _, changed := range []kube.ResourceKind{kind, kube.KindPod, kube.KindJob} {
				updated, cmd := m.Update(kube.ResourceChangedMsg{Kind: changed})
				m = *updated.(*Model)
				reloadCmds = append(reloadCmds, cmd)
			}
			reloadCmds = append(reloadCmds, m.Reload())
			updated, cmd := m.Update(reloadDueMsg{epoch: m.reloadEpoch})
			m = *updated.(*Model)
			if cmd == nil {
				t.Fatal("expected the debounced reload to issue a load")
			}
			for _, msg := range drainCmd(cmd, 500*time.Millisecond) {
				if loaded, ok := msg.(rowsLoadedMsg); ok {
					updated, cmd := m.Update(loaded)
					m = *updated.(*Model)
					reloadCmds = append(reloadCmds, cmd)
				}
			}
			if extra := clockTicks(drainCmd(tea.Batch(reloadCmds...), 1500*time.Millisecond)); len(extra) != 0 {
				t.Fatalf("reloads started %d extra clock chains, want 0", len(extra))
			}

			// The tick that was in flight across all of that still lands.
			stale := m.now
			updated, cmd = m.Update(inFlight)
			m = *updated.(*Model)
			if cmd == nil {
				t.Fatal("clock chain died on a reload: the in-flight tick did not reschedule")
			}
			if !m.now.After(stale) {
				t.Fatalf("tick did not advance m.now (still %v)", m.now)
			}
			next := clockTicks(drainCmd(cmd, 1500*time.Millisecond))
			if len(next) != 1 || next[0] != inFlight {
				t.Fatalf("tick rescheduled %v, want exactly one %v", next, inFlight)
			}
		})
	}
}

// TestCronJobClockKindSwitchStillRetiresOldChain pins the other half of the
// one-chain rule: a kind switch away and back (resetAndLoad) starts a new
// chain, so the old chain's tick must die on arrival rather than run
// alongside it.
func TestCronJobClockKindSwitchStillRetiresOldChain(t *testing.T) {
	cj := cronJobObj("default", "webhook-retry")
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindCronJob: {cj}}}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)
	old := cronJobTickMsg{epoch: m.clockEpoch}

	_ = m.switchKind(kube.KindJob)

	if _, cmd := m.Update(old); cmd != nil {
		t.Fatal("a superseded chain's tick rescheduled itself alongside the new chain")
	}
}
