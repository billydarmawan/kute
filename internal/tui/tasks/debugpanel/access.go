package debugpanel

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
)

// AccessReviewer answers one authoritative current-user access question.
// The real cluster uses SelfSubjectAccessReview; the fake resolves against
// its seeded authorization graph.
type AccessReviewer interface {
	CanI(ctx context.Context, query kube.WhoCanQuery) (kube.AccessReviewResult, error)
}

// OpenWhoCanFunc pushes the cache-local binding explorer for a denied live
// review. The server verdict controls launch; WHO CAN explains the RBAC graph.
type OpenWhoCanFunc func(verb, resource, namespace string, width, height int) (tea.Model, tea.Cmd)

const accessReviewTimeout = 5 * time.Second

type accessState uint8

const (
	accessUnchecked accessState = iota
	accessChecking
	accessAllowed
	accessDenied
)

type accessReviewedMsg struct {
	gen    int
	query  kube.WhoCanQuery
	result kube.AccessReviewResult
	err    error
}

func (m Model) accessQuery() kube.WhoCanQuery {
	resource := kube.DebugAttachResource
	if m.mode == modeCopy {
		resource = kube.DebugCopyResource
	}
	return kube.WhoCanQuery{Verb: "create", Resource: resource, Namespace: m.namespace}
}

func (m *Model) startAccessReview() tea.Cmd {
	if m.tgt != targetPod || m.access == nil {
		m.accessState = accessAllowed
		return nil
	}
	m.accessGen++
	m.accessState = accessChecking
	m.accessResult = kube.AccessReviewResult{}
	m.accessErr = nil
	return m.accessReviewCmd(m.accessGen, m.accessQuery())
}

func (m Model) accessReviewCmd(gen int, query kube.WhoCanQuery) tea.Cmd {
	reviewer := m.access
	if m.session == nil {
		return func() tea.Msg {
			return accessReviewedMsg{gen: gen, query: query, err: errors.New("cluster session is unavailable")}
		}
	}
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, accessReviewTimeout)
		defer cancel()
		result, err := reviewer.CanI(ctx, query)
		return accessReviewedMsg{gen: gen, query: query, result: result, err: err}
	}
}

func (m *Model) handleAccessReviewed(msg accessReviewedMsg) {
	if msg.gen != m.accessGen || msg.query != m.accessQuery() {
		return
	}
	m.accessResult = msg.result
	m.accessErr = msg.err
	switch {
	case msg.err != nil:
		// A failed review must not recreate the old false-denial trap. The
		// launch remains available and the real API server is the backstop.
		m.accessState = accessAllowed
	case msg.result.Allowed:
		m.accessState = accessAllowed
	case msg.result.EvaluationError != "":
		// The server couldn't finish evaluating — inconclusive, so the
		// launch stays available with the warning.
		m.accessState = accessAllowed
	default:
		// allowed=false is the server's denial whether or not denied is
		// set: the review already ran the API server's whole authorizer
		// chain against the real identity, and "no authorizer allowed it"
		// is exactly what makes the real request a 403. RBAC never sets
		// denied, so waiting for it would never deny on an RBAC cluster.
		m.accessState = accessDenied
	}
}

func (m Model) accessFeedback() string {
	query := m.accessQuery()
	switch m.accessState {
	case accessChecking:
		return "checking access to " + query.Resource + "…"
	case accessDenied:
		reason := m.accessResult.Reason
		if reason == "" {
			reason = fmt.Sprintf("%s %s is denied", query.Verb, query.Resource)
		}
		return reason
	case accessAllowed:
		switch {
		case m.accessErr != nil:
			return "can't verify access: " + m.accessErr.Error()
		case m.accessResult.EvaluationError != "":
			return "can't verify access: " + m.accessResult.EvaluationError
		}
	}
	return ""
}
