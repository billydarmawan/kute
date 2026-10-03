package kube

// Test-only lock-taking wrappers around the Locked forms production calls
// from inside its own critical sections (Start, ensureKind, recordWatchError).

// registerWatches is registerWatchesLocked for a test that doesn't hold c.mu.
func (c *Cluster) registerWatches(kinds ...ResourceKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registerWatchesLocked(kinds...)
}

// noteWatchError is noteWatchErrorLocked behind recordWatchError's own
// generation check, without the connection-health update.
func (c *Cluster) noteWatchError(gen int, kind ResourceKind, namespace string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != gen {
		return
	}
	c.noteWatchErrorLocked(kind, namespace, err)
}

// markKindFailed is markKindFailedLocked behind a generation check.
func (c *Cluster) markKindFailed(gen int, kind ResourceKind, namespace string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != gen {
		return
	}
	c.markKindFailedLocked(kind, namespace, err)
}

// allStartedKindsSynced is allStartedKindsReadyLocked for a test that doesn't
// hold c.mu.
func (c *Cluster) allStartedKindsSynced() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allStartedKindsReadyLocked()
}
