package kube

import (
	"cmp"
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Event is the projected view of a corev1.Event this app renders (9a/9b).
type Event struct {
	Type    string // "Normal" or "Warning"
	Reason  string
	Message string
	Object  string // "Kind/Name", e.g. "Pod/nva-worker-9k2ss"
	// ObjectAPIVersion is involvedObject.apiVersion — what makes Object's
	// bare Kind resolvable to a registry kind (ObjectKind): "HelmRelease"
	// alone names §18a's Helm kind, not the Flux object that emitted it.
	ObjectAPIVersion string
	// ObjectUID is involvedObject.uid: what tells a StatefulSet pod apart
	// from the same-named predecessor it replaced (see ForObjectUID).
	ObjectUID string
	Namespace string
	Count     int32
	FirstSeen time.Time
	LastSeen  time.Time
	// Annotations is the Event object's own metadata.annotations. Flux's
	// controllers put the reconciled revision there
	// (kustomize.toolkit.fluxcd.io/revision), which is the exact,
	// unparsed source for §32a's revision rows — the message is prose,
	// the annotation is data.
	Annotations map[string]string
}

// EventGroup is one deduped row for the events screen (9b): every
// underlying Event object sharing the same reason+object+message folded
// into one row, with the summed count and most recent timestamp.
type EventGroup struct {
	Type             string
	Reason           string
	Message          string
	Object           string
	ObjectAPIVersion string // see Event.ObjectAPIVersion
	Namespace        string
	Count            int32
	LastSeen         time.Time
}

// ObjectEvents returns every cached Event whose involvedObject matches
// kind/name in namespace, newest first.
func (c *Cluster) ObjectEvents(ctx context.Context, namespace string, kind ResourceKind, name string) ([]Event, error) {
	objs, err := c.ListRaw(ctx, KindEvent, namespace)
	if err != nil {
		return nil, err
	}
	events := make([]*corev1.Event, 0, len(objs))
	for _, obj := range objs {
		if ev, ok := obj.(*corev1.Event); ok {
			events = append(events, ev)
		}
	}
	return filterEventsByInvolvedObject(events, kind.APIKind(), name), nil
}

// NamespaceEvents returns every cached Event in namespace ("" = every
// namespace, mirroring ListRaw), newest first — 9b's namespace-scoped view
// (browse's 'e'), unlike ObjectEvents unfiltered by involvedObject.
func (c *Cluster) NamespaceEvents(ctx context.Context, namespace string) ([]Event, error) {
	objs, err := c.ListRaw(ctx, KindEvent, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(objs))
	for _, obj := range objs {
		if ev, ok := obj.(*corev1.Event); ok {
			out = append(out, EventFromObject(ev))
		}
	}
	SortEventsNewestFirst(out)
	return out, nil
}

// ForObjectUID drops the events recorded against a different incarnation of
// an object than uid — ObjectEvents matches on kind and name, so a
// StatefulSet pod recreated under the same name would otherwise inherit its
// predecessor's events. An event with no involvedObject.uid is kept, and an
// empty uid (the caller doesn't know it) keeps everything.
func ForObjectUID(events []Event, uid string) []Event {
	if uid == "" {
		return events
	}
	return slices.DeleteFunc(slices.Clone(events), func(e Event) bool {
		return e.ObjectUID != "" && e.ObjectUID != uid
	})
}

// filterEventsByInvolvedObject projects and filters raw Events by
// involvedObject kind/name, newest first. Factored out of ObjectEvents so
// the filter/projection logic is unit-testable without an informer.
func filterEventsByInvolvedObject(events []*corev1.Event, kind, name string) []Event {
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		if ev.InvolvedObject.Kind != kind || ev.InvolvedObject.Name != name {
			continue
		}
		out = append(out, EventFromObject(ev))
	}
	SortEventsNewestFirst(out)
	return out
}

// SortEventsNewestFirst orders by LastSeen descending, the informer cache's
// list order (map-backed, so not deterministic across reloads) otherwise
// leaving ties to an unstable sort's ordering — which visibly "glitched"
// 9b, flipping two same-second events (e.g. a container's Pulling/Started
// pair) back and forth on every reload. FirstSeen desc as a tiebreak both
// fixes that and, for causally-linked events like Pulling→Started, surfaces
// the one that actually happened later; Reason and Message are the final,
// fully deterministic fallbacks.
func SortEventsNewestFirst(events []Event) {
	slices.SortFunc(events, func(a, b Event) int {
		return cmp.Or(
			b.LastSeen.Compare(a.LastSeen),
			b.FirstSeen.Compare(a.FirstSeen),
			cmp.Compare(a.Reason, b.Reason),
			cmp.Compare(a.Message, b.Message),
		)
	})
}

// EventFromObject projects a corev1.Event — the one projection both the
// real Cluster and kube/fake use, so the demo and tests see the same
// Series/EventTime/CreationTimestamp fallbacks a live cluster does.
func EventFromObject(ev *corev1.Event) Event {
	last := ev.LastTimestamp.Time
	if last.IsZero() {
		last = ev.EventTime.Time
	}
	if last.IsZero() {
		last = ev.CreationTimestamp.Time
	}
	first := ev.FirstTimestamp.Time
	if first.IsZero() {
		first = last
	}
	count := ev.Count
	// events.k8s.io/v1-native events (emitted by kube-scheduler and most
	// controllers on modern Kubernetes) never populate the legacy
	// LastTimestamp/Count fields at all — repeats instead update Series,
	// leaving LastTimestamp/Count at their zero value for the object's
	// entire lifetime. Without this, "last" above falls back to EventTime
	// (the *first* occurrence), which can be hours old for a long-running
	// series — silently expelling an actively-recurring warning from every
	// time-windowed view (9b's own "last hour" default, poddetail/
	// nodedetail's EVENTS grid, the 16a/16b timeline) even though it's
	// still happening right now. Series.LastObservedTime/Count are the
	// authoritative "last seen"/"how many times" for these events, so they
	// take priority over whatever the legacy-field fallback above computed.
	if ev.Series != nil {
		if !ev.Series.LastObservedTime.IsZero() {
			last = ev.Series.LastObservedTime.Time
		}
		if ev.Series.Count > 0 {
			count = ev.Series.Count
		}
	}
	if count == 0 {
		count = 1
	}
	return Event{
		Type:             ev.Type,
		Reason:           ev.Reason,
		Message:          ev.Message,
		Object:           ev.InvolvedObject.Kind + "/" + ev.InvolvedObject.Name,
		ObjectAPIVersion: ev.InvolvedObject.APIVersion,
		ObjectUID:        string(ev.InvolvedObject.UID),
		Namespace:        ev.Namespace,
		Count:            count,
		FirstSeen:        first,
		LastSeen:         last,
		Annotations:      ev.Annotations,
	}
}

// DedupeEvents folds true repeats sharing reason+object+message into one
// EventGroup, summing Count and keeping the latest LastSeen. Message is part
// of the identity because one object can emit several materially different
// events under a generic reason such as Failed: the detailed image-pull
// error, ErrImagePull, and ImagePullBackOff must all remain visible.
// Namespace is part of the key even though Object ("Kind/Name")
// doesn't carry it — 9b's all-namespaces mode (browse's 'e' with
// namespace == "", mirroring 6b) would otherwise fold together two
// same-named, same-kind objects with the same reason in different
// namespaces (e.g. "FailedScheduling" on "Pod/cache-0" in both
// shop-checkout and shop-payments) into one wrongly-merged row. Results are
// ordered newest-first; screens partition warnings-first or otherwise
// re-sort on top of this base order.
func DedupeEvents(events []Event) []EventGroup {
	type key struct{ reason, namespace, object, apiVersion, message string }
	groups := make(map[key]*EventGroup, len(events))
	order := make([]key, 0, len(events))

	for _, e := range events {
		k := key{e.Reason, e.Namespace, e.Object, e.ObjectAPIVersion, e.Message}
		g, ok := groups[k]
		if !ok {
			g = &EventGroup{Reason: e.Reason, Message: e.Message, Object: e.Object, ObjectAPIVersion: e.ObjectAPIVersion, Namespace: e.Namespace}
			groups[k] = g
			order = append(order, k)
		}
		g.Count += e.Count
		if e.LastSeen.After(g.LastSeen) {
			g.LastSeen = e.LastSeen
			g.Type = e.Type
			g.Message = e.Message
		}
	}

	out := make([]EventGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	// LastSeen-tied groups need a deterministic tiebreak too, same reasoning
	// as SortEventsNewestFirst — EventGroup has no FirstSeen to fall back on
	// (it folds many occurrences), so Reason/Object is the best available.
	slices.SortFunc(out, func(a, b EventGroup) int {
		return cmp.Or(
			b.LastSeen.Compare(a.LastSeen),
			cmp.Compare(a.Reason, b.Reason),
			cmp.Compare(a.Object, b.Object),
			cmp.Compare(a.Message, b.Message),
		)
	})
	return out
}
