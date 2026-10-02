package podlogs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// fakeRestartLister answers ListRaw with a single pod carrying one
// container's live restart count and (state, reason) — letting tests
// simulate an actual container restart happening mid-stream, or a
// container that hasn't started yet (or fails to). state defaults to
// "Running" when unset, so existing restart-count-only tests that never set
// it are unaffected. listErr, when set, makes ListRaw itself fail —
// simulating a lister/lookup failure distinct from "the pod says Waiting".
type fakeRestartLister struct {
	podName   string
	container string
	restarts  int32
	state     string // "", "Running", "Waiting", "Terminated"
	reason    string
	listErr   error
	deleted   bool
	// lastTerminated gives the container a previous run (a terminated
	// LastTerminationState with a container ID), the shape a
	// CrashLoopBackOff container has between restarts.
	lastTerminated bool
}

func (f *fakeRestartLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	if kind != kube.KindPod {
		return nil, nil
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.deleted {
		return nil, nil
	}
	status := corev1.ContainerStatus{Name: f.container, RestartCount: f.restarts}
	switch f.state {
	case "Waiting":
		status.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: f.reason}}
	case "Terminated":
		status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: f.reason}}
	default: // "" and "Running" both mean running — the common case for existing restart-count tests
		status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	}
	if f.lastTerminated {
		status.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error", ContainerID: "containerd://previous",
		}}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: f.podName},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: f.container}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{status}},
	}
	return []runtime.Object{pod}, nil
}

// fakeStreamer replays connects[N] on the Nth call for a given container —
// letting tests simulate a container ending its stream and podlogs
// reconnecting to a "new" instance of it.
type fakeStreamer struct {
	mu        sync.Mutex
	connects  map[string][]string // container -> lines per successive connect
	callCount map[string]int
	err       error
	requests  []kube.LogStreamRequest
}

func (s *fakeStreamer) StreamPodLogs(_ context.Context, req kube.LogStreamRequest) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	if s.callCount == nil {
		s.callCount = map[string]int{}
	}
	lines := s.connects[req.Container]
	idx := s.callCount[req.Container]
	s.callCount[req.Container]++
	if idx >= len(lines) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return io.NopCloser(strings.NewReader(lines[idx])), nil
}

func TestStreamContainerEmitsLinesThenReconnectsWithBoundary(t *testing.T) {
	t.Parallel()

	streamer := &fakeStreamer{connects: map[string][]string{
		"app": {"first\nsecond\n", "", "third\n"},
	}}
	model := testModel()
	model.streamer = streamer
	model.pod.Restarts = 3

	ctx, cancel := context.WithCancel(t.Context())
	var entries []LogEntry
	err := model.streamContainer(ctx, "app", nil, func(e LogEntry) bool {
		entries = append(entries, e)
		if len(entries) == 4 { // first, second, boundary, third
			cancel()
			return false
		}
		return true
	})
	if err != nil {
		t.Fatalf("streamContainer error = %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Message != "first" || entries[1].Message != "second" {
		t.Fatalf("first connect entries = %+v", entries[:2])
	}
	if !entries[2].Boundary || !strings.Contains(entries[2].Message, "restart 3") {
		t.Fatalf("boundary entry = %+v", entries[2])
	}
	if entries[3].Message != "third" {
		t.Fatalf("post-reconnect entry = %+v", entries[3])
	}

	if len(streamer.requests) != 3 {
		t.Fatalf("requests = %+v", streamer.requests)
	}
	if streamer.requests[0].TailLines != DefaultTailLines || streamer.requests[0].SinceSeconds == 0 || streamer.requests[0].Follow {
		t.Fatalf("first request missing history window: %+v", streamer.requests[0])
	}
	if streamer.requests[1].TailLines != 0 || streamer.requests[1].SinceSeconds != 1 || !streamer.requests[1].Follow {
		t.Fatalf("follow request should not replay history: %+v", streamer.requests[1])
	}
	if streamer.requests[2].TailLines != 0 || streamer.requests[2].SinceSeconds != 0 || !streamer.requests[2].Follow {
		t.Fatalf("reconnect request should not include history: %+v", streamer.requests[2])
	}
}

func TestStreamContainerFallsBackToRecentHistoryWhenSinceWindowIsEmpty(t *testing.T) {
	t.Parallel()

	streamer := &fakeStreamer{connects: map[string][]string{
		"app": {"", "older line\n"},
	}}
	model := testModel()
	model.streamer = streamer

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var entries []LogEntry
	err := model.streamContainer(ctx, "app", nil, func(e LogEntry) bool {
		entries = append(entries, e)
		cancel()
		return false
	})
	if err != nil {
		t.Fatalf("streamContainer error = %v", err)
	}
	if len(entries) != 1 || entries[0].Message != "older line" {
		t.Fatalf("entries = %+v, want one fallback line", entries)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("requests = %+v, want initial and fallback requests", streamer.requests)
	}
	if streamer.requests[0].SinceSeconds != 900 || streamer.requests[0].TailLines != DefaultTailLines {
		t.Fatalf("initial request = %+v, want since=15m", streamer.requests[0])
	}
	if streamer.requests[1].SinceSeconds != 0 || streamer.requests[1].TailLines != DefaultTailLines {
		t.Fatalf("fallback request = %+v, want one recent page", streamer.requests[1])
	}
	if streamer.requests[1].Follow {
		t.Fatalf("fallback request should be finite: %+v", streamer.requests[1])
	}
}

// TestStreamContainerFollowsAfterEmptyHistory: a container with no output
// yet reports empty once, then still follows — a line it writes later must
// arrive, from a follow connection with no since-window or tail that could
// skip it.
func TestStreamContainerFollowsAfterEmptyHistory(t *testing.T) {
	t.Parallel()

	streamer := &fakeStreamer{connects: map[string][]string{"app": {"", "", "later\n"}}}
	model := testModel()
	model.streamer = streamer

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	empties := 0
	var entries []LogEntry
	err := model.streamContainer(ctx, "app", func() { empties++ }, func(e LogEntry) bool {
		if empties != 1 {
			t.Errorf("line arrived with %d empty reports before it, want 1", empties)
		}
		entries = append(entries, e)
		cancel()
		return false
	})
	if err != nil {
		t.Fatalf("streamContainer error = %v", err)
	}
	if len(entries) != 1 || entries[0].Message != "later" {
		t.Fatalf("entries = %+v, want the line logged after the empty history", entries)
	}
	if len(streamer.requests) != 3 {
		t.Fatalf("requests = %+v, want history, fallback, then follow", streamer.requests)
	}
	if follow := streamer.requests[2]; !follow.Follow || follow.SinceSeconds != 0 || follow.TailLines != 0 {
		t.Fatalf("follow request = %+v, want Follow with no since-window or tail", follow)
	}
}

// TestStreamContainerWaitsForActualRestartBeforeReconnecting is a regression
// test for the "kute constantly repeats a log line, k9s shows it once"
// symptom: a container that ends its stream naturally without actually
// restarting (e.g. CrashLoopBackOff's real backoff is far longer than
// reconnectDelay) must not be reconnected to — that just replays the same
// terminated instance's full log again. With a lister available,
// streamContainer must wait for the container's live restart count to
// actually move before opening a new connection.
func TestStreamContainerWaitsForActualRestartBeforeReconnecting(t *testing.T) {
	t.Parallel()

	streamer := &fakeStreamer{connects: map[string][]string{
		"app": {"first\n", "", "second\n"},
	}}
	model := testModel()
	model.streamer = streamer
	model.lister = &fakeRestartLister{podName: "api", container: "app", restarts: 3}

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(700*time.Millisecond, cancel) // let one reconnectDelay tick pass with no restart

	var entries []LogEntry
	err := model.streamContainer(ctx, "app", nil, func(e LogEntry) bool {
		entries = append(entries, e)
		return true
	})
	if err != nil {
		t.Fatalf("streamContainer error = %v", err)
	}
	if len(entries) != 1 || entries[0].Message != "first" {
		t.Fatalf("entries = %+v, want only the first connect's line while restart count is unchanged", entries)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("requests = %+v, want exactly one connect attempt while restart count is unchanged", streamer.requests)
	}
}

// TestStreamContainerReconnectsAfterActualRestartDetected confirms the other
// half: once the container's live restart count actually moves, streamContainer
// still reconnects and synthesizes a boundary entry carrying the new count.
func TestStreamContainerReconnectsAfterActualRestartDetected(t *testing.T) {
	t.Parallel()

	streamer := &fakeStreamer{connects: map[string][]string{
		"app": {"first\n", "", "second\n"},
	}}
	lister := &fakeRestartLister{podName: "api", container: "app", restarts: 3}
	model := testModel()
	model.streamer = streamer
	model.lister = lister

	ctx, cancel := context.WithCancel(t.Context())
	var entries []LogEntry
	err := model.streamContainer(ctx, "app", nil, func(e LogEntry) bool {
		entries = append(entries, e)
		if e.Message == "first" {
			lister.restarts = 4 // the real restart happens now
		}
		if len(entries) == 3 { // first, boundary, second
			cancel()
			return false
		}
		return true
	})
	if err != nil {
		t.Fatalf("streamContainer error = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	if !entries[1].Boundary || !strings.Contains(entries[1].Message, "restart 4") {
		t.Fatalf("boundary entry = %+v, want restart count from the live restart", entries[1])
	}
	if entries[2].Message != "second" {
		t.Fatalf("post-reconnect entry = %+v", entries[2])
	}
}

func TestStreamContainerEndsWhenPodIsDeleted(t *testing.T) {
	t.Parallel()

	streamer := &fakeStreamer{connects: map[string][]string{"app": {"first\n", ""}}}
	lister := &fakeRestartLister{podName: "api", container: "app", restarts: 3}
	model := testModel()
	model.streamer = streamer
	model.lister = lister

	err := model.streamContainer(t.Context(), "app", nil, func(e LogEntry) bool {
		if e.Message == "first" {
			lister.deleted = true
		}
		return true
	})
	if !errors.Is(err, errPodDeleted) {
		t.Fatalf("streamContainer error = %v, want pod deleted", err)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("requests = %+v, want history plus one follow request and no retry loop", streamer.requests)
	}
}

func TestStreamContainerStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	streamer := &fakeStreamer{connects: map[string][]string{"app": {"only\n"}}}
	model := testModel()
	model.streamer = streamer

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := model.streamContainer(ctx, "app", nil, func(LogEntry) bool { return true })
	if err != nil {
		t.Fatalf("streamContainer error = %v", err)
	}
}

// TestStreamContainerStopsOnUnretrievableLogsLine is a regression test for a
// terminated/GC'd container: the kubelet logs endpoint answers with a 200 OK
// whose body is just "unable to retrieve container logs for containerd://…"
// (a long-standing kubelet quirk — never surfaces as an HTTP error client-go
// can see). Before this fix, streamContainer read that line as ordinary log
// content and reconnected every 500ms forever, spamming the buffer with the
// same line on an endless loop. It must instead surface once as an error.
func TestStreamContainerStopsOnUnretrievableLogsLine(t *testing.T) {
	t.Parallel()

	streamer := &fakeStreamer{connects: map[string][]string{
		"app": {
			"unable to retrieve container logs for containerd://f6095a3ed59be25aaf5ed084fbf43be5b124782cb2bfb1ef56e6bc4e7afdcaad\n",
			"unable to retrieve container logs for containerd://f6095a3ed59be25aaf5ed084fbf43be5b124782cb2bfb1ef56e6bc4e7afdcaad\n",
		},
	}}
	model := testModel()
	model.streamer = streamer

	var entries []LogEntry
	err := model.streamContainer(t.Context(), "app", nil, func(e LogEntry) bool {
		entries = append(entries, e)
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "unable to retrieve container logs for") {
		t.Fatalf("err = %v, want the unretrievable-logs line surfaced as an error", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none emitted as log content", entries)
	}
	if len(streamer.requests) != 1 {
		t.Fatalf("requests = %+v, want exactly one connect attempt (no reconnect loop)", streamer.requests)
	}
}

func TestStreamContainerReturnsConnectError(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.streamer = &fakeStreamer{err: errors.New("boom")}
	err := model.streamContainer(t.Context(), "app", nil, func(LogEntry) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestCurrentRestartCountFallsBackWithoutLister(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.pod.Restarts = 7
	if got := model.currentRestartCount(t.Context()); got != 7 {
		t.Fatalf("restart count = %d, want 7", got)
	}
}

func TestRunStreamValidation(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.pod.Name = ""
	msgs := collectRunStream(model)
	if _, ok := msgs[0].(streamErrorMsg); !ok {
		t.Fatalf("missing pod name did not produce streamErrorMsg: %+v", msgs)
	}

	model = testModel()
	model.pod.Containers = nil
	msgs = collectRunStream(model)
	if _, ok := msgs[0].(streamEmptyMsg); !ok {
		t.Fatalf("missing containers did not produce streamEmptyMsg: %+v", msgs)
	}

	model = testModel()
	model.streamer = nil
	msgs = collectRunStream(model)
	if _, ok := msgs[0].(streamErrorMsg); !ok {
		t.Fatalf("nil streamer did not produce streamErrorMsg: %+v", msgs)
	}
}

func collectRunStream(model Model) []tea.Msg {
	ch := make(chan tea.Msg, 16)
	model.runStream(context.Background(), 1, ch)
	var out []tea.Msg
	for msg := range ch {
		out = append(out, msg)
	}
	return out
}

// TestCheckContainerCmdReadyWhenRunning confirms the pre-connect check lets
// a running container through as containerReadyMsg.
func TestCheckContainerCmdReadyWhenRunning(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.lister = &fakeRestartLister{podName: "api", container: "app", state: "Running"}

	msg := model.checkContainerCmd(1)()
	ready, ok := msg.(containerReadyMsg)
	if !ok || ready.streamID != 1 {
		t.Fatalf("msg = %#v, want containerReadyMsg{streamID: 1}", msg)
	}
}

// TestCheckContainerCmdWaitingWhenContainerCreating is the core of the
// feature: a container still ContainerCreating must not be treated as ready
// to stream.
func TestCheckContainerCmdWaitingWhenContainerCreating(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.lister = &fakeRestartLister{podName: "api", container: "app", state: "Waiting", reason: "ContainerCreating"}

	msg := model.checkContainerCmd(1)()
	waiting, ok := msg.(containerWaitingMsg)
	if !ok || waiting.streamID != 1 || waiting.reason != "ContainerCreating" {
		t.Fatalf("msg = %#v, want containerWaitingMsg{streamID: 1, reason: \"ContainerCreating\"}", msg)
	}
}

// TestCheckContainerCmdStreamsCrashLoopingContainer is review 04 H2: a
// CrashLoopBackOff container is Waiting, but it has a terminated previous
// run the kubelet serves logs for. Parking it hid the crash output — the
// one thing the user opened logs to read — behind "waiting for container to
// start" until a Pod event happened to land while it was briefly Running.
func TestCheckContainerCmdStreamsCrashLoopingContainer(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.lister = &fakeRestartLister{podName: "api", container: "app", state: "Waiting", reason: "CrashLoopBackOff", restarts: 6, lastTerminated: true}

	msg := model.checkContainerCmd(1)()
	ready, ok := msg.(containerReadyMsg)
	if !ok || ready.streamID != 1 || ready.crashLoop != "CrashLoopBackOff" {
		t.Fatalf("msg = %#v, want containerReadyMsg{streamID: 1, crashLoop: \"CrashLoopBackOff\"}", msg)
	}
}

// TestCheckContainerCmdParksNeverStartedImagePull is the other half of H2's
// line: a Waiting container with no previous run has nothing to read, so it
// still parks — ImagePullBackOff on a first pull, not just ContainerCreating.
func TestCheckContainerCmdParksNeverStartedImagePull(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.lister = &fakeRestartLister{podName: "api", container: "app", state: "Waiting", reason: "ImagePullBackOff"}

	msg := model.checkContainerCmd(1)()
	waiting, ok := msg.(containerWaitingMsg)
	if !ok || waiting.reason != "ImagePullBackOff" {
		t.Fatalf("msg = %#v, want containerWaitingMsg{reason: \"ImagePullBackOff\"}", msg)
	}
}

// TestCheckContainerCmdReadyWhenLookupFails is the fail-open guard: a
// lister error must never hang the screen in StreamWaitingForContainer —
// it degrades to the old "just try to connect" behavior instead.
func TestCheckContainerCmdReadyWhenLookupFails(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.lister = &fakeRestartLister{podName: "api", container: "app", listErr: errors.New("boom")}

	msg := model.checkContainerCmd(1)()
	if _, ok := msg.(containerReadyMsg); !ok {
		t.Fatalf("msg = %#v, want containerReadyMsg on lookup failure", msg)
	}
}

// TestBeginStreamSkipsCheckWithoutLister is a regression guard: beginStream
// must connect immediately, exactly as the old restartStream did, when no
// lister is wired to check against (testModel() itself, and any other
// caller/test that never sets one).
func TestBeginStreamSkipsCheckWithoutLister(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.streamer = &fakeStreamer{connects: map[string][]string{"app": {""}}}
	cmd := model.beginStream(StreamLoading)
	if model.streamCancel == nil {
		t.Fatalf("streamCancel is nil, want beginStream to have connected synchronously without a lister")
	}
	if cmd == nil {
		t.Fatalf("beginStream returned a nil cmd")
	}
	model.cancelStream()
}

// TestRunStreamTreatsContainerNotStartedErrorAsWaiting covers the narrow
// race beginStream's pre-check can't fully close: the container flips back
// to waiting between that check and the actual connect attempt. The
// kubelet's fixed error text for that must not surface as a fatal
// streamErrorMsg.
func TestRunStreamTreatsContainerNotStartedErrorAsWaiting(t *testing.T) {
	t.Parallel()

	model := testModel()
	model.streamer = &fakeStreamer{err: errors.New(`container "app" in pod "api" is waiting to start: ContainerCreating`)}
	msgs := collectRunStream(model)
	if len(msgs) != 1 {
		t.Fatalf("msgs = %+v, want exactly one message", msgs)
	}
	waiting, ok := msgs[0].(containerWaitingMsg)
	if !ok {
		t.Fatalf("msgs[0] = %#v, want containerWaitingMsg, not a fatal streamErrorMsg", msgs[0])
	}
	if waiting.reason == "" {
		t.Fatalf("waiting.reason is empty, want a non-empty fallback reason")
	}
}

// TestWaitForStreamBatchesQueuedLines is the throughput fix from
// docs/performance.md: everything already sitting on the channel has to come
// back as one message, because Bubble Tea renders once per message and a
// per-line render capped a saturating burst at roughly 45 lines/sec.
func TestWaitForStreamBatchesQueuedLines(t *testing.T) {
	t.Parallel()

	ch := make(chan tea.Msg, streamChanBuffer)
	for i := range 10 {
		ch <- logLineMsg{streamID: 3, entry: LogEntry{Container: "app", Message: fmt.Sprintf("line-%d", i)}}
	}
	batch, ok := waitForStream(ch)().(logBatchMsg)
	if !ok {
		t.Fatalf("waitForStream did not return a logBatchMsg")
	}
	if len(batch.entries) != 10 || batch.streamID != 3 || batch.tail != nil {
		t.Fatalf("batch = %+v, want 10 entries for stream 3 and no tail", batch)
	}
	for i, entry := range batch.entries {
		if want := fmt.Sprintf("line-%d", i); entry.Message != want {
			t.Fatalf("entries[%d] = %q, want %q — batching must preserve order", i, entry.Message, want)
		}
	}
	if len(ch) != 0 {
		t.Fatalf("%d messages left queued, want the drain to have taken them all", len(ch))
	}
}

// The drain must not swallow the ordering of the messages around the lines:
// a stream error/close, or a line from a superseded stream, ends the batch
// and rides along as its tail for Update to handle next.
func TestWaitForStreamStopsBatchingAtNonLineMessages(t *testing.T) {
	t.Parallel()

	streamErr := streamErrorMsg{streamID: 3, err: errors.New("boom")}
	tests := map[string]struct {
		queue     []tea.Msg
		wantLines int
		wantTail  tea.Msg
	}{
		"error ends the batch": {
			queue:     []tea.Msg{logLineMsg{streamID: 3, entry: LogEntry{Message: "a"}}, streamErr},
			wantLines: 1,
			wantTail:  streamErr,
		},
		"superseded stream ends the batch": {
			queue: []tea.Msg{
				logLineMsg{streamID: 3, entry: LogEntry{Message: "a"}},
				logLineMsg{streamID: 4, entry: LogEntry{Message: "b"}},
			},
			wantLines: 1,
			wantTail:  logLineMsg{streamID: 4, entry: LogEntry{Message: "b"}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ch := make(chan tea.Msg, streamChanBuffer)
			for _, msg := range tc.queue {
				ch <- msg
			}
			batch, ok := waitForStream(ch)().(logBatchMsg)
			if !ok {
				t.Fatalf("waitForStream did not return a logBatchMsg")
			}
			if len(batch.entries) != tc.wantLines {
				t.Fatalf("entries = %+v, want %d", batch.entries, tc.wantLines)
			}
			if batch.tail != tc.wantTail {
				t.Fatalf("tail = %#v, want %#v", batch.tail, tc.wantTail)
			}
		})
	}
}

// A message that isn't a log line at all still comes back untouched, so the
// existing streamErrorMsg/streamEmptyMsg/streamClosedMsg cases in Update keep
// seeing exactly what they always have.
func TestWaitForStreamPassesNonLineMessagesThrough(t *testing.T) {
	t.Parallel()

	ch := make(chan tea.Msg, 1)
	ch <- streamEmptyMsg{streamID: 3}
	if msg := waitForStream(ch)(); msg != (tea.Msg(streamEmptyMsg{streamID: 3})) {
		t.Fatalf("msg = %#v, want the streamEmptyMsg unchanged", msg)
	}
}

// A batch may not run away with the command goroutine: a producer faster
// than the viewer would otherwise keep the drain loop fed forever and never
// yield a frame.
func TestWaitForStreamCapsBatchSize(t *testing.T) {
	t.Parallel()

	ch := make(chan tea.Msg, maxLogBatch*2)
	for range maxLogBatch + 20 {
		ch <- logLineMsg{streamID: 1, entry: LogEntry{Message: "x"}}
	}
	batch, ok := waitForStream(ch)().(logBatchMsg)
	if !ok {
		t.Fatalf("waitForStream did not return a logBatchMsg")
	}
	if len(batch.entries) != maxLogBatch {
		t.Fatalf("entries = %d, want the batch capped at %d", len(batch.entries), maxLogBatch)
	}
	if batch.tail != nil {
		t.Fatalf("tail = %#v, want nil — a capped batch just comes back for more", batch.tail)
	}
	if len(ch) != 20 {
		t.Fatalf("%d messages left queued, want the 20 the cap deferred", len(ch))
	}
}

func TestWaitForStreamReportsClosedChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan tea.Msg, 1)
	ch <- logLineMsg{streamID: 1, entry: LogEntry{Message: "last"}}
	close(ch)
	batch, ok := waitForStream(ch)().(logBatchMsg)
	if !ok {
		t.Fatalf("waitForStream did not return a logBatchMsg")
	}
	if len(batch.entries) != 1 || batch.tail != (tea.Msg(streamWaitMsg{})) {
		t.Fatalf("batch = %+v, want the final line plus a streamWaitMsg tail", batch)
	}
}
