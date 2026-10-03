// Package jobactions is the one copy of the staged Job/CronJob action
// preflights two screens each reach: §37c's create-vs-replace rerun (the
// Jobs list in browse and the jobattempts ledger) and §36b/§36c's CronJob
// run-now and suspend/resume (the CronJobs list in browse and cronjobdetail).
// Task packages can't import one another, so this used to be two hand-kept
// copies per verb, and they drifted (review 06 M6/L6).
//
// It owns what must be identical wherever the verb is pressed: the staged
// state, how its keys move it (from the verbs registry), the will-run text
// and keybar, the actions.Request/TaskAction a commit hands to
// Controller.Begin, and how a result reads. It owns no I/O and reads no
// clock — every staging call takes the caller's own now — and it never
// calls Begin itself: the screen does, so its controller stays the one
// TestEveryControllerScreenRoutesConfirmKeys checks. Where a preflight
// renders (a keybar, browse's behavior-strip banner) stays each screen's
// own.
package jobactions

import (
	"github.com/kute-dev/kute/internal/kube"
)

// Outcome is what a key press means to an open preflight.
type Outcome int

const (
	// Ignored: the key means nothing to this preflight.
	Ignored Outcome = iota
	// Stay: the key was handled and the preflight stays open.
	Stay
	// Cancel: close the preflight without acting.
	Cancel
	// Commit: close the preflight and Begin its action.
	Commit
)

// IdentityReader is the connect-time SelfSubjectReview seam — satisfied by
// *kube.Cluster and *fake.Cluster's ReviewedIdentity, and by each screen's
// own identically-shaped interface.
type IdentityReader interface {
	ReviewedIdentity() (kube.ReviewedIdentity, bool)
}

// ActingUser is who an action taken now is attributed to: the API server's
// reviewed username (read at the moment of the action — it lands after a
// screen is built and is replaced on every context switch), else the
// kubeconfig user name, else "".
func ActingUser(identity IdentityReader, currentUser string) string {
	if identity != nil {
		if id, ok := identity.ReviewedIdentity(); ok && id.Username != "" {
			return id.Username
		}
	}
	return currentUser
}

// Creator is the identity §36b's run-now and §37c's rerun stamp into
// kube.AnnotationTriggeredBy: ActingUser, falling back to a generic,
// still-truthful "kute" rather than an empty annotation value.
func Creator(identity IdentityReader, currentUser string) string {
	if who := ActingUser(identity, currentUser); who != "" {
		return who
	}
	return "kute"
}
