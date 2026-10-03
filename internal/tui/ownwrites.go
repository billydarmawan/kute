package tui

import "slices"

// OwnWrites remembers the resourceVersion an editor's own successful write
// produced, so the editor's next write can precondition on it while the
// informer cache still holds the object from before that write. Without it,
// a buffer opened in the window between a save and the watch event that
// delivers it reads the pre-write resourceVersion from the cache and its
// first commit is a spurious 409 against the user's own change (27a/27b).
//
// resourceVersions are opaque (never compared numerically), so staleness is
// decided by equality only: a cached resourceVersion that is one this
// editor's writes have already superseded means the cache hasn't caught up,
// and the last written one is used instead; any other resourceVersion means
// the cache has moved on (to this write or past it) and is trusted again.
// The zero value is ready to use.
type OwnWrites struct {
	latest     string
	superseded []string
}

// Wrote records a successful write that was preconditioned on before and
// that the server answered with after. A chain of writes made before the
// cache catches up keeps every superseded resourceVersion; a write from
// anything other than the last one written starts a new chain.
func (w *OwnWrites) Wrote(before, after string) {
	if after == "" {
		return
	}
	if w.latest == "" || before != w.latest {
		w.superseded = nil
	}
	if before != "" {
		// Clipped so a copied model (Bubble Tea models are values) never
		// shares the appended-to array.
		w.superseded = append(slices.Clip(w.superseded), before)
	}
	w.latest = after
}

// Resolve returns the resourceVersion to treat as current given what the
// cache reports: the last written one while cached is one of the versions
// it superseded, cached itself otherwise (and the record is dropped).
func (w *OwnWrites) Resolve(cached string) string {
	if w.latest != "" && slices.Contains(w.superseded, cached) {
		return w.latest
	}
	w.Forget()
	return cached
}

// Latest is the last written resourceVersion still being remembered, or "".
func (w *OwnWrites) Latest() string { return w.latest }

// Forget drops the record, so the next Resolve trusts the cache — after a
// conflict, which says the server has moved past every version known here.
func (w *OwnWrites) Forget() {
	w.latest = ""
	w.superseded = nil
}
