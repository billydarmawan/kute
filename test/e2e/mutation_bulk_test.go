//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestBulkDeleteMarkedSetCommits drives 20a end to end: filter, mark the
// filtered set with '*', and delete all of it behind one confirm.
//
// Bulk delete is the app's widest write — one keypress, N objects, no
// per-object confirmation — and every part of it that can go wrong only goes
// wrong against a real API server: the marked set is keyed by
// namespace+name while the deletes go out one request at a time, the list
// reloads from a watch rather than from the command's own result, and the
// cursor has to land somewhere sane when the rows under it stop existing.
//
// ConfigMaps rather than Pods on purpose: they delete immediately, with no
// grace period to wait out and no controller recreating them underneath the
// assertions.
func TestBulkDeleteMarkedSetCommits(t *testing.T) {
	RequireCluster(t)
	const prefix = "phase3-bulk-"
	names := []string{prefix + "a", prefix + "b", prefix + "c"}
	for _, name := range names {
		createConfigMap(t, name, map[string]string{"value": "bulk"})
	}
	// A neighbour outside the marked set: something has to remain for the
	// selection to clamp onto, and its survival is what proves '*' marked
	// the *filtered* rows rather than the whole list.
	createConfigMap(t, "phase3-bulk-keep", map[string]string{"value": "keep"})
	client := mutationClient(t)

	a := Launch(t)
	a.WaitFor("api-", Connect)
	a.gotoKind(t, "configmaps", "ConfigMaps")
	a.WaitForAll(Settle, names[0], names[1], names[2])

	// filter-then-mark is 20a's whole grammar — there is no range-mark
	// chord, so the filter is how a set gets selected at all.
	a.filterTo(t, prefix)
	a.Press("*")

	// The health strip's first slot becomes the marked count. Four, not
	// three: phase3-bulk-keep matches the same prefix, which is exactly why
	// the next step narrows the set by hand instead of trusting '*' alone.
	a.waitForMarkCount(t, 4)

	// Unmark the keeper: space toggles the cursor row, so putting the cursor
	// on it and pressing space takes it back out of the set.
	a.selectRow(t, "phase3-bulk-keep")
	a.Press("space")
	a.waitForMarkCount(t, 3)

	a.Press("D")
	a.WaitFor("CONFIRM", Settle)
	// §20a's will-run line names every marked object in one kubectl command,
	// which is the only place the user can see what the set actually is.
	for _, name := range names {
		a.WaitForWrapped(name, Settle)
	}
	a.WaitForWrapped("kubectl delete configmap", Settle)
	a.Press("y")

	for _, name := range names {
		waitForDeleted(t, "bulk-deleted configmap "+name, func(ctx context.Context) error {
			_, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, name, metav1.GetOptions{})
			return err
		})
		a.WaitGone(name, Settle)
	}

	// The marked set is cleared by a clean bulk delete — a leftover count
	// would aim the *next* destructive key at objects that no longer exist.
	// Both places it shows: the health strip's "▪ N marked" slot and the
	// mode pill, which 20a hands over to the count while anything is marked.
	a.WaitGone("marked", Settle)
	a.WaitGone("MARKED", Settle)
	// The unmarked row survived, and the cursor clamped onto it rather than
	// off the end of a list that just lost three rows.
	a.waitForSelectedRow(t, "phase3-bulk-keep")
}

// TestBulkDeletePartialFailureKeepsMarks injects a 403 on exactly one of the
// marked objects' DELETEs.
//
// The controller's bulk execution records per-row errors rather than
// stopping at the first, so a partial failure is a real state the screen has to report: some
// objects gone, one still there, and the marked set retained rather than
// silently cleared. Nothing below the UI can produce that — the fake
// clientset would have to be taught to fail one name — and a bulk verb that
// quietly drops the failures is indistinguishable from one that worked.
func TestBulkDeletePartialFailureKeepsMarks(t *testing.T) {
	RequireCluster(t)
	const prefix = "phase3-bulkfail-"
	const refused, deleted = prefix + "refused", prefix + "deleted"
	createConfigMap(t, refused, map[string]string{"value": "stays"})
	createConfigMap(t, deleted, map[string]string{"value": "goes"})
	client := mutationClient(t)

	proxy := NewAPIProxy(t, KubeconfigPath())
	a := Launch(t, WithAPIProxy(proxy))
	a.WaitFor("api-", Connect)
	a.gotoKind(t, "configmaps", "ConfigMaps")
	a.WaitForAll(Settle, refused, deleted)

	a.filterTo(t, prefix)
	a.Press("*")
	a.waitForMarkCount(t, 2)

	// Path is a prefix matcher, so this selects one object's DELETE and
	// nothing else — the other marked row's delete goes through untouched,
	// which is the whole point.
	proxy.FailNextStatus(RequestMatcher{
		Method: "DELETE",
		Path:   fmt.Sprintf("/api/v1/namespaces/%s/configmaps/%s", Namespace, refused),
	}, 403, "fault injected by e2e proxy", 1)

	a.Press("D")
	a.WaitFor("CONFIRM", Settle)
	a.Press("y")

	// The delete that was allowed to land, landed.
	waitForDeleted(t, "the permitted configmap", func(ctx context.Context) error {
		_, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, deleted, metav1.GetOptions{})
		return err
	})

	// The failure is reported rather than swallowed, and the marked set
	// survives it so the user can retry the set they chose.
	a.WaitForWrapped("1 of 2 targets failed", Settle)
	a.WaitFor("marked", Settle)

	// And the refused object is still there — both on the server and on the
	// screen, which reloads from the informer cache rather than from the
	// command's own optimistic result.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, refused, metav1.GetOptions{}); err != nil {
		t.Fatalf("the refused configmap is gone from the server — the injected 403 did not hold: %v", err)
	}
	a.WaitFor(refused, Settle)
}

// waitForMarkCount waits until the health strip's marked slot reads exactly
// want.
//
// One line, not two WaitFors: "▪" and the number are only meaningful
// together, and the strip renders the count beside the glyph — a frame where
// the glyph is present and the number is still the previous one is exactly
// the race this closes.
func (a *App) waitForMarkCount(t *testing.T, want int) {
	t.Helper()
	frame, ok := a.poll(func(f string) bool {
		for _, line := range strings.Split(f, "\n") {
			if !strings.Contains(line, "▪") {
				continue
			}
			fields := strings.Fields(line)
			for i, field := range fields {
				if field == "▪" && i+2 < len(fields) && fields[i+1] == strconv.Itoa(want) && fields[i+2] == "marked" {
					return true
				}
			}
		}
		return false
	}, Settle)
	if !ok {
		t.Fatalf("the marked count never reached %d:\n%s", want, frame)
	}
}
