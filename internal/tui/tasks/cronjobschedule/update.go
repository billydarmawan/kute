package cronjobschedule

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget is whichever buffer currently has focus — schedule by
// default, timezone only once 'tab' has moved focus there (and only when
// tzEditable). Built on a pointer receiver per CLAUDE.md's own paste
// warning: the returned closure must hold *m, not a copy, or the
// recomputeAll() call inside it would mutate a value nobody keeps.
func (m *Model) pasteTarget() tui.PasteTarget {
	if m.state != tui.TaskStateReady {
		return nil
	}
	if m.tzFocused && m.tzEditable() {
		insert := tui.PasteInto(&m.tzInput)
		return func(s string) {
			insert(s)
			m.recomputeAll()
		}
	}
	insert := tui.PasteInto(&m.scheduleInput)
	return func(s string) {
		insert(s)
		m.recomputeAll()
	}
}

// Reload implements tui.Reloader — see its doc comment: this screen misses
// every kube.ResourceChangedMsg while parked in the stack, so BackMsg
// restoring it asks it to catch up immediately rather than showing stale
// data until an unrelated change happens to land while it's active again.
func (m *Model) Reload() tea.Cmd {
	m.reloadEpoch++
	return m.load()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		if msg.Kind == kube.KindCronJob {
			m.reloadEpoch++
			return m, m.load()
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
	case loadedMsg:
		return m.applyLoaded(msg)
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.reloadEpoch {
			return m, m.load()
		}
	case tickMsg:
		m.now = time.Time(msg)
		var cmds []tea.Cmd
		cmds = append(cmds, tickCmd())
		if m.comparisonDirty && !m.comparing {
			m.comparing = true
			cmds = append(cmds, runComparison(m.editGen, m.accepted, m.pendingSchedule(), m.pendingTimeZone(), m.now))
		}
		return m, tea.Batch(cmds...)
	case whatChangesMsg:
		if msg.gen == m.editGen {
			m.comparison = &msg.result
			m.comparing = false
			m.comparisonDirty = false
		}
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case actions.ResultMsg:
		m.actions.HandleResult(msg)
		// This screen's own will-run strip is the result's surface (the
		// spec'd inline result line), so the keybar doesn't repeat it.
		m.actions.DismissResult()
		return m, m.handleResult(msg)
	case editResultMsg:
		if msg.err != nil {
			m.feedback = msg.err.Error()
		}
		m.reloadEpoch++
		return m, m.load()
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actions.DismissResult()
		return m.updateKey(msg)
	}
	return m, nil
}

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.reloadEpoch {
		return m, nil
	}
	if msg.err != nil {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = msg.err.Error()
		return m, nil
	}
	if !tui.KindsSynced(m.lister, m.namespace, kube.KindCronJob) {
		// §4.4's anti-hang rule applies here exactly as it does to browse's
		// own CronJob branch: stay loading and retry shortly rather than
		// reporting "not found" against a cache that hasn't finished
		// filling yet.
		m.reloadEpoch++
		return m, tui.ScheduleCacheSyncRetry(m.reloadEpoch)
	}
	if err := tui.KindsError(m.lister, m.namespace, kube.KindCronJob); err != nil {
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		} else {
			m.state = tui.TaskStateError
		}
		m.feedback = err.Error()
		return m, nil
	}
	if !msg.found {
		m.state = tui.TaskStateError
		m.feedback = fmt.Sprintf("cronjob %s/%s not found", m.namespace, m.name)
		return m, nil
	}

	accepted := acceptedPair{
		schedule:        msg.cj.schedule,
		timezone:        msg.cj.timezone,
		hasTimezone:     msg.cj.hasTimezone,
		resourceVersion: msg.cj.resourceVersion,
		generation:      msg.cj.generation,
	}
	m.accepted = accepted
	if !m.initialized {
		// The very first load seeds the editable buffers; every later
		// reload (a watch event, or our own post-apply reconciliation)
		// only refreshes m.accepted's bookkeeping above — the buffer stays
		// exactly what the user typed, per task 11's "preserve the user's
		// buffer" contract. A Conflict failure's stale resourceVersion is
		// healed the same way: the next watch event that lands quietly
		// brings m.accepted.resourceVersion current, so the next ↵ retries
		// against the real precondition without any dedicated "reload" key.
		m.scheduleInput.SetValue(accepted.schedule)
		m.scheduleInput.CursorEnd()
		m.scheduleInput.Focus()
		if accepted.hasTimezone {
			m.tzInput.SetValue(accepted.timezone)
			m.tzInput.CursorEnd()
		}
		m.initialized = true
		m.recomputeAll()
	}
	m.state = tui.TaskStateReady
	m.feedback = ""
	return m, nil
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.state != tui.TaskStateReady {
		switch msg.String() {
		case "ctrl+q", "ctrl+c":
			return m, tea.Quit
		case "esc", "backspace":
			return m, func() tea.Msg { return tui.BackMsg{} }
		}
		return m, nil
	}
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case verbs.CronJobFocusTimezone.Key, "shift+tab":
		if m.tzEditable() {
			m.tzFocused = !m.tzFocused
			if m.tzFocused {
				m.scheduleInput.Blur()
				m.tzInput.Focus()
			} else {
				m.tzInput.Blur()
				m.scheduleInput.Focus()
			}
		}
		return m, nil
	case verbs.CronJobScheduleFullEdit.Key:
		if m.mutator != nil && !m.conn.Offline() {
			return m, editCmd(m.session.CommandTarget(), m.namespace, m.name)
		}
		return m, nil
	case "enter":
		if m.mutator != nil && !m.conn.Offline() && m.pendingCommit == nil &&
			m.parseErr == nil && m.tzErr == nil && m.hasPendingChange() {
			return m, m.commitApply()
		}
		return m, nil
	case verbs.CronJobScheduleCopy.Key:
		return m, tea.SetClipboard(m.willRunCommand())
	case verbs.CronJobScheduleUndo.Key:
		if m.previous != nil && m.mutator != nil && !m.conn.Offline() && m.pendingCommit == nil {
			return m, m.commitUndo()
		}
		return m, nil
	}
	// Copy and undo are control chords (verbs.CronJobScheduleCopy/
	// CronJobScheduleUndo) precisely so every printable key reaches the
	// focused buffer: as bare 'y'/'u' they ate the letters out of "@daily",
	// "@hourly", "0 2 * * sun" and "UTC". A chord never carries Text, so
	// they now work from either buffer.
	var cmd tea.Cmd
	if m.tzFocused && m.tzEditable() {
		m.tzInput, cmd = m.tzInput.Update(msg)
	} else {
		m.scheduleInput, cmd = m.scheduleInput.Update(msg)
	}
	m.recomputeAll()
	return m, cmd
}

// pendingSchedule/pendingTimeZone are the buffers' current values, trimmed
// — runComparison's "new" side, and recomputeAll's own schedule/timezone
// inputs. Kept as methods (not locals repeated at every call site) so
// tickMsg's own comparison dispatch and recomputeAll agree on exactly what
// "the currently typed schedule" means.
func (m Model) pendingSchedule() string { return strings.TrimSpace(m.scheduleInput.Value()) }

// pendingTimeZone is the effective timezone the *typed* buffers describe —
// the tz buffer's own value when it's editable, otherwise whatever the
// object is already carrying (a read-only field never contributes an edit).
func (m Model) pendingTimeZone() string {
	if m.tzEditable() {
		return strings.TrimSpace(m.tzInput.Value())
	}
	if m.accepted.hasTimezone {
		return m.accepted.timezone
	}
	return ""
}

// hasPendingChange reports whether the typed buffers differ from the last
// server-accepted values — commitApply/Keybar's own "nothing to apply"
// gate, and recomputeAll's own comparisonDirty trigger.
func (m Model) hasPendingChange() bool {
	if m.pendingSchedule() != m.accepted.schedule {
		return true
	}
	if !m.tzEditable() {
		return false
	}
	tz := strings.TrimSpace(m.tzInput.Value())
	if m.accepted.hasTimezone {
		return tz != m.accepted.timezone
	}
	return tz != ""
}

// recomputeAll rebuilds every piece of derived state from the current
// buffers — task 8's "recompute parsing and next-five runs immediately on
// typed/pasted edits." The yearly WHAT CHANGES comparison is *not* run
// here: it's flagged via comparisonDirty and picked up by the next tick
// (Update's tickMsg case), which is what makes it asynchronous without
// needing a Cmd this method — called from inside a paste closure, which
// can't return one (tui.PasteTarget is a bare func(string)) — could ever
// propagate.
func (m *Model) recomputeAll() {
	schedule := m.pendingSchedule()
	_, m.parseErr = parseSchedule(schedule)
	m.fields = decodeScheduleFields(schedule)

	if m.tzEditable() {
		_, m.tzErr = validateTimeZone(strings.TrimSpace(m.tzInput.Value()))
	} else {
		m.tzErr = nil
	}

	switch {
	case m.parseErr != nil || m.tzErr != nil:
		m.nextRuns, m.nextErr = nil, nil
	default:
		m.nextRuns, m.nextErr = resources.NextCronRuns(schedule, m.pendingTimeZone(), m.now, 5)
	}

	m.editGen++
	m.comparison = nil
	m.comparing = false
	m.comparisonDirty = m.parseErr == nil && m.tzErr == nil && m.hasPendingChange()
	m.conflict = false
}

// commitApply executes the typed schedule/timezone through
// actions.Controller at TierNone — reversible in every context, PROD
// included (0.8.0 plan §3 decision 10: the dedicated editor is itself the
// intentionality gate a bare inline buffer would have needed a PROD escalation
// for). timezone follows CronJobScheduleEdit's own three-state contract:
// nil when the tz buffer isn't editable (never touch a field this screen
// can't validate), a pointer to "" to clear an existing value down to
// empty, a pointer to the typed name to set/change it — never sent at all
// when it already matches what's accepted, so an untouched-but-editable tz
// field doesn't turn into a needless PATCH of its own value onto itself.
func (m *Model) commitApply() tea.Cmd {
	schedule := m.pendingSchedule()
	var tzEdit *string
	if m.tzEditable() {
		tz := strings.TrimSpace(m.tzInput.Value())
		switch {
		case tz == "" && m.accepted.hasTimezone:
			empty := ""
			tzEdit = &empty
		case tz != "" && (!m.accepted.hasTimezone || tz != m.accepted.timezone):
			t := tz
			tzEdit = &t
		}
	}
	before := m.accepted
	m.pendingCommit = &scheduleCommit{schedule: schedule, timezone: tzEdit, resourceVersion: m.accepted.resourceVersion, before: before}
	m.message, m.lastError = "", ""
	return m.actions.Begin(actions.Request{Verb: verbs.CronJobSetSchedule, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cronjob-set-schedule-" + m.namespace + "/" + m.name,
		Label: fmt.Sprintf("Set %s's schedule?", m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: m.name, Namespace: m.namespace,
			Verb: "cronjob-set-schedule", IsMutating: true,
			Schedule: schedule, CronJobTimeZone: tzEdit, CronJobResourceVersion: m.accepted.resourceVersion,
		},
	})
}

// commitUndo re-applies m.previous — the pre-commit accepted pair
// handleResult stashed on the last successful apply — as an ordinary
// reversible mutation through the same registry/controller (task 12: "Undo
// is another normal mutation," not a client-side buffer revert). The
// resourceVersion precondition is the *current* accepted one (what the
// server actually has right now), never the stale one m.previous itself
// carries — undoing has to survive exactly the same optimistic-concurrency
// check a fresh apply does.
func (m *Model) commitUndo() tea.Cmd {
	prev := *m.previous
	var tzEdit *string
	switch {
	case !prev.hasTimezone && m.accepted.hasTimezone:
		empty := ""
		tzEdit = &empty
	case prev.hasTimezone:
		tz := prev.timezone
		tzEdit = &tz
	}
	m.scheduleInput.SetValue(prev.schedule)
	m.scheduleInput.CursorEnd()
	if prev.hasTimezone {
		m.tzInput.SetValue(prev.timezone)
		m.tzInput.CursorEnd()
	} else {
		m.tzInput.SetValue("")
	}
	before := m.accepted
	m.pendingCommit = &scheduleCommit{schedule: prev.schedule, timezone: tzEdit, resourceVersion: m.accepted.resourceVersion, isUndo: true, before: before}
	m.message, m.lastError = "", ""
	return m.actions.Begin(actions.Request{Verb: verbs.CronJobScheduleUndo, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cronjob-schedule-undo-" + m.namespace + "/" + m.name,
		Label: fmt.Sprintf("Undo %s's schedule change?", m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: m.name, Namespace: m.namespace,
			Verb: "cronjob-set-schedule", IsMutating: true,
			Schedule: prev.schedule, CronJobTimeZone: tzEdit, CronJobResourceVersion: m.accepted.resourceVersion,
		},
	})
}

// handleResult applies an apply/undo's outcome — "confirm → execute →
// refresh → show result → remain on screen" (CLAUDE.md), never leaving the
// screen. On success it trusts the mutation response's own accepted
// schedule/timezone/resourceVersion (task 10 — "must not declare refresh/
// undo ready from an immediate stale informer-cache read"), not a fresh
// ListRaw. On a Conflict it leaves the buffer untouched and says so
// explicitly rather than overwriting it (task 11); every other failure
// just surfaces the server's own error the same way.
func (m *Model) handleResult(msg actions.ResultMsg) tea.Cmd {
	pc := m.pendingCommit
	m.pendingCommit = nil
	if msg.Err != nil {
		m.lastError = msg.Err.Error()
		m.message = ""
		m.conflict = apierrors.IsConflict(msg.Err)
		if m.conflict {
			m.lastError = "the object changed on the server since this screen loaded — press ↵ again to retry against the latest version"
		}
		return nil
	}
	m.lastError = ""
	m.conflict = false
	if pc == nil || msg.CronJobSchedule == nil {
		return nil
	}
	result := *msg.CronJobSchedule
	m.accepted = acceptedPair{
		schedule:        result.Schedule,
		timezone:        result.TimeZone,
		hasTimezone:     result.TimeZone != "",
		resourceVersion: result.ResourceVersion,
		generation:      m.accepted.generation,
	}
	if pc.isUndo {
		m.previous = nil
		m.message = "reverted schedule to " + result.Schedule
	} else {
		before := pc.before
		m.previous = &before
		m.message = "updated schedule to " + result.Schedule
	}
	m.editGen++
	m.comparison, m.comparing, m.comparisonDirty = nil, false, false
	return nil
}

// editResultMsg carries ctrl-y's kubectl-edit exit — mirrors browse's own
// editResultMsg (edit.go)/poddetail's/nodedetail's, duplicated per the
// repo's package-local-seam convention.
type editResultMsg struct{ err error }

// editCmd hands the tty to `kubectl edit cronjob/name` (task 14 — "uses the
// existing kubectl-edit subprocess machinery") — always immediate, no
// confirmation of any tier: CronJobScheduleFullEdit's own doc comment says
// why ("no Tier because kubectl owns the session once kute suspends"),
// unlike browse's global 'E' which stages a PROD y/N first.
func editCmd(target kube.CommandTarget, namespace, name string) tea.Cmd {
	spec := kube.EditSpec(target, kube.KindCronJob, namespace, name)
	return tea.ExecProcess(spec, func(err error) tea.Msg {
		return editResultMsg{err: err}
	})
}

// tickMsg drives the UI-only clock (task 10 in §4.5's shared contract):
// relative ETA text and the comparisonDirty→async-comparison handoff,
// never a lister/mutator/discovery call.
type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// whatChangesMsg carries compareFiringSets' result back from
// runComparison's background command, tagged with the edit generation it
// was computed for — Update's whatChangesMsg case drops it outright when
// gen no longer matches m.editGen (task 7: "a stale result from an earlier
// keystroke is never shown").
type whatChangesMsg struct {
	gen    int
	result scheduleComparison
}

// runComparison runs compareFiringSets off the update loop — the "yearly"
// half of task 7, kept out of recomputeAll (which runs synchronously on
// every keystroke) specifically so a fast typist never blocks on it.
func runComparison(gen int, accepted acceptedPair, newSchedule, newTZ string, now time.Time) tea.Cmd {
	oldTZ := ""
	if accepted.hasTimezone {
		oldTZ = accepted.timezone
	}
	oldSchedule, oldTZCopy := accepted.schedule, oldTZ
	newScheduleCopy, newTZCopy := newSchedule, newTZ
	return func() tea.Msg {
		result := compareFiringSets(oldSchedule, oldTZCopy, newScheduleCopy, newTZCopy, now)
		return whatChangesMsg{gen: gen, result: result}
	}
}

// willRunCommand renders the exact kubectl-equivalent command ctrl+t copies
// and the will-run band shows — CronJobSetScheduleCommandString's own
// three-state timezone contract (nil/&""/&name) built from whichever
// pending-commit values are current: an in-flight commit's own values,
// otherwise whatever's currently typed.
func (m Model) willRunCommand() string {
	schedule, tz := m.pendingSchedule(), m.pendingTimeZoneEdit()
	if pc := m.pendingCommit; pc != nil {
		schedule, tz = pc.schedule, pc.timezone
	}
	return kube.CronJobSetScheduleCommandString(m.namespace, m.name, schedule, tz)
}

// pendingTimeZoneEdit mirrors commitApply's own tzEdit derivation — the
// three-state value a live (not-yet-committed) buffer state would send,
// for willRunCommand's own preview while nothing is pending yet.
func (m Model) pendingTimeZoneEdit() *string {
	if !m.tzEditable() {
		return nil
	}
	tz := strings.TrimSpace(m.tzInput.Value())
	switch {
	case tz == "" && m.accepted.hasTimezone:
		empty := ""
		return &empty
	case tz != "" && (!m.accepted.hasTimezone || tz != m.accepted.timezone):
		t := tz
		return &t
	default:
		return nil
	}
}
