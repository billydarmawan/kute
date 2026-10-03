package cronjobschedule

import (
	"context"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/tui"
)

// step drains a tea.Cmd chain synchronously — mirrors certchain/renew_test.go's
// own step, duplicated per the repo's package-local-seam convention. Never
// called on Init()'s own batch: tickCmd/spinner.Tick are both real
// tea.Tick-backed commands that would recurse (or block for a real second)
// forever under synchronous draining, the same hazard
// browse_jobs_test.go documents for §36a's own clock tick — tests drive
// m.load()() directly instead.
func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				m = step(t, m, c())
			}
		}
		return m
	}
	updated, cmd := m.Update(msg)
	next := *updated.(*Model)
	if cmd != nil {
		return step(t, next, cmd())
	}
	return next
}

func newSession(namespace string) *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "test-cluster", Namespace: namespace}}
}

// newFakeCronJob seeds a fake cluster with one CronJob and returns the
// cluster (implements resources.RawLister, kube.Mutator, and
// CapabilityReader all at once, so most tests need no bespoke stub).
func newFakeCronJob(namespace, name, schedule, timezone string) *fake.Cluster {
	c := fake.New(namespace, "test-cluster")
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			UID: "cj-uid-1", ResourceVersion: "1", Generation: 1,
		},
		Spec: batchv1.CronJobSpec{Schedule: schedule},
	}
	if timezone != "" {
		tz := timezone
		cj.Spec.TimeZone = &tz
	}
	c.Seed(kube.KindCronJob, cj)
	return c
}

func newModel(t *testing.T, c *fake.Cluster, namespace, name string) Model {
	t.Helper()
	m := New(Config{Session: newSession(namespace), Lister: c, Mutator: c, Capabilities: c, Namespace: namespace, Name: name})
	m.SetSize(120, 36)
	return step(t, m, m.load()())
}

func TestLoadSeedsAcceptedAndBuffersFromTheCronJob(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "*/5 * * * *", "")
	m := newModel(t, c, "default", "nightly")

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready (feedback=%q)", m.state, m.feedback)
	}
	if m.scheduleInput.Value() != "*/5 * * * *" {
		t.Errorf("scheduleInput = %q, want the CronJob's own schedule", m.scheduleInput.Value())
	}
	if m.accepted.resourceVersion != "1" {
		t.Errorf("accepted.resourceVersion = %q, want 1", m.accepted.resourceVersion)
	}
	if m.accepted.hasTimezone {
		t.Errorf("accepted.hasTimezone = true, want false for a CronJob with no spec.timeZone")
	}
	if m.parseErr != nil {
		t.Errorf("parseErr = %v, want nil for a valid schedule", m.parseErr)
	}
	if len(m.nextRuns) != 0 {
		// No explicit timezone -> ErrTimeZoneUnset, not a populated list.
		t.Errorf("nextRuns = %v, want empty without an explicit time zone", m.nextRuns)
	}
}

func TestLoadSeedsExistingTimeZoneIntoBuffer(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "America/New_York")
	m := newModel(t, c, "default", "nightly")

	if !m.accepted.hasTimezone || m.accepted.timezone != "America/New_York" {
		t.Fatalf("accepted timezone = (%v, %q), want (true, America/New_York)", m.accepted.hasTimezone, m.accepted.timezone)
	}
	if m.tzInput.Value() != "America/New_York" {
		t.Errorf("tzInput = %q, want America/New_York", m.tzInput.Value())
	}
	if len(m.nextRuns) != 5 {
		t.Errorf("nextRuns has %d entries, want 5 for a valid schedule+timezone", len(m.nextRuns))
	}
	// §3.8 point 4: an already-populated spec.timeZone is direct evidence
	// the field works on this object, regardless of the (Unknown, here)
	// capability read.
	if !m.tzEditable() {
		t.Errorf("expected tzEditable() true for an already-populated time zone even under Unknown capability")
	}
}

func TestLoadNotFoundReportsError(t *testing.T) {
	t.Parallel()
	c := fake.New("default", "test-cluster")
	m := newModel(t, c, "default", "ghost")
	if m.state != tui.TaskStateError {
		t.Fatalf("state = %v, want Error", m.state)
	}
}

type erroringLister struct{ err error }

func (l erroringLister) ListRaw(context.Context, kube.ResourceKind, string) ([]runtime.Object, error) {
	return nil, l.err
}

func TestLoadPermissionErrorReportsPermissionDenied(t *testing.T) {
	t.Parallel()
	lister := erroringLister{err: fmt.Errorf("cronjobs is forbidden: user cannot list")}
	m := New(Config{Session: newSession("default"), Lister: lister, Namespace: "default", Name: "nightly"})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())
	if m.state != tui.TaskStatePermissionDenied {
		t.Fatalf("state = %v, want PermissionDenied", m.state)
	}
}

// unsyncedThenSyncedLister mirrors browse_cronjob_list_test.go's
// perKindSyncedCronJobLister: reports KindCronJob unsynced once, then
// synced, so applyLoaded's §4.4-style retry path is exercised end to end.
type unsyncedThenSyncedLister struct {
	objs   []runtime.Object
	polls  int
	synced bool
}

func (l *unsyncedThenSyncedLister) ListRaw(context.Context, kube.ResourceKind, string) ([]runtime.Object, error) {
	l.polls++
	return l.objs, nil
}
func (l *unsyncedThenSyncedLister) KindSynced(kube.ResourceKind, string) bool { return l.synced }

func TestUnsyncedCronJobCacheStaysLoadingThenRetries(t *testing.T) {
	t.Parallel()
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "1"}, Spec: batchv1.CronJobSpec{Schedule: "0 2 * * *"}}
	lister := &unsyncedThenSyncedLister{objs: []runtime.Object{cj}}
	m := New(Config{Session: newSession("default"), Lister: lister, Namespace: "default", Name: "nightly"})
	m.SetSize(120, 36)
	// A single, un-drained step: applyLoaded's own retry cmd
	// (tui.ScheduleCacheSyncRetry) is a real tea.Tick, so this deliberately
	// does *not* use step() here — draining it would really sleep 250ms per
	// bounce until lister.synced flips, the same hazard every other
	// synchronous-cmd-draining test in this repo avoids around a recurring
	// tick.
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)
	if m.state != tui.TaskStateLoading {
		t.Fatalf("state = %v, want Loading while the cache reports unsynced", m.state)
	}
	firstPolls := lister.polls
	lister.synced = true
	m = step(t, m, tui.CacheSyncRetryMsg{Gen: m.reloadEpoch})
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready once the cache reports synced", m.state)
	}
	if lister.polls <= firstPolls {
		t.Fatalf("expected the retry to issue another ListRaw call")
	}
}

// typeText delivers each rune of s as its own KeyPressMsg — the same
// character-at-a-time path a real terminal drives updateKey's default
// buffer-forwarding branch with.
func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m = step(t, m, tea.KeyPressMsg{Text: string(r), Code: r})
	}
	return m
}

func clearInput(t *testing.T, m Model) Model {
	t.Helper()
	for range 40 {
		m = step(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	return m
}

func TestCommitApplySuccessRemainsReadyAndOffersUndo(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	m := newModel(t, c, "default", "nightly")

	m = clearInput(t, m)
	m = typeText(t, m, "*/15 * * * *")
	if m.parseErr != nil {
		t.Fatalf("parseErr = %v after typing a valid schedule", m.parseErr)
	}
	if !m.hasPendingChange() {
		t.Fatalf("expected a pending change after editing the schedule")
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready — a successful apply must remain on screen", m.state)
	}
	if m.pendingCommit != nil {
		t.Fatalf("expected pendingCommit cleared after the result lands")
	}
	if m.message == "" {
		t.Fatalf("expected an inline success message")
	}
	if m.previous == nil {
		t.Fatalf("expected a one-step undo target after a successful apply")
	}
	if m.accepted.schedule != "*/15 * * * *" {
		t.Fatalf("accepted.schedule = %q, want the newly applied schedule", m.accepted.schedule)
	}

	got, err := c.ListRaw(t.Context(), kube.KindCronJob, "default")
	if err != nil || len(got) != 1 {
		t.Fatalf("ListRaw after apply: %v, %v", got, err)
	}
	if cj := got[0].(*batchv1.CronJob); cj.Spec.Schedule != "*/15 * * * *" {
		t.Fatalf("server schedule = %q, want */15 * * * *", cj.Spec.Schedule)
	}
}

func TestCommitApplyInvalidScheduleNeverCallsMutator(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	m := newModel(t, c, "default", "nightly")

	m = clearInput(t, m)
	m = typeText(t, m, "not a schedule")
	if m.parseErr == nil {
		t.Fatalf("expected parseErr set for an invalid schedule")
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.pendingCommit != nil {
		t.Fatalf("expected enter to no-op while the schedule is invalid")
	}
	got, _ := c.ListRaw(t.Context(), kube.KindCronJob, "default")
	if cj := got[0].(*batchv1.CronJob); cj.Spec.Schedule != "0 2 * * *" {
		t.Fatalf("server schedule changed to %q, want it untouched", cj.Spec.Schedule)
	}
}

func TestCommitApplyConflictPreservesBufferAndExplainsWhy(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	m := newModel(t, c, "default", "nightly")

	// Simulate a concurrent external edit: bump the server's resourceVersion
	// out from under the still-loaded m.accepted.resourceVersion.
	if err := c.SetCronJobSuspend(t.Context(), "default", "nightly", true, m.accepted.resourceVersion, m.accepted.generation, time.Now()); err != nil {
		t.Fatalf("SetCronJobSuspend (simulating a concurrent edit): %v", err)
	}

	m = clearInput(t, m)
	m = typeText(t, m, "*/10 * * * *")
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})

	if !m.conflict {
		t.Fatalf("expected m.conflict true after a stale-resourceVersion apply")
	}
	if m.lastError == "" {
		t.Fatalf("expected an explicit conflict message")
	}
	if m.scheduleInput.Value() != "*/10 * * * *" {
		t.Fatalf("scheduleInput = %q, want the typed value preserved after a failed apply", m.scheduleInput.Value())
	}
}

func TestCommitUndoRestoresScheduleAndTimeZone(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "America/New_York")
	m := newModel(t, c, "default", "nightly")

	m = clearInput(t, m)
	m = typeText(t, m, "*/30 * * * *")
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.previous == nil {
		t.Fatalf("expected an undo target after the first apply")
	}

	m = step(t, m, ctrlKey('z'))
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready after undo", m.state)
	}
	if m.accepted.schedule != "0 2 * * *" {
		t.Fatalf("accepted.schedule = %q after undo, want the original 0 2 * * *", m.accepted.schedule)
	}
	if !m.accepted.hasTimezone || m.accepted.timezone != "America/New_York" {
		t.Fatalf("accepted timezone after undo = (%v, %q), want (true, America/New_York)", m.accepted.hasTimezone, m.accepted.timezone)
	}
	if m.previous != nil {
		t.Fatalf("expected the undo target cleared after undo itself applies (one step, not a stack)")
	}

	got, _ := c.ListRaw(t.Context(), kube.KindCronJob, "default")
	cj := got[0].(*batchv1.CronJob)
	if cj.Spec.Schedule != "0 2 * * *" || cj.Spec.TimeZone == nil || *cj.Spec.TimeZone != "America/New_York" {
		t.Fatalf("server state after undo = schedule %q tz %v, want reverted", cj.Spec.Schedule, cj.Spec.TimeZone)
	}
}

func TestPasteIntoScheduleRecomputesDerivedState(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	m := newModel(t, c, "default", "nightly")

	m = clearInput(t, m)
	m = step(t, m, tea.PasteMsg{Content: "*/5 * * * *"})
	if m.scheduleInput.Value() != "*/5 * * * *" {
		t.Fatalf("scheduleInput = %q, want the pasted schedule", m.scheduleInput.Value())
	}
	if m.parseErr != nil {
		t.Fatalf("parseErr = %v, want nil for a valid pasted schedule", m.parseErr)
	}
	if m.fields.Minute != "*/5" {
		t.Fatalf("expected recomputeAll to decode fields off the pasted buffer immediately, got %+v", m.fields)
	}
}

func TestPasteIntoTimeZoneWhenFocused(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	m := newModel(t, c, "default", "nightly")
	// Capability Unknown + no existing timezone -> not editable yet; give
	// it Supported so tab can focus it.
	c.SetTimeZoneCapability(kube.TimeZoneCapabilitySupported)
	m.capabilities = c

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Text: "tab"})
	if !m.tzFocused {
		t.Fatalf("expected tab to focus the timezone buffer once editable")
	}
	m = step(t, m, tea.PasteMsg{Content: "UTC"})
	if m.tzInput.Value() != "UTC" {
		t.Fatalf("tzInput = %q, want UTC pasted in", m.tzInput.Value())
	}
	if m.tzErr != nil {
		t.Fatalf("tzErr = %v, want nil for a valid pasted time zone", m.tzErr)
	}
}

// ctrlKey is the KeyPressMsg a terminal delivers for ctrl+<r>: no Text,
// so it can never reach a buffer as typed input.
func ctrlKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}

// isClipboardWrite reports whether msg (or any message of a batch) is
// tea.SetClipboard's write request — the only observable trace of a copy.
func isClipboardWrite(msg tea.Msg) bool {
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil && isClipboardWrite(c()) {
				return true
			}
		}
		return false
	}
	return fmt.Sprintf("%T", msg) == fmt.Sprintf("%T", tea.SetClipboard("")())
}

// typeTextNoCopy is typeText that also fails on any keystroke answering
// with a clipboard write instead of inserting its rune.
func typeTextNoCopy(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		updated, cmd := m.Update(tea.KeyPressMsg{Text: string(r), Code: r})
		m = *updated.(*Model)
		if cmd != nil {
			msg := cmd()
			if isClipboardWrite(msg) {
				t.Fatalf("typing %q into %q copied to the clipboard instead of inserting it", r, s)
			}
			m = step(t, m, msg)
		}
	}
	return m
}

// appliedOnce applies one schedule edit so a one-step undo is armed — the
// state in which the old bare 'u' fired an undo mid-word.
func appliedOnce(t *testing.T, c *fake.Cluster) Model {
	t.Helper()
	m := newModel(t, c, "default", "nightly")
	m = clearInput(t, m)
	m = typeText(t, m, "*/15 * * * *")
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.previous == nil {
		t.Fatalf("expected an undo target after the first apply")
	}
	return m
}

// TestScheduleBufferTypesMacrosAndNames is the regression for copy/undo
// being bare 'y'/'u': with undo armed, "@daily" lost its y to a clipboard
// copy, and "@hourly" or "0 2 * * sun" lost their u to an undo.
func TestScheduleBufferTypesMacrosAndNames(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"@daily", "@hourly", "@yearly", "@monthly", "0 2 * * sun", "0 0 1 jul *"} {
		c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
		m := appliedOnce(t, c)

		m = clearInput(t, m)
		m = typeTextNoCopy(t, m, want)
		if got := m.scheduleInput.Value(); got != want {
			t.Errorf("scheduleInput = %q, want %q typed literally", got, want)
		}
		if m.parseErr != nil {
			t.Errorf("%q: parseErr = %v, want a valid schedule", want, m.parseErr)
		}
		if m.previous == nil || m.accepted.schedule != "*/15 * * * *" {
			t.Errorf("%q: typing fired an undo (previous=%v, accepted=%q)", want, m.previous, m.accepted.schedule)
		}
	}
}

// TestTimeZoneBufferTypesUAndY pins the same fix for the timezone buffer:
// "UTC" and "Asia/Yerevan" type literally with undo armed.
func TestTimeZoneBufferTypesUAndY(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"UTC", "Europe/Dublin", "Asia/Yerevan"} {
		c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
		c.SetTimeZoneCapability(kube.TimeZoneCapabilitySupported)
		m := appliedOnce(t, c)
		m.capabilities = c

		m = step(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Text: "tab"})
		if !m.tzFocused {
			t.Fatalf("expected tab to focus the timezone buffer")
		}
		m = typeTextNoCopy(t, m, want)
		if got := m.tzInput.Value(); got != want {
			t.Errorf("tzInput = %q, want %q typed literally", got, want)
		}
		if m.previous == nil || m.accepted.schedule != "*/15 * * * *" {
			t.Errorf("%q: typing fired an undo", want)
		}
	}
}

// TestCtrlTCopiesWillRunCommand pins copy's new chord, from either buffer.
func TestCtrlTCopiesWillRunCommand(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	c.SetTimeZoneCapability(kube.TimeZoneCapabilitySupported)
	m := newModel(t, c, "default", "nightly")
	m.capabilities = c
	m = clearInput(t, m)
	m = typeText(t, m, "@daily")

	for _, focusTZ := range []bool{false, true} {
		if focusTZ {
			m = step(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Text: "tab"})
		}
		before := m.scheduleInput.Value() + "|" + m.tzInput.Value()
		updated, cmd := m.Update(ctrlKey('t'))
		m = *updated.(*Model)
		if cmd == nil {
			t.Fatalf("ctrl+t (tz focused=%v) returned no command, want a clipboard write", focusTZ)
		}
		msg := cmd()
		if !isClipboardWrite(msg) || fmt.Sprint(msg) != m.willRunCommand() {
			t.Fatalf("ctrl+t (tz focused=%v) = %T %v, want clipboard write of %q", focusTZ, msg, msg, m.willRunCommand())
		}
		if after := m.scheduleInput.Value() + "|" + m.tzInput.Value(); after != before {
			t.Fatalf("ctrl+t edited a buffer: %q -> %q", before, after)
		}
	}
}

// TestCapitalYInTimeZoneIsText is the regression for the full-YAML-edit
// escape being a bare 'Y': it was matched before the focused buffer, so
// "Asia/Yerevan" lost its Y and suspended into kubectl edit instead.
func TestCapitalYInTimeZoneIsText(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	c.SetTimeZoneCapability(kube.TimeZoneCapabilitySupported)
	m := newModel(t, c, "default", "nightly")
	m.capabilities = c

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Text: "tab"})
	m = typeText(t, m, "Asia/Yerevan")
	if m.tzInput.Value() != "Asia/Yerevan" {
		t.Fatalf("tzInput = %q, want Asia/Yerevan typed literally", m.tzInput.Value())
	}
}

func TestTimeZoneCapabilityGatesTabFocus(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	c.SetTimeZoneCapability(kube.TimeZoneCapabilityUnsupported)
	m := newModel(t, c, "default", "nightly")

	if m.tzEditable() {
		t.Fatalf("expected tzEditable() false: Unsupported capability, no existing time zone")
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Text: "tab"})
	if m.tzFocused {
		t.Fatalf("expected tab to no-op while the time zone field isn't editable")
	}
}

// countingLister wraps a fake.Cluster's ListRaw to count calls — task 10's
// "clock tick issues zero ListRaw calls" is otherwise unobservable through
// fake.Cluster alone.
type countingLister struct {
	inner *fake.Cluster
	calls int
}

func (l *countingLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	l.calls++
	return l.inner.ListRaw(ctx, kind, namespace)
}

func TestTickUpdatesClockWithoutIssuingAnyListRawCall(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "America/New_York")
	lister := &countingLister{inner: c}
	m := New(Config{Session: newSession("default"), Lister: lister, Mutator: c, Capabilities: c, Namespace: "default", Name: "nightly"})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	before := m.now
	callsBefore := lister.calls
	m2, cmd := m.Update(tickMsg(before.Add(5 * time.Second)))
	m = *m2.(*Model)
	if !m.now.Equal(before.Add(5 * time.Second)) {
		t.Fatalf("now = %v, want the tick's own timestamp", m.now)
	}
	if cmd == nil {
		t.Fatalf("expected the tick to reschedule itself")
	}
	if lister.calls != callsBefore {
		t.Fatalf("tick issued %d ListRaw calls, want 0", lister.calls-callsBefore)
	}
}

// A cache snapshot that predates our own apply (the reload an unrelated
// CronJob event triggers in that window) must not revert m.accepted to the
// pre-apply schedule/resourceVersion.
func TestStaleCacheSnapshotAfterApplyKeepsAcceptedFromResponse(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	m := newModel(t, c, "default", "nightly")
	preRV := m.accepted.resourceVersion

	m = clearInput(t, m)
	m = typeText(t, m, "*/15 * * * *")
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	applied := m.accepted
	if applied.resourceVersion == preRV {
		t.Fatalf("apply did not move the resourceVersion (still %q)", preRV)
	}

	m = step(t, m, loadedMsg{epoch: m.reloadEpoch, found: true, cj: &cronJobSnapshot{
		schedule: "0 2 * * *", resourceVersion: preRV, generation: 1,
	}})
	if m.accepted != applied {
		t.Fatalf("accepted = %+v after a stale snapshot, want the apply response's %+v", m.accepted, applied)
	}
	if m.hasPendingChange() {
		t.Fatalf("a stale snapshot produced a phantom pending change")
	}
}

// An external edit moves the baseline under the typed buffer: the pending-
// change gate and WHAT CHANGES must be re-derived against the new one.
func TestExternalEditRecomputesAgainstNewBaseline(t *testing.T) {
	t.Parallel()
	c := newFakeCronJob("default", "nightly", "0 2 * * *", "")
	m := newModel(t, c, "default", "nightly")
	m = clearInput(t, m)
	m = typeText(t, m, "*/15 * * * *")
	if !m.comparisonDirty {
		t.Fatalf("expected a dirty comparison after typing a change")
	}
	m.comparisonDirty = false // as if the comparison had already landed
	m.comparison = &scheduleComparison{}

	m = step(t, m, loadedMsg{epoch: m.reloadEpoch, found: true, cj: &cronJobSnapshot{
		schedule: "*/15 * * * *", resourceVersion: "9", generation: 2,
	}})
	if m.accepted.schedule != "*/15 * * * *" {
		t.Fatalf("accepted.schedule = %q, want the external edit", m.accepted.schedule)
	}
	if m.comparison != nil {
		t.Fatalf("WHAT CHANGES still shows a diff against the old baseline")
	}
	if m.comparisonDirty || m.hasPendingChange() {
		t.Fatalf("buffer now matches the new baseline; want no pending change (dirty=%v)", m.comparisonDirty)
	}
}
