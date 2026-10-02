package kube

import (
	"cmp"
	"context"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
)

// WhoCanQuery is 22a's editable question — "who can <Verb> <Resource> [in
// <Namespace>]" — the strip's v/k/n slots (docs/design README.md §22a).
// Resource is the plural, lowercase API resource name ("secrets", "pods"),
// matching how rbacv1.PolicyRule.Resources spells it; Group is its API
// group ("" for core, "apps" for deployments), matched against
// rbacv1.PolicyRule.APIGroups — without it a metrics.k8s.io "pods" rule
// reads as a grant on core pods.
type WhoCanQuery struct {
	Verb      string
	Resource  string
	Group     string
	Namespace string
}

// SplitRBACResource splits kubectl's group-qualified resource spelling
// ("deployments.apps", "pods/ephemeralcontainers", "deployments.apps/scale")
// into the resource (with any subresource) and its API group — "" when
// unqualified, i.e. the core group, as `kubectl auth can-i` reads it.
func SplitRBACResource(s string) (resource, group string) {
	base, sub, hasSub := strings.Cut(s, "/")
	resource, group, _ = strings.Cut(base, ".")
	if hasSub {
		resource += "/" + sub
	}
	return resource, group
}

// JoinRBACResource is SplitRBACResource's inverse: "deployments" + "apps"
// → "deployments.apps", a core resource unqualified.
func JoinRBACResource(resource, group string) string {
	if group == "" {
		return resource
	}
	base, sub, hasSub := strings.Cut(resource, "/")
	out := base + "." + group
	if hasSub {
		out += "/" + sub
	}
	return out
}

// BuiltinRBACResource is a typed kind's plural resource and API group as
// RBAC rules name them ("deployments", "apps"); ok is false for any kind
// outside the typed table.
func BuiltinRBACResource(kind ResourceKind) (resource, group string, ok bool) {
	tk, found := typedKinds[kind]
	if !found {
		return "", "", false
	}
	return tk.gvr.Resource, tk.gvr.Group, true
}

// WhoCanSubject is one resolved SUBJECT/KIND/VIA/SCOPE row (§22a).
type WhoCanSubject struct {
	Name string // subject name
	Kind string // "User", "Group", or "ServiceAccount"
	// Namespace is a ServiceAccount subject's namespace ("" for User and
	// Group, which are cluster-wide identities) — team-a/default and
	// team-b/default are different service accounts.
	Namespace string
	// Via names the effective rule's role and binding, e.g.
	// "clusterrole/admin ← rb/admins" — aggregated ClusterRoles need no
	// special handling here since kube-controller-manager already flattens
	// an aggregating ClusterRole's Rules server-side, so the leaf
	// clusterrole read from the cache already carries the effective rule.
	Via string
	// ClusterScope is true when the grant came from a ClusterRoleBinding
	// (§22a's SCOPE column: "cluster", blue) vs a namespace-scoped
	// RoleBinding ("namespace", dim).
	ClusterScope bool
	// BindingKind/BindingNamespace/BindingName identify the binding object
	// backing this row, for whocan's "↵ opens the binding's YAML" (§22a).
	BindingKind      ResourceKind
	BindingNamespace string
	BindingName      string
}

// WhoCanResult is one WhoCan resolution — every subject currently granted
// Query, plus the pinned current-user verdict (§22a: "the current user
// pinned as a red ✕ row whose VIA explains the closest miss").
type WhoCanResult struct {
	Query    WhoCanQuery
	Subjects []WhoCanSubject
	// CurrentUser is the real authenticated identity (kube.Context.UserName
	// — a client cert's CommonName when available, else the kubeconfig
	// AuthInfo name), "" if unknown.
	CurrentUser string
	// CurrentUserGranted reports whether CurrentUser is covered by one of
	// Subjects — either an exact "User" kind match, or (the overwhelmingly
	// common shape for cluster-admin/system:masters-style grants) a
	// "Group" kind subject naming one of CurrentUserGroups.
	CurrentUserGranted bool
	// CurrentUserVia explains how CurrentUser is granted when
	// CurrentUserGranted (e.g. "clusterrole/cluster-admin ←
	// clusterrolebinding/cluster-admin · via group system:masters"), or
	// the closest miss when !CurrentUserGranted — "role/viewer grants get,
	// list on pods — not secrets" — or a plain "no bindings grant ...
	// access here" when nothing at all was found. Empty when CurrentUser
	// is unknown.
	CurrentUserVia string
	// CurrentUserClusterScope mirrors WhoCanSubject.ClusterScope for
	// whichever grant backs CurrentUserVia — meaningful only when
	// CurrentUserGranted.
	CurrentUserClusterScope bool
}

// matchesVerb reports whether rule grants verb ("*" matches anything).
func matchesVerb(rule rbacv1.PolicyRule, verb string) bool {
	return slices.ContainsFunc(rule.Verbs, func(v string) bool { return v == "*" || v == verb })
}

// matchesResource reports whether rule grants resource ("*" matches
// anything). ResourceNames is deliberately ignored — 22a's query has no
// resource-name slot, so a rule scoped to specific names still counts as a
// match on the resource type.
func matchesResource(rule rbacv1.PolicyRule, resource string) bool {
	return slices.ContainsFunc(rule.Resources, func(r string) bool { return r == "*" || r == resource })
}

// matchesAPIGroup reports whether rule covers group ("*" matches any; ""
// is the core group, as RBAC spells it).
func matchesAPIGroup(rule rbacv1.PolicyRule, group string) bool {
	return slices.ContainsFunc(rule.APIGroups, func(g string) bool { return g == "*" || g == group })
}

func ruleMatches(rule rbacv1.PolicyRule, query WhoCanQuery) bool {
	return matchesVerb(rule, query.Verb) && matchesAPIGroup(rule, query.Group) && matchesResource(rule, query.Resource)
}

func anyRuleMatches(rules []rbacv1.PolicyRule, query WhoCanQuery) bool {
	return slices.ContainsFunc(rules, func(rule rbacv1.PolicyRule) bool {
		return ruleMatches(rule, query)
	})
}

// matchesIdentity reports whether subjects covers user (an exact "User"
// kind match) or any of groups (a "Group" kind subject naming one of
// them) — the two ways a real identity ever appears in an RBAC binding.
func matchesIdentity(subjects []rbacv1.Subject, user string, groups []string) bool {
	return slices.ContainsFunc(subjects, func(s rbacv1.Subject) bool {
		return s.Kind == rbacv1.UserKind && s.Name == user
	}) || slices.ContainsFunc(subjects, func(s rbacv1.Subject) bool {
		return s.Kind == rbacv1.GroupKind && slices.Contains(groups, s.Name)
	})
}

// ResolveWhoCan walks ClusterRoleBindings/RoleBindings → (Cluster)Roles
// entirely in memory — no server round trip — answering §22a's question.
// Every argument is already-fetched informer-cache content (the caller,
// Cluster.WhoCan/fake.Cluster.WhoCan, does the ListRaw-equivalent fetch);
// this stays a pure function so it's unit-testable without any cluster.
// roleBindings should already be namespace-filtered to query.Namespace by
// the caller (mirrors ListRaw's own namespace filtering) — an empty
// Namespace means "no RoleBindings apply", matching a cluster-scoped
// resource query.
func ResolveWhoCan(
	query WhoCanQuery,
	currentUser string,
	currentUserGroups []string,
	clusterRoles []*rbacv1.ClusterRole,
	roles []*rbacv1.Role,
	clusterRoleBindings []*rbacv1.ClusterRoleBinding,
	roleBindings []*rbacv1.RoleBinding,
) WhoCanResult {
	crByName := make(map[string]*rbacv1.ClusterRole, len(clusterRoles))
	for _, cr := range clusterRoles {
		crByName[cr.Name] = cr
	}
	rByName := make(map[string]*rbacv1.Role, len(roles))
	for _, r := range roles {
		rByName[r.Namespace+"/"+r.Name] = r
	}

	var subjects []WhoCanSubject
	seen := make(map[string]bool)
	addRow := func(subj rbacv1.Subject, via string, clusterScope bool, bindingKind ResourceKind, bindingNS, bindingName string) {
		var ns string
		if subj.Kind == rbacv1.ServiceAccountKind {
			ns = subj.Namespace
		}
		key := subj.Kind + "/" + ns + "/" + subj.Name
		if seen[key] {
			return
		}
		seen[key] = true
		subjects = append(subjects, WhoCanSubject{
			Name: subj.Name, Kind: subj.Kind, Namespace: ns, Via: via, ClusterScope: clusterScope,
			BindingKind: bindingKind, BindingNamespace: bindingNS, BindingName: bindingName,
		})
	}

	for _, crb := range clusterRoleBindings {
		if crb.RoleRef.Kind != "ClusterRole" {
			continue
		}
		cr, ok := crByName[crb.RoleRef.Name]
		if !ok || !anyRuleMatches(cr.Rules, query) {
			continue
		}
		via := "clusterrole/" + cr.Name + " ← clusterrolebinding/" + crb.Name
		for _, s := range crb.Subjects {
			addRow(s, via, true, KindClusterRoleBinding, "", crb.Name)
		}
	}

	if query.Namespace != "" {
		for _, rb := range roleBindings {
			if rb.Namespace != query.Namespace {
				continue
			}
			var rules []rbacv1.PolicyRule
			var roleLabel string
			switch rb.RoleRef.Kind {
			case "ClusterRole":
				cr, ok := crByName[rb.RoleRef.Name]
				if !ok {
					continue
				}
				rules, roleLabel = cr.Rules, "clusterrole/"+cr.Name
			case "Role":
				r, ok := rByName[rb.Namespace+"/"+rb.RoleRef.Name]
				if !ok {
					continue
				}
				rules, roleLabel = r.Rules, "role/"+r.Name
			default:
				continue
			}
			if !anyRuleMatches(rules, query) {
				continue
			}
			via := roleLabel + " ← rolebinding/" + rb.Name
			for _, s := range rb.Subjects {
				addRow(s, via, false, KindRoleBinding, rb.Namespace, rb.Name)
			}
		}
	}

	slices.SortFunc(subjects, func(a, b WhoCanSubject) int {
		return cmp.Or(
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Namespace, b.Namespace),
		)
	})

	result := WhoCanResult{Query: query, Subjects: subjects, CurrentUser: currentUser}
	if currentUser == "" {
		return result
	}
	// Exact User match first; a Group match (the common shape for
	// cluster-admin/system:masters-style grants) only if no more specific
	// User row exists.
	for _, s := range subjects {
		if s.Kind == rbacv1.UserKind && s.Name == currentUser {
			result.CurrentUserGranted = true
			result.CurrentUserVia = s.Via
			result.CurrentUserClusterScope = s.ClusterScope
			return result
		}
	}
	for _, s := range subjects {
		if s.Kind == rbacv1.GroupKind && slices.Contains(currentUserGroups, s.Name) {
			result.CurrentUserGranted = true
			result.CurrentUserVia = s.Via + " · via group " + s.Name
			result.CurrentUserClusterScope = s.ClusterScope
			return result
		}
	}
	result.CurrentUserVia = closestMiss(currentUser, currentUserGroups, query, crByName, rByName, clusterRoleBindings, roleBindings)
	return result
}

// closestMiss explains why currentUser isn't among the resolved subjects —
// §22a's "role/viewer grants get, list on pods — not secrets": the first
// rule found (across every binding naming the user, or one of their
// groups, as a subject) that shares either the queried verb or the queried
// resource.
func closestMiss(
	user string, groups []string, query WhoCanQuery,
	crByName map[string]*rbacv1.ClusterRole, rByName map[string]*rbacv1.Role,
	clusterRoleBindings []*rbacv1.ClusterRoleBinding, roleBindings []*rbacv1.RoleBinding,
) string {
	var best *rbacv1.PolicyRule
	var bestLabel string
	consider := func(rules []rbacv1.PolicyRule, label string) {
		for i := range rules {
			if best != nil {
				return
			}
			if matchesResource(rules[i], query.Resource) || matchesVerb(rules[i], query.Verb) {
				best = &rules[i]
				bestLabel = label
			}
		}
	}

	for _, crb := range clusterRoleBindings {
		if !matchesIdentity(crb.Subjects, user, groups) {
			continue
		}
		if cr, ok := crByName[crb.RoleRef.Name]; ok {
			consider(cr.Rules, "clusterrole/"+cr.Name)
		}
	}
	for _, rb := range roleBindings {
		if rb.Namespace != query.Namespace || !matchesIdentity(rb.Subjects, user, groups) {
			continue
		}
		switch rb.RoleRef.Kind {
		case "ClusterRole":
			if cr, ok := crByName[rb.RoleRef.Name]; ok {
				consider(cr.Rules, "clusterrole/"+cr.Name)
			}
		case "Role":
			if r, ok := rByName[rb.Namespace+"/"+rb.RoleRef.Name]; ok {
				consider(r.Rules, "role/"+r.Name)
			}
		}
	}

	if best == nil {
		return "no bindings grant " + user + " access here"
	}
	return bestLabel + " grants " + strings.Join(best.Verbs, ", ") + " on " + strings.Join(best.Resources, ", ") + " — not " + query.Resource
}

// WhoCan resolves query against the informer cache (§22a) — the Cluster
// counterpart of ResolveWhoCan; fake.Cluster.WhoCan mirrors this shape
// against its own in-memory objects.
func (c *Cluster) WhoCan(ctx context.Context, query WhoCanQuery) (WhoCanResult, error) {
	crObjs, err := c.ListRaw(ctx, KindClusterRole, "")
	if err != nil {
		return WhoCanResult{}, err
	}
	rObjs, err := c.ListRaw(ctx, KindRole, query.Namespace)
	if err != nil {
		return WhoCanResult{}, err
	}
	crbObjs, err := c.ListRaw(ctx, KindClusterRoleBinding, "")
	if err != nil {
		return WhoCanResult{}, err
	}
	rbObjs, err := c.ListRaw(ctx, KindRoleBinding, query.Namespace)
	if err != nil {
		return WhoCanResult{}, err
	}

	clusterRoles := make([]*rbacv1.ClusterRole, 0, len(crObjs))
	for _, o := range crObjs {
		if cr, ok := o.(*rbacv1.ClusterRole); ok {
			clusterRoles = append(clusterRoles, cr)
		}
	}
	roles := make([]*rbacv1.Role, 0, len(rObjs))
	for _, o := range rObjs {
		if r, ok := o.(*rbacv1.Role); ok {
			roles = append(roles, r)
		}
	}
	clusterRoleBindings := make([]*rbacv1.ClusterRoleBinding, 0, len(crbObjs))
	for _, o := range crbObjs {
		if crb, ok := o.(*rbacv1.ClusterRoleBinding); ok {
			clusterRoleBindings = append(clusterRoleBindings, crb)
		}
	}
	roleBindings := make([]*rbacv1.RoleBinding, 0, len(rbObjs))
	for _, o := range rbObjs {
		if rb, ok := o.(*rbacv1.RoleBinding); ok {
			roleBindings = append(roleBindings, rb)
		}
	}

	return ResolveWhoCan(query, c.Context.UserName, c.Context.UserGroups, clusterRoles, roles, clusterRoleBindings, roleBindings), nil
}
