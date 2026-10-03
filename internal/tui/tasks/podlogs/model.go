// Package podlogs is 5b (docs/design/README.md §5b): the streaming
// log-view screen, reached from tasks/browse's Pods list and tasks/
// poddetail on 'l'. Restyled onto Chrome v2 in Phase 6 (mvp-tasks.md) —
// stream machinery (this file, stream.go) carries over from the pre-
// redesign screen; view.go/keys.go are new.
package podlogs

import (
	"context"
	"slices"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

const DefaultMaxEntries = 5000
const DefaultTailLines int64 = 200

// SelectedPod is the pod identity + container list podlogs needs — a
// trimmed projection of kube.Pod (no CPU/MEM/Age/Node: 5b's toolbar has no
// slot for them, and the pod that opened this screen — browse or
// poddetail — already showed that metadata).
type SelectedPod struct {
	Context    string
	Namespace  string
	Name       string
	Containers []string
	Restarts   int32
}

// sinceOption is one entry in the 's' since-window cycle.
type sinceOption struct {
	label   string
	seconds int64
}

// sinceOptions is the 's' cycle (docs/design README.md §5b: "since 15m ...
// s changes since-window"); index 1 ("15m") is the toolbar's default.
var sinceOptions = []sinceOption{
	{"5m", 300},
	{"15m", 900},
	{"1h", 3600},
	{"6h", 21600},
	{"all", 0},
}

const defaultSinceIndex = 1

// LogViewState is the display-only presentation state — none of these
// fields require a stream restart to change (Timestamps/Wrap are parsed/
// wrapped at render time; only the container and since-window selections,
// which change what's actually requested from the API, do).
type LogViewState struct {
	AutoScroll       bool // "follow" (docs/design README.md §5b)
	Wrap             bool
	Timestamps       bool
	VerticalOffset   int
	HorizontalOffset int
	Width            int
	Height           int
}

// LogEntry is one rendered line: either a real streamed log line
// (Container/Timestamp/Message/Severity) or a synthesized restart-boundary
// marker (Boundary set, Message carries "container restarted · restart N").
type LogEntry struct {
	Container string
	Timestamp string
	Message   string
	Severity  string // "", SeverityInfo, SeverityWarn, SeverityErr
	Boundary  bool
}

type LogBuffer struct {
	Entries      []LogEntry
	MaxEntries   int
	DroppedCount int
}

type StreamState string

const (
	StreamIdle      StreamState = "idle"
	StreamLoading   StreamState = "loading"
	StreamStreaming StreamState = "streaming"
	// StreamWaitingForContainer is the pre-connect state: the active
	// container hasn't started yet per the pod cache (checkContainerCmd's
	// containerWaitingMsg, stream.go), so no stream has been opened at all.
	// Distinct from StreamReconnecting, which is between two actual
	// connections against a container that has already run. Resolved by
	// the next kube.ResourceChangedMsg{Kind: KindPod} (update.go), not a
	// timer.
	StreamWaitingForContainer StreamState = "waiting-container"
	StreamReconnecting        StreamState = "reconnecting"
	StreamEmpty               StreamState = "empty"
	StreamError               StreamState = "error"
	StreamClosed              StreamState = "closed"
)

// Config are podlogs' dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values).
type Config struct {
	Session *tui.Session
	// Lister is optional — used only to refresh the pod's live restart
	// count when synthesizing a reconnect boundary entry. A nil Lister
	// falls back to the Restarts count captured when the screen opened.
	Lister   resources.RawLister
	Pod      SelectedPod
	Streamer kube.PodLogStreamer
	// InitialContainer names the container to start streaming (poddetail's
	// 'l' opens on whichever row the CONTAINERS grid had selected) — empty
	// falls back to index 0, unchanged from before this field existed. A
	// name that isn't in Pod.Containers (stale selection, race with a pod
	// update) falls back the same way rather than erroring.
	InitialContainer string
	MaxEntries       int
	TailLines        int64
}

type Model struct {
	width, height int

	session *tui.Session
	lister  resources.RawLister

	pod          SelectedPod
	containerIdx int
	sinceIdx     int

	view          LogViewState
	buffer        LogBuffer
	stream        StreamState
	lastError     string
	permDenied    bool
	waitingReason string // set only while stream == StreamWaitingForContainer
	// crashLoopReason is the active container's Waiting reason when the
	// stream was opened on its previous run (CrashLoopBackOff): the logs
	// shown are the last crash, and the status line says so. Cleared by
	// beginStream.
	crashLoopReason string
	feedback        string
	streamer        kube.PodLogStreamer
	tailLines       int64
	conn            kube.ConnState

	streamCancel context.CancelFunc
	streamCh     chan tea.Msg
	streamID     int

	filterActive bool
	filterInput  textfield.Model

	// rowCounts is the layout index: how many physical rows each entry in
	// buffer.Entries occupies, 0 for an entry the '/' filter hides. It exists
	// so a frame costs the visible rows rather than the whole buffer —
	// laying out all 5,000 entries to show 30 of them is what capped a
	// saturating log burst at ~45 lines/sec (docs/performance.md). Maintained
	// incrementally by appendEntry from numbers it already computes, and
	// rebuilt wholesale only when layoutKey changes.
	rowCounts  []int
	totalRows  int // sum of rowCounts
	matchCount int // entries passing the filter, for the filter strip's "matched/total"
	lastErr    int // newest ERR entry passing the filter, -1 for none
	layout     layoutKey

	rateGen        int
	linesSinceTick int
	lastRate       int

	spinner spinner.Model
}

func New(cfg Config) Model {
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultMaxEntries
	}
	if cfg.TailLines <= 0 {
		cfg.TailLines = DefaultTailLines
	}

	containerIdx := 0
	for i, name := range cfg.Pod.Containers {
		if name == cfg.InitialContainer {
			containerIdx = i
			break
		}
	}

	model := Model{
		width:        tui.DefaultWidth,
		height:       tui.DefaultHeight,
		session:      cfg.Session,
		lister:       cfg.Lister,
		pod:          cfg.Pod,
		containerIdx: containerIdx,
		view: LogViewState{
			AutoScroll: true,
			Wrap:       true,
			Width:      tui.DefaultWidth,
			Height:     tui.DefaultHeight,
		},
		buffer:    LogBuffer{MaxEntries: cfg.MaxEntries},
		stream:    StreamIdle,
		feedback:  "Loading logs...",
		streamer:  cfg.Streamer,
		tailLines: cfg.TailLines,
		sinceIdx:  defaultSinceIndex,
		spinner:   components.NewSpinner(),
	}
	model.rebuildLayout()
	return model
}

// FromPod builds podlogs for pod, opened from browse/poddetail — the shape
// every OpenLogsFunc caller in internal/app wires against. initialContainer
// is the container to start streaming (poddetail's CONTAINERS grid
// selection) — empty falls back to index 0.
func FromPod(session *tui.Session, lister resources.RawLister, pod kube.Pod, initialContainer string, streamer kube.PodLogStreamer) Model {
	return New(Config{
		Session: session,
		Lister:  lister,
		Pod: SelectedPod{
			Context:    pod.Context,
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			Containers: logContainerNames(pod),
			Restarts:   pod.Restarts,
		},
		InitialContainer: initialContainer,
		Streamer:         streamer,
	})
}

func logContainerNames(pod kube.Pod) []string {
	capacity := len(pod.Containers) + len(pod.ContainerInfos) + len(pod.InitContainerInfos) + len(pod.EphemeralContainerInfos)
	names := make([]string, 0, capacity)
	seen := make(map[string]bool, capacity)
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, name := range pod.Containers {
		add(name)
	}
	for _, info := range pod.ContainerInfos {
		add(info.Name)
	}
	for _, info := range pod.InitContainerInfos {
		add(info.Name)
	}
	for _, info := range pod.EphemeralContainerInfos {
		add(info.Name)
	}
	return names
}

func (m Model) Init() tea.Cmd { return nil }

// Start begins streaming — called once by the OpenLogsFunc caller
// alongside pushing the screen (mirrors poddetail/nodedetail's Init()
// pattern, kept as an explicit method since browse already calls it that
// way and changing the call site isn't otherwise needed).
func (m *Model) Start() tea.Cmd { return m.beginStream(StreamLoading) }

func (m Model) Theme() tui.Theme {
	if m.session != nil {
		return m.session.Theme
	}
	return tui.Dark()
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
	m.view.Width, m.view.Height = size.Width, size.Height
	m.clampOffsets()
}

func (b *LogBuffer) Append(entry LogEntry) {
	if b.MaxEntries <= 0 {
		b.MaxEntries = DefaultMaxEntries
	}
	// Trim before append so a saturated buffer reuses its backing array. The
	// old append-to-nil trim allocated and copied all 5,000 entries for every
	// incoming line after saturation.
	if over := len(b.Entries) - b.MaxEntries + 1; over > 0 {
		copy(b.Entries, b.Entries[over:])
		newLen := len(b.Entries) - over
		clear(b.Entries[newLen:])
		b.Entries = b.Entries[:newLen]
		b.DroppedCount += over
	}
	b.Entries = append(b.Entries, entry)
}

// layoutKey is every input that changes how many physical rows an entry
// occupies. HorizontalOffset is deliberately absent: it only applies when
// wrap is off, where an entry is exactly one row whatever the offset — so
// h/l scrolling never invalidates the index.
type layoutKey struct {
	width      int
	wrap       bool
	timestamps bool
	filter     string
}

func (m Model) currentLayoutKey() layoutKey {
	return layoutKey{
		width:      m.view.Width,
		wrap:       m.view.Wrap,
		timestamps: m.view.Timestamps,
		filter:     m.filterInput.Value(),
	}
}

// layoutValid reports whether rowCounts still describes the current buffer at
// the current layout. Every mutating path syncs (clampOffsets, appendEntry),
// so this is true in the app; the render path checks it anyway because tests
// and golden fixtures populate buffer.Entries directly and must keep
// rendering correctly — a stale index degrades to the old full pass rather
// than to a wrong screen.
func (m Model) layoutValid() bool {
	return len(m.rowCounts) == len(m.buffer.Entries) && m.layout == m.currentLayoutKey()
}

// syncLayout is the single place a stale index is repaired. It sits at the
// top of clampOffsets — which every key, resize and append path already runs
// through — rather than being called from each of them, so a new toggle
// can't forget it. Never call it from a render path: Render stays pure.
func (m *Model) syncLayout() {
	if !m.layoutValid() {
		m.rebuildLayout()
	}
}

// rebuildLayout is the one O(buffer) layout pass, reached only on the
// human-rate events that genuinely invalidate the index: a resize, the wrap
// or timestamps toggle, and each edit of the '/' query.
func (m *Model) rebuildLayout() {
	if cap(m.rowCounts) < len(m.buffer.Entries) {
		m.rowCounts = make([]int, len(m.buffer.Entries))
	}
	m.rowCounts = m.rowCounts[:len(m.buffer.Entries)]
	m.totalRows, m.matchCount, m.lastErr = 0, 0, -1
	for i, entry := range m.buffer.Entries {
		rows := 0
		if m.entryMatchesFilter(entry) {
			rows = m.entryRowCount(entry)
			m.matchCount++
			if entry.Severity == SeverityErr {
				m.lastErr = i
			}
		}
		m.rowCounts[i] = rows
		m.totalRows += rows
	}
	m.layout = m.currentLayoutKey()
}

// entryAtRow maps a physical row offset to the entry containing it and that
// entry's own first row, walking rowCounts from whichever end is closer — a
// following viewport sits at the tail, so the common case never touches the
// front of the buffer. Returns len(rowCounts) when offset is past the last
// row.
func (m Model) entryAtRow(offset int) (index, rowsBefore int) {
	if offset*2 > m.totalRows {
		i, rows := len(m.rowCounts), m.totalRows
		for i > 0 {
			n := m.rowCounts[i-1]
			if rows-n <= offset {
				if rows > offset {
					return i - 1, rows - n
				}
				break
			}
			i--
			rows -= n
		}
		return i, rows
	}
	rows := 0
	for i, n := range m.rowCounts {
		if rows+n > offset {
			return i, rows
		}
		rows += n
	}
	return len(m.rowCounts), rows
}

// totalRowCount, matchedCount and lastErrEntry are the three whole-buffer
// facts the chrome needs. Each answers from the index, falling back to the
// full computation for a buffer the index hasn't seen (see layoutValid).
func (m Model) totalRowCount() int {
	if m.layoutValid() {
		return m.totalRows
	}
	return len(m.visualRows(m.filteredEntries(), m.view.Width))
}

func (m Model) matchedCount() int {
	if m.layoutValid() {
		return m.matchCount
	}
	return len(m.filteredEntries())
}

// lastErrEntry is the index into buffer.Entries of the newest ERR entry the
// filter keeps — docs/design README.md §5b's "full-width red-tinted row for
// the most significant one".
func (m Model) lastErrEntry() int {
	if m.layoutValid() {
		return m.lastErr
	}
	for i, e := range slices.Backward(m.buffer.Entries) {
		if e.Severity == SeverityErr && m.entryMatchesFilter(e) {
			return i
		}
	}
	return -1
}

func (m *Model) appendEntry(entry LogEntry) {
	// Load-bearing, not defensive: everything below reads and splices
	// rowCounts positionally against buffer.Entries, so the index has to
	// describe the buffer before the first line lands (a caller can have
	// filled the buffer directly, and clampOffsets — the usual choke point —
	// only runs at the end of the follow path).
	m.syncLayout()
	// Once following has enough rows to scroll, advance the bottom offset by
	// only the rows added/evicted here. Re-laying out the full 5,000-entry
	// buffer for every streamed line would make wrapping quadratic at steady
	// state. Before the first scroll point, clampOffsets performs the small
	// full calculation needed to detect when the viewport becomes full.
	oldViewportHeight := m.entryViewportHeight()
	// Normalized exactly as LogBuffer.Append normalizes it: the index trims in
	// lockstep with the buffer, so the two must agree on how many entries the
	// coming append evicts.
	maxEntries := m.buffer.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	// The evicted entries' row counts are already in the index — reading them
	// back beats re-wrapping every evicted line, which at steady state is one
	// re-wrap per streamed line.
	droppedRows := m.trimRowIndex(len(m.buffer.Entries) - maxEntries + 1)
	addedRows := 0
	if m.entryMatchesFilter(entry) {
		addedRows = m.entryRowCount(entry)
	}
	wasScrollableFollow := m.view.AutoScroll && m.view.VerticalOffset > 0

	m.buffer.Append(entry)
	m.pushRowIndex(addedRows, entry.Severity)
	m.linesSinceTick++
	if m.stream != StreamError {
		m.stream = StreamStreaming
	}
	m.feedback = ""
	switch {
	case wasScrollableFollow:
		// A state/strip change or the first persistent "older lines dropped"
		// notice can also change the number of physical rows available.
		viewportDelta := oldViewportHeight - m.entryViewportHeight()
		m.view.VerticalOffset = max(0, m.view.VerticalOffset+addedRows-droppedRows+viewportDelta)
	case m.view.AutoScroll:
		m.clampOffsets()
	case droppedRows > 0:
		// Keep the same logical content anchored when the bounded buffer evicts
		// physical rows above a paused viewport.
		m.view.VerticalOffset = max(0, m.view.VerticalOffset-droppedRows)
	}
}

// trimRowIndex drops the oldest n entries from the row index, mirroring
// LogBuffer.Append's own trim — the same copy-down, so a saturated buffer
// keeps reusing its backing array — and reports how many physical rows went
// with them.
func (m *Model) trimRowIndex(n int) int {
	if n <= 0 {
		return 0
	}
	rows, matches := 0, 0
	for _, count := range m.rowCounts[:n] {
		if count > 0 {
			rows += count
			matches++
		}
	}
	copy(m.rowCounts, m.rowCounts[n:])
	m.rowCounts = m.rowCounts[:len(m.rowCounts)-n]
	m.totalRows -= rows
	m.matchCount -= matches
	// lastErr indexes the *newest* ERR entry, so if eviction reaches it,
	// nothing newer was an ERR and there is none left to tint.
	m.lastErr = max(m.lastErr-n, -1)
	return rows
}

// pushRowIndex records the entry LogBuffer.Append has just added; rows is 0
// for an entry the filter hides.
func (m *Model) pushRowIndex(rows int, severity string) {
	m.rowCounts = append(m.rowCounts, rows)
	m.totalRows += rows
	if rows > 0 {
		m.matchCount++
		if severity == SeverityErr {
			m.lastErr = len(m.rowCounts) - 1
		}
	}
}

// filteredEntries is the buffer narrowed by the live '/' filter (a plain
// case-insensitive substring match on the message — simpler and cheaper
// than the table's fuzzy grammar, and a better fit for searching arbitrary
// log text than fuzzy-matching short names). Boundary markers always pass,
// so a restart's context isn't hidden by an unrelated filter.
func (m Model) filteredEntries() []LogEntry {
	if m.filterInput.Value() == "" {
		return m.buffer.Entries
	}
	out := make([]LogEntry, 0, len(m.buffer.Entries))
	for _, e := range m.buffer.Entries {
		if m.entryMatchesFilter(e) {
			out = append(out, e)
		}
	}
	return out
}

func (m Model) entryMatchesFilter(entry LogEntry) bool {
	query := strings.ToLower(m.filterInput.Value())
	return query == "" || entry.Boundary || strings.Contains(strings.ToLower(entry.Message), query)
}

func (m *Model) clampOffsets() {
	// The choke point for the row index: every key, resize, filter edit and
	// append path already ends here, so the index is repaired in one place
	// instead of at each mutation site.
	m.syncLayout()
	if m.view.VerticalOffset < 0 {
		m.view.VerticalOffset = 0
	}
	maxOff := m.maxVerticalOffset()
	if m.view.AutoScroll {
		m.view.VerticalOffset = maxOff
	} else if m.view.VerticalOffset > maxOff {
		m.view.VerticalOffset = maxOff
	}
	if m.view.HorizontalOffset < 0 || m.view.Wrap {
		m.view.HorizontalOffset = 0
	}
}

func (m Model) maxVerticalOffset() int {
	visible := m.entryViewportHeight()
	total := m.totalRowCount()
	if total <= visible {
		return 0
	}
	return total - visible
}

func (m Model) entryViewportHeight() int {
	height := m.viewportHeight() - 1 // bottom status line
	if m.buffer.DroppedCount > 0 {
		height--
	}
	if height < 1 {
		return 1
	}
	return height
}

// viewportHeight is the body height Frame budgets, computed the same way
// every other Chrome v2 screen does (tui.FrameBodyHeight against this
// screen's strip-line count) — stripLineCount rather than len(Strips(...))
// deliberately, since Strips renders the toolbar's severity-in-view counts
// from the very entry viewport this method computes (a real cycle: Strips
// -> visibleEntries -> entryViewportHeight -> viewportHeight -> Strips).
func (m Model) viewportHeight() int {
	return tui.FrameBodyHeight(m.height, m.stripLineCount())
}

// stripLineCount is Strips' line count without rendering it — see
// viewportHeight's doc comment for why that split matters.
func (m Model) stripLineCount() int {
	if m.taskState() != tui.TaskStateReady {
		if m.filterActive {
			return 1
		}
		return 0
	}
	n := 1
	if m.filterActive {
		n++
	}
	return n
}

func (m Model) activeContainer() (string, bool) {
	if len(m.pod.Containers) == 0 {
		return "", false
	}
	idx := m.containerIdx
	if idx < 0 || idx >= len(m.pod.Containers) {
		idx = 0
	}
	return m.pod.Containers[idx], true
}

// nextContainer is what 'tab' would switch to — used by both the toolbar's
// "(tab: metrics-sidecar)" hint and cycleContainer's mutation, so the two
// never drift.
func (m Model) nextContainerIndex() int {
	if len(m.pod.Containers) == 0 {
		return 0
	}
	return (m.containerIdx + 1) % len(m.pod.Containers)
}

func (m *Model) cycleContainer() {
	if len(m.pod.Containers) <= 1 {
		return
	}
	m.containerIdx = m.nextContainerIndex()
}

func (m *Model) cycleSince() {
	m.sinceIdx = (m.sinceIdx + 1) % len(sinceOptions)
}

func (m Model) sinceSeconds() int64 { return sinceOptions[m.sinceIdx].seconds }
func (m Model) sinceLabel() string  { return sinceOptions[m.sinceIdx].label }

func (m *Model) cancelStream() {
	if m.streamCancel != nil {
		m.streamCancel()
		m.streamCancel = nil
	}
}

func (m Model) scope() string {
	container, _ := m.activeContainer()
	return strings.TrimSpace(m.pod.Context + "/" + m.pod.Namespace + "/" + m.pod.Name + "/" + container)
}

func (m Model) taskState() tui.TaskState {
	switch m.stream {
	case StreamEmpty:
		return tui.TaskStateEmpty
	case StreamError:
		// permDenied, not a re-classification of lastError: that field is
		// only the error's *text*, and rebuilding an error from it threw away
		// the *apierrors.StatusError, so IsPermissionError could never reach
		// its apierrors.IsForbidden path here — only its substring fallback.
		// update.go classifies the real error the moment it arrives.
		if m.permDenied {
			return tui.TaskStatePermissionDenied
		}
		return tui.TaskStateError
	case StreamLoading, StreamReconnecting, StreamIdle, StreamWaitingForContainer:
		return tui.TaskStateLoading
	default:
		return tui.TaskStateReady
	}
}
