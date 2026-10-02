package fake

import "github.com/kute-dev/kute/internal/kube"

// DiscoveredKinds returns the seeded discovery cache — the fake counterpart
// of *kube.Cluster.DiscoveredKinds, feeding resources.BuildDiscoveredRegistry
// the same way in --demo as against a real cluster.
func (c *Cluster) DiscoveredKinds() []kube.DiscoveredKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]kube.DiscoveredKind(nil), c.discovered...)
}

// SeedDiscovered registers dk in the fake discovery cache (fixtures.go's
// demo cert-manager set, and any test that wants a discovered kind without
// standing up a real cluster).
func (c *Cluster) SeedDiscovered(dk kube.DiscoveredKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The same collision resolution the real discovery pass applies, so a
	// test seeding two same-Kind groups gets the keys a cluster would give.
	c.discovered = kube.AssignRegistryKinds(append(c.discovered, dk))
}

// CountInstances is the fake counterpart of *kube.Cluster.CountInstances —
// the 14b CRDs list's live COUNT column, reading straight off the seeded
// objects map (fully generic already, same as ListRaw).
func (c *Cluster) CountInstances(kind kube.ResourceKind) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.objects[kind]), true
}
