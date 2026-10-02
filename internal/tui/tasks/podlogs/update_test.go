package podlogs

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

func press(model *Model, key string) {
	_, _ = model.Update(tea.KeyPressMsg{Text: key})
}

func TestStreamLifecycleMessages(t *testing.T) {
	t.Parallel()

	model := testModel()
	_, _ = model.Update(streamStartedMsg{state: StreamLoading})
	if model.stream != StreamLoading {
		t.Fatalf("stream = %s", model.stream)
	}
	_, _ = model.Update(logLineMsg{entry: LogEntry{Container: "app", Message: "ready"}})
	if model.stream != StreamStreaming || len(model.buffer.Entries) != 1 {
		t.Fatalf("model = %+v", model)
	}
	_, _ = model.Update(streamEmptyMsg{})
	if model.stream != StreamEmpty || !strings.Contains(model.feedback, "No logs found") {
		t.Fatalf("empty state = %s %q", model.stream, model.feedback)
	}
	_, _ = model.Update(streamErrorMsg{err: errors.New("pods/log is forbidden")})
	if model.stream != StreamError || !strings.Contains(model.feedback, "Permission denied") {
		t.Fatalf("error state = %s %q", model.stream, model.feedback)
	}
	if model.taskState() != tui.TaskStatePermissionDenied {
		t.Fatalf("taskState = %s, want permission-denied", model.taskState())
	}
}

func TestExitClosesStream(t *testing.T) {
	t.Parallel()

	model := testModel()
	_, cmd := model.Update(tea.KeyPressMsg{Text: "ctrl+q"})
	if cmd == nil || model.stream != StreamClosed {
		t.Fatalf("cmd = %v stream = %s", cmd, model.stream)
	}
}

func TestEscapeReturnsBackMessageAndClosesStream(t *testing.T) {
	t.Parallel()

	model := testModel()
	_, cmd := model.Update(tea.KeyPressMsg{Text: "esc"})
	if cmd == nil {
		t.Fatalf("esc command is nil")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("esc command did not return BackMsg")
	}
	if model.stream != StreamClosed {
		t.Fatalf("stream = %s, want closed", model.stream)
	}
}

func TestVerticalNavigationKeys(t *testing.T) {
	t.Parallel()

	model := testModel()
	seedLines(&model, 20)
	model.view.AutoScroll = false
	model.view.VerticalOffset = 0

	press(&model, "j")
	if model.view.VerticalOffset != 1 {
		t.Fatalf("j offset = %d", model.view.VerticalOffset)
	}
	press(&model, "k")
	if model.view.VerticalOffset != 0 {
		t.Fatalf("k offset = %d", model.view.VerticalOffset)
	}
	press(&model, "pgdown")
	if model.view.VerticalOffset == 0 {
		t.Fatalf("pgdown did not move")
	}
	press(&model, "G")
	if model.view.VerticalOffset != model.maxVerticalOffset() {
		t.Fatalf("G offset = %d want %d", model.view.VerticalOffset, model.maxVerticalOffset())
	}
	press(&model, "home")
	if model.view.VerticalOffset != 0 {
		t.Fatalf("home offset = %d", model.view.VerticalOffset)
	}
}

func TestVerticalNavigationMovesByWrappedPhysicalRow(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.SetSize(40, 12)
	model.stream = StreamStreaming
	model.buffer.Append(LogEntry{Message: strings.Repeat("wrapped message ", 12)})
	model.buffer.Append(LogEntry{Message: "next logical entry"})
	model.view.AutoScroll = false
	model.view.VerticalOffset = 0

	rows := model.visualRows(model.filteredEntries(), model.view.Width)
	if len(rows) < 3 || rows[0].EntryIndex != 0 || rows[1].EntryIndex != 0 {
		t.Fatalf("unexpected wrapped layout: %+v", rows)
	}
	press(&model, "j")
	if model.view.VerticalOffset != 1 {
		t.Fatalf("j offset = %d, want the next physical row", model.view.VerticalOffset)
	}
	if got := model.visibleWindow(model.view.Width)[0].EntryIndex; got != 0 {
		t.Fatalf("j skipped the first entry's continuation row; top entry = %d", got)
	}
}

func TestHalfPageNavigationKeys(t *testing.T) {
	t.Parallel()

	model := testModel()
	seedLines(&model, 30)
	model.view.AutoScroll = false
	model.view.VerticalOffset = 0
	press(&model, "ctrl+d")
	if model.view.VerticalOffset != max(1, model.entryViewportHeight()/2) {
		t.Fatalf("ctrl+d offset = %d", model.view.VerticalOffset)
	}
	press(&model, "ctrl+u")
	if model.view.VerticalOffset != 0 {
		t.Fatalf("ctrl+u offset = %d", model.view.VerticalOffset)
	}
}

func TestHorizontalNavigationWhenWrapOff(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.view.Wrap = false
	press(&model, "l")
	press(&model, "right")
	if model.view.HorizontalOffset != 2 {
		t.Fatalf("horizontal offset = %d", model.view.HorizontalOffset)
	}
	press(&model, "h")
	if model.view.HorizontalOffset != 1 {
		t.Fatalf("horizontal offset after h = %d", model.view.HorizontalOffset)
	}
	model.view.Wrap = true
	press(&model, "l")
	if model.view.HorizontalOffset != 0 {
		t.Fatalf("wrap horizontal offset = %d", model.view.HorizontalOffset)
	}
}

func TestFollowToggleAndAutoScroll(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.SetSize(80, 8)
	seedLines(&model, 20)
	bottom := model.maxVerticalOffset()
	if model.view.VerticalOffset != bottom {
		t.Fatalf("autoscroll offset = %d want %d", model.view.VerticalOffset, bottom)
	}

	press(&model, "space")
	if model.view.AutoScroll {
		t.Fatalf("space did not pause following")
	}
	model.view.VerticalOffset = 2
	model.appendEntry(LogEntry{Container: "app", Message: "new"})
	if model.view.VerticalOffset != 2 {
		t.Fatalf("paused follow moved to %d", model.view.VerticalOffset)
	}

	press(&model, "space")
	if !model.view.AutoScroll || model.view.VerticalOffset != model.maxVerticalOffset() {
		t.Fatalf("space did not resume following: %+v", model.view)
	}
}

func TestWrapAndTimestampTogglesAreDisplayOnly(t *testing.T) {
	t.Parallel()

	model := testModel()
	_, cmd := model.Update(tea.KeyPressMsg{Text: "W"})
	if cmd != nil || model.view.Wrap {
		t.Fatalf("wrap toggle = %v cmd = %v", model.view.Wrap, cmd)
	}
	_, cmd = model.Update(tea.KeyPressMsg{Text: "t"})
	if cmd != nil || !model.view.Timestamps {
		t.Fatalf("timestamps toggle = %v cmd = %v", model.view.Timestamps, cmd)
	}
}

func TestWrapToggleReflowsAndKeepsFollowingAtBottom(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.SetSize(40, 10)
	for range 4 {
		model.appendEntry(LogEntry{Message: strings.Repeat("long log message ", 8)})
	}
	wrappedBottom := model.maxVerticalOffset()
	if wrappedBottom == 0 || model.view.VerticalOffset != wrappedBottom {
		t.Fatalf("wrapped following offset = %d, bottom = %d", model.view.VerticalOffset, wrappedBottom)
	}

	press(&model, "W")
	if model.view.Wrap || model.view.VerticalOffset != model.maxVerticalOffset() {
		t.Fatalf("nowrap view did not remain at bottom: %+v", model.view)
	}
	press(&model, "W")
	if !model.view.Wrap || model.view.VerticalOffset != model.maxVerticalOffset() {
		t.Fatalf("rewrapped view did not remain at bottom: %+v", model.view)
	}
}

func TestFollowingStaysAtBottomWhenFirstDroppedNoticeAppears(t *testing.T) {
	t.Parallel()

	model := New(Config{
		Pod:        SelectedPod{Name: "api", Containers: []string{"app"}},
		MaxEntries: 2,
	})
	model.SetSize(40, 8)
	for range 2 {
		model.appendEntry(LogEntry{Message: strings.Repeat("long log message ", 10)})
	}
	before := model.view.VerticalOffset
	model.appendEntry(LogEntry{Message: strings.Repeat("long log message ", 10)})
	if model.buffer.DroppedCount != 1 {
		t.Fatalf("dropped count = %d, want 1", model.buffer.DroppedCount)
	}
	if model.view.VerticalOffset != model.maxVerticalOffset() {
		t.Fatalf("following offset = %d, bottom = %d after dropped notice appeared (before=%d rows=%d viewport=%d)",
			model.view.VerticalOffset, model.maxVerticalOffset(), before,
			len(model.visualRows(model.filteredEntries(), model.view.Width)), model.entryViewportHeight())
	}
}

func TestTabCyclesContainerAndRestartsStream(t *testing.T) {
	t.Parallel()

	model := testModel() // app, sidecar
	_, cmd := model.Update(tea.KeyPressMsg{Text: "tab"})
	if cmd == nil {
		t.Fatalf("tab did not restart the stream")
	}
	if container, _ := model.activeContainer(); container != "sidecar" {
		t.Fatalf("container after tab = %q", container)
	}
}

func TestSinceKeyCyclesWindowAndRestartsStream(t *testing.T) {
	t.Parallel()

	model := testModel()
	start := model.sinceLabel()
	_, cmd := model.Update(tea.KeyPressMsg{Text: "s"})
	if cmd == nil {
		t.Fatalf("s did not restart the stream")
	}
	if model.sinceLabel() == start {
		t.Fatalf("since label did not change from %q", start)
	}
}

// entryVisible reports whether any physical row belonging to index idx (into
// model.buffer.Entries) falls within the current viewport window.
func entryVisible(model Model, idx int) bool {
	for _, row := range model.visibleWindow(model.view.Width) {
		if row.EntryIndex == idx {
			return true
		}
	}
	return false
}

func TestJumpSeverityMovesToNextWarningOrError(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.SetSize(80, 12)
	model.stream = StreamStreaming
	for range 5 {
		model.buffer.Append(LogEntry{Message: "info"})
	}
	model.buffer.Append(LogEntry{Message: "warn one", Severity: SeverityWarn}) // index 5
	for range 5 {
		model.buffer.Append(LogEntry{Message: "info"})
	}
	model.buffer.Append(LogEntry{Message: "error one", Severity: SeverityErr}) // index 11
	model.view.VerticalOffset = 0

	press(&model, "w")
	if !entryVisible(model, 5) {
		t.Fatalf("warning entry not visible after w: offset=%d height=%d", model.view.VerticalOffset, model.entryViewportHeight())
	}
	model.view.VerticalOffset = 0
	press(&model, "e")
	if !entryVisible(model, 11) {
		t.Fatalf("error entry not visible after e: offset=%d height=%d", model.view.VerticalOffset, model.entryViewportHeight())
	}
}

func TestFilterOpensNarrowsAndClears(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.stream = StreamStreaming
	model.buffer.Append(LogEntry{Message: "starting up"})
	model.buffer.Append(LogEntry{Message: "request failed"})

	_, cmd := model.Update(tea.KeyPressMsg{Text: "/"})
	if cmd != nil || !model.filterActive {
		t.Fatalf("filterActive = %v cmd = %v", model.filterActive, cmd)
	}
	if !model.CapturingInput() {
		t.Fatalf("CapturingInput = false while filtering")
	}

	_, _ = model.Update(tea.KeyPressMsg{Text: "failed"})
	if len(model.filteredEntries()) != 1 {
		t.Fatalf("filtered = %+v, want 1 match", model.filteredEntries())
	}
	// docs/design system-wide interactions: "items never silently
	// disappear" — the strip must say a line was hidden by the filter, not
	// just show a bare matched count.
	if view := model.Render(); !strings.Contains(view, "hidden by filter") {
		t.Fatalf("expected the 'hidden by filter' notice:\n%s", view)
	}

	press(&model, "esc")
	if model.filterActive || model.filterInput.Value() != "" {
		t.Fatalf("esc did not clear filter: active=%v query=%q", model.filterActive, model.filterInput.Value())
	}
}

func TestFilterRemainsVisibleAfterTerminalStreamError(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.stream = StreamError
	model.feedback = "API disconnected"
	model.buffer.Append(LogEntry{Message: "KUTE-E2E marker"})

	press(&model, "/")
	press(&model, "KUTE-E2E")
	strips := model.Strips(model.width)
	if len(strips) != 1 || !strings.Contains(strips[0], "KUTE-E2E") {
		t.Fatalf("terminal stream error hid the active filter input: strips=%q", strips)
	}
}

func TestFilterCtrlJKAltHLMoveWithoutTyping(t *testing.T) {
	t.Parallel()

	model := testModel()
	seedLines(&model, 20)
	model.view.AutoScroll = false
	model.view.Wrap = false
	model.view.VerticalOffset = 0
	model.view.HorizontalOffset = 0

	_, _ = model.Update(tea.KeyPressMsg{Text: "/"})
	if !model.filterActive {
		t.Fatalf("expected / to activate the filter")
	}

	_, _ = model.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	if model.view.VerticalOffset != 1 {
		t.Fatalf("VerticalOffset = %d, want 1 after ctrl+j", model.view.VerticalOffset)
	}
	_, _ = model.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	if model.view.VerticalOffset != 0 {
		t.Fatalf("VerticalOffset = %d, want 0 after ctrl+k", model.view.VerticalOffset)
	}
	_, _ = model.Update(tea.KeyPressMsg{Code: 'l', Mod: tea.ModAlt})
	if model.view.HorizontalOffset != 1 {
		t.Fatalf("HorizontalOffset = %d, want 1 after alt+l", model.view.HorizontalOffset)
	}
	_, _ = model.Update(tea.KeyPressMsg{Code: 'h', Mod: tea.ModAlt})
	if model.view.HorizontalOffset != 0 {
		t.Fatalf("HorizontalOffset = %d, want 0 after alt+h", model.view.HorizontalOffset)
	}
	if model.filterInput.Value() != "" {
		t.Fatalf("filterQuery = %q, want empty (ctrl+j/k/alt+h/l must move, not type)", model.filterInput.Value())
	}
}

func TestCopyVisibleViewSetsClipboard(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.buffer.Append(LogEntry{Message: "hello"})
	_, cmd := model.Update(tea.KeyPressMsg{Text: "Y"})
	if cmd == nil {
		t.Fatalf("ctrl+y did not return a command")
	}
}

func TestVisibleViewCopyIncludesWrappedEntryOnlyOnce(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.SetSize(40, 9)
	message := strings.Repeat("one logical message ", 8)
	model.buffer.Append(LogEntry{Timestamp: "10:00:00", Severity: SeverityWarn, Message: message})
	model.view.VerticalOffset = 1 // begin on a continuation row

	got := model.visibleViewText()
	if strings.Count(got, strings.TrimSpace(message)) != 1 {
		t.Fatalf("wrapped logical entry was not copied exactly once: %q", got)
	}
}

func TestContainerWaitingMsgParksInWaitingState(t *testing.T) {
	t.Parallel()

	model := testModel()
	_, cmd := model.Update(containerWaitingMsg{reason: "ContainerCreating"})
	if cmd != nil {
		t.Fatalf("cmd = %v, want nil (no self-rescheduling — the next check comes from kube.ResourceChangedMsg)", cmd)
	}
	if model.stream != StreamWaitingForContainer || model.waitingReason != "ContainerCreating" {
		t.Fatalf("model = %+v", model)
	}
	if model.taskState() != tui.TaskStateLoading {
		t.Fatalf("taskState = %s, want loading", model.taskState())
	}

	// A stale streamID (from a superseded beginStream, e.g. the user
	// switched containers again while this check was in flight) is ignored.
	model.streamID = 9
	_, cmd = model.Update(containerWaitingMsg{streamID: 1, reason: "ImagePullBackOff"})
	if cmd != nil || model.waitingReason != "ContainerCreating" {
		t.Fatalf("stale containerWaitingMsg was applied: waitingReason=%q cmd=%v", model.waitingReason, cmd)
	}
}

func TestContainerReadyMsgConnects(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.streamer = &fakeStreamer{connects: map[string][]string{"app": {""}}}
	model.stream = StreamWaitingForContainer
	model.waitingReason = "ContainerCreating"

	_, cmd := model.Update(containerReadyMsg{})
	if cmd == nil {
		t.Fatalf("containerReadyMsg did not return a connect cmd")
	}
	if model.streamCancel == nil {
		t.Fatalf("connect did not spin up the stream goroutine")
	}
	model.cancelStream()
}

// TestCrashLoopingStreamShowsInlineNote: logs opened on a crash-looping
// container's previous run say so in the status line, so its last run's
// output isn't mistaken for a live process — and a fresh beginStream (tab to
// another container) drops the note.
func TestCrashLoopingStreamShowsInlineNote(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.streamer = &fakeStreamer{connects: map[string][]string{"app": {"boom"}}}
	_, cmd := model.Update(containerReadyMsg{crashLoop: "CrashLoopBackOff"})
	if cmd == nil {
		t.Fatalf("containerReadyMsg did not return a connect cmd")
	}
	model.cancelStream()
	model.stream = StreamStreaming
	model.appendEntry(LogEntry{Container: "app", Message: "panic: boom"})
	model.SetSize(120, 24)

	view := ansi.Strip(model.Body(120, 20))
	if !strings.Contains(view, "app is crash-looping (CrashLoopBackOff) · showing its last run") {
		t.Fatalf("status line lacks the crash-looping note:\n%s", view)
	}
	if !strings.Contains(view, "panic: boom") {
		t.Fatalf("crash output not rendered:\n%s", view)
	}

	model.lister = &fakeRestartLister{podName: "api", container: "sidecar", state: "Running"}
	_ = model.beginStream(StreamLoading)
	if model.crashLoopReason != "" {
		t.Fatalf("crashLoopReason = %q after beginStream, want cleared", model.crashLoopReason)
	}
}

func TestResourceChangedMsgRechecksOnlyWhileWaitingOnPods(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.lister = &fakeRestartLister{podName: "api", container: "app", state: "Running"}
	model.stream = StreamWaitingForContainer

	_, cmd := model.Update(kube.ResourceChangedMsg{Kind: kube.KindPod})
	if cmd == nil {
		t.Fatalf("expected a recheck cmd while StreamWaitingForContainer")
	}
	if msg := cmd(); msg != (containerReadyMsg{streamID: model.streamID}) {
		t.Fatalf("recheck result = %#v, want containerReadyMsg", msg)
	}

	// Not waiting: a Pod change event is a no-op.
	model.stream = StreamStreaming
	_, cmd = model.Update(kube.ResourceChangedMsg{Kind: kube.KindPod})
	if cmd != nil {
		t.Fatalf("cmd = %v, want nil while not waiting on a container", cmd)
	}

	// Waiting, but a change to an unrelated kind is also a no-op.
	model.stream = StreamWaitingForContainer
	_, cmd = model.Update(kube.ResourceChangedMsg{Kind: kube.KindEvent})
	if cmd != nil {
		t.Fatalf("cmd = %v, want nil for a non-Pod kind", cmd)
	}
}

func TestRateTickUpdatesLastRateAndDropsStaleGeneration(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.rateGen = 5
	model.linesSinceTick = 3

	_, cmd := model.Update(rateTickMsg{gen: 5})
	if model.lastRate != 3 || model.linesSinceTick != 0 || cmd == nil {
		t.Fatalf("model = %+v cmd nil = %v", model, cmd == nil)
	}

	model.linesSinceTick = 9
	_, cmd = model.Update(rateTickMsg{gen: 1})
	if model.lastRate != 3 || cmd != nil {
		t.Fatalf("stale tick applied: lastRate=%d cmd=%v", model.lastRate, cmd)
	}
}

// TestTypedForbiddenErrorReachesPermissionDeniedState pins that taskState
// classifies the *real* error rather than a reconstruction of its text.
//
// taskState used to run kube.IsPermissionError over fmt.Errorf("%s",
// m.lastError) — an error rebuilt from the message string, which had already
// thrown the *apierrors.StatusError away. IsPermissionError's good
// apierrors.IsForbidden path could therefore never fire there; only its
// documented substring fallback could. This error is deliberately typed
// Forbidden while carrying a message with neither "forbidden" nor
// "permission" in it, so the substring path cannot rescue it.
func TestTypedForbiddenErrorReachesPermissionDeniedState(t *testing.T) {
	t.Parallel()
	forbidden := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    403,
		Reason:  metav1.StatusReasonForbidden,
		Message: "denied by admission webhook",
	}}
	if strings.Contains(strings.ToLower(forbidden.Error()), "forbidden") {
		t.Fatal("test fixture defeats its own purpose: message must not contain \"forbidden\"")
	}

	model := testModel()
	_, _ = model.Update(streamErrorMsg{err: forbidden})

	if model.stream != StreamError {
		t.Fatalf("stream = %s, want error", model.stream)
	}
	if got := model.taskState(); got != tui.TaskStatePermissionDenied {
		t.Fatalf("taskState = %s, want permission-denied", got)
	}
}

func TestLogBatchAppendsEveryEntryAndKeepsReading(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.streamCh = make(chan tea.Msg, streamChanBuffer)
	_, cmd := model.Update(logBatchMsg{entries: []LogEntry{
		{Container: "app", Message: "one"},
		{Container: "app", Message: "two"},
		{Container: "app", Message: "three"},
	}})
	if len(model.buffer.Entries) != 3 {
		t.Fatalf("entries = %+v, want all three appended", model.buffer.Entries)
	}
	if model.stream != StreamStreaming {
		t.Fatalf("stream = %s, want streaming", model.stream)
	}
	if model.linesSinceTick != 3 {
		t.Fatalf("linesSinceTick = %d, want 3 — the rate counter must still count lines, not batches", model.linesSinceTick)
	}
	if cmd == nil {
		t.Fatalf("cmd is nil, want the batch to keep reading the stream")
	}
}

// A batch's tail is the message that ended the drain, and it has to be
// handled after the lines in front of it — an error arriving behind a burst
// must still put the screen in its error state.
func TestLogBatchHandlesItsTail(t *testing.T) {
	t.Parallel()

	model := testModel()
	_, _ = model.Update(logBatchMsg{
		entries: []LogEntry{{Container: "app", Message: "last gasp"}},
		tail:    streamErrorMsg{err: errors.New("stream broke")},
	})
	if len(model.buffer.Entries) != 1 {
		t.Fatalf("entries = %+v, want the line ahead of the tail kept", model.buffer.Entries)
	}
	if model.stream != StreamError || !strings.Contains(model.feedback, "stream broke") {
		t.Fatalf("state = %s %q, want the tail's error surfaced", model.stream, model.feedback)
	}
}

func TestLogBatchFromSupersededStreamIsDropped(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.streamCh = make(chan tea.Msg, streamChanBuffer)
	model.streamID = 7
	_, cmd := model.Update(logBatchMsg{streamID: 6, entries: []LogEntry{{Container: "app", Message: "stale"}}})
	if len(model.buffer.Entries) != 0 {
		t.Fatalf("entries = %+v, want a superseded stream's lines dropped", model.buffer.Entries)
	}
	if cmd == nil {
		t.Fatalf("cmd is nil, want the superseded channel still drained to close")
	}
}
