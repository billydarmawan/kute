package cronjobschedule

import (
	"time"

	tea "charm.land/bubbletea/v2"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// reloadDebounce coalesces bursts of watch events into one reload — same
// value as cronjobdetail/jobattempts' own reloadDebounce, duplicated per the
// repo's package-local-seam convention.
const reloadDebounce = 250 * time.Millisecond

// reloadDueMsg fires scheduleReload's tick; epoch drops a superseded one.
type reloadDueMsg struct{ epoch int }

// scheduleReload arranges one reloadDueMsg reloadDebounce from now.
func (m Model) scheduleReload(epoch int) tea.Cmd {
	return tea.Tick(reloadDebounce, func(time.Time) tea.Msg {
		return reloadDueMsg{epoch: epoch}
	})
}

// findCronJob resolves name against objs (a KindCronJob ListRaw reply),
// returning a cronJobSnapshot of just the fields this screen needs.
func findCronJob(objs []runtime.Object, name string) (*cronJobSnapshot, bool) {
	for _, obj := range objs {
		cj, ok := obj.(*batchv1.CronJob)
		if !ok || cj.Name != name {
			continue
		}
		snap := &cronJobSnapshot{
			schedule:        cj.Spec.Schedule,
			resourceVersion: cj.ResourceVersion,
			generation:      cj.Generation,
		}
		if cj.Spec.TimeZone != nil {
			snap.hasTimezone = true
			snap.timezone = *cj.Spec.TimeZone
		}
		return snap, true
	}
	return nil, false
}

// timeZoneCapability reads m.capabilities, defaulting to Unknown when it's
// nil (a Config built without one, or a task test that doesn't care) — the
// same safe default a real cluster carries before its first probe.
func (m Model) timeZoneCapability() kube.TimeZoneCapability {
	if m.capabilities == nil {
		return kube.TimeZoneCapabilityUnknown
	}
	return m.capabilities.TimeZoneCapability()
}

// tzEditable reports whether the timezone buffer accepts focus/typing
// (§3.8): a Supported capability always allows it; otherwise only when the
// object's own spec.timeZone is already populated, which is direct
// evidence this particular object's field works regardless of what the
// version band alone would suggest (§3.8 point 4). An Unknown/Unsupported
// capability with no existing value keeps the field visible but read-only —
// never a fabricated cluster version, never a write an older server might
// silently prune.
func (m Model) tzEditable() bool {
	return m.timeZoneCapability() == kube.TimeZoneCapabilitySupported || m.accepted.hasTimezone
}
