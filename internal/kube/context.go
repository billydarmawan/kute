package kube

import "context"

// Context describes the active Kubernetes cluster context used by task screens.
type Context struct {
	ClusterName string
	ContextName string
	Namespace   string
	// UserName is the identity tasks/whocan (22a) pins as "current user"
	// when resolving RBAC bindings. For client-certificate auth (the common
	// case for kind/kwok/k3d-style local clusters) this is the client
	// cert's Subject CommonName — the identity the API server actually
	// authenticates, which is often completely different from the
	// kubeconfig user entry's own name (e.g. a context/user both literally
	// named "kwok-prod-sim" whose client cert CN is "kwok-admin"). Falls
	// back to the kubeconfig AuthInfo name for every other auth mode (token,
	// exec plugin, OIDC, …), where there's no local way to know the real
	// identity without a server round trip (deliberately avoided — 22a's
	// resolution is cache-only, "no server round-trip, works read-only").
	UserName string
	// UserGroups are the identity's known group memberships — for
	// client-certificate auth, the cert's Subject Organization fields
	// (RFC 2253/Kubernetes' own convention: O= entries are group names,
	// e.g. "system:masters"). Empty for every other auth mode: kubeconfig
	// carries no group information for a bearer token or exec plugin
	// identity locally, so a Group-only grant (the overwhelmingly common
	// shape for "system:masters"/cluster-admin bindings) can't be resolved
	// for those without a server round trip.
	UserGroups []string
	// UserFromAuthInfo reports that UserName is only the kubeconfig
	// AuthInfo entry's name (token, exec plugin, OIDC — every managed
	// cloud), e.g. "arn:aws:eks:…" or "gke_proj_zone_c", which is never a
	// subject any binding names. 22a then can't claim a denial for it.
	UserFromAuthInfo bool
}

// ContextProvider reads the active Kubernetes context.
type ContextProvider interface {
	CurrentContext(context.Context) (Context, error)
}

// UnavailableContext returns an explicit fallback context for error states.
func UnavailableContext(clusterName string) Context {
	if clusterName == "" {
		clusterName = "cluster unavailable"
	}

	return Context{ClusterName: clusterName, Namespace: "default"}
}
