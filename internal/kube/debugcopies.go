package kube

import "sync"

// DebugCopyRegistry tracks pods §41c's "copy pod" debug mode has created
// this session — kute's own client-side record, since the Kubernetes API
// carries no marker distinguishing a debug copy from any other pod.
// Session-scoped, not persisted: built once at the composition root
// (mirrors kube.ForwardManager's own placement/lifetime — tui.Session's
// DebugCopies field), survives context switches — so every entry is keyed
// by the kubeconfig context it was created in, and a same-named pod on
// another cluster is never tagged — and its entries never
// expire on their own — only an explicit Remove (the CLEAN UP prompt's
// ctrl-d) or the end of the process clears one, the same "no attempt to
// track external deletion" realism ForwardManager applies to a forward's
// own target.
type DebugCopyRegistry struct {
	mu  sync.Mutex
	set map[string]bool
}

// NewDebugCopyRegistry returns an empty registry.
func NewDebugCopyRegistry() *DebugCopyRegistry {
	return &DebugCopyRegistry{set: map[string]bool{}}
}

func debugCopyKey(contextName, namespace, name string) string {
	return contextName + "\x00" + namespace + "/" + name
}

// Add records namespace/name in context as a debug copy kute created.
func (r *DebugCopyRegistry) Add(contextName, namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.set == nil {
		r.set = map[string]bool{}
	}
	r.set[debugCopyKey(contextName, namespace, name)] = true
}

// Remove clears namespace/name in context — called once the copy is
// deleted (or the user tells kute to forget it).
func (r *DebugCopyRegistry) Remove(contextName, namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.set, debugCopyKey(contextName, namespace, name))
}

// Contains reports whether namespace/name in context is a tracked debug
// copy — the pods-table row-decoration check (§41c's ⚑ tag).
func (r *DebugCopyRegistry) Contains(contextName, namespace, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set[debugCopyKey(contextName, namespace, name)]
}
