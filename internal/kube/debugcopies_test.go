package kube

import (
	"sync"
	"testing"
)

func TestDebugCopyRegistryAddContainsRemove(t *testing.T) {
	t.Parallel()
	r := NewDebugCopyRegistry()
	if r.Contains("dev", "default", "worker-debug") {
		t.Fatalf("empty registry must not contain anything")
	}
	r.Add("dev", "default", "worker-debug")
	if !r.Contains("dev", "default", "worker-debug") {
		t.Fatalf("expected Contains true after Add")
	}
	if r.Contains("dev", "default", "other-debug") {
		t.Fatalf("Contains must not match an unrelated name")
	}
	if r.Contains("dev", "prod", "worker-debug") {
		t.Fatalf("Contains must be namespace-scoped")
	}
	if r.Contains("staging", "default", "worker-debug") {
		t.Fatalf("Contains must be context-scoped: a same-named pod on another cluster is not a copy")
	}
	r.Remove("dev", "default", "worker-debug")
	if r.Contains("dev", "default", "worker-debug") {
		t.Fatalf("expected Contains false after Remove")
	}
}

func TestDebugCopyRegistryZeroValueUsable(t *testing.T) {
	t.Parallel()
	var r DebugCopyRegistry
	r.Add("dev", "default", "x")
	if !r.Contains("dev", "default", "x") {
		t.Fatalf("expected a zero-value DebugCopyRegistry to work after Add")
	}
}

func TestDebugCopyRegistryConcurrentAccess(t *testing.T) {
	t.Parallel()
	r := NewDebugCopyRegistry()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Add("dev", "default", "copy")
			r.Contains("dev", "default", "copy")
			r.Remove("dev", "default", "copy")
		}(i)
	}
	wg.Wait()
}
