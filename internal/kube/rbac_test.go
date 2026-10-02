package kube

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func clusterRole(name string, rules ...rbacv1.PolicyRule) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rules}
}

func role(name, ns string, rules ...rbacv1.PolicyRule) *rbacv1.Role {
	return &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Rules: rules}
}

func clusterRoleBinding(name, roleName string, subjects ...rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: roleName},
		Subjects:   subjects,
	}
}

func roleBinding(name, ns, roleKind, roleName string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{Kind: roleKind, Name: roleName},
		Subjects:   subjects,
	}
}

func TestResolveWhoCan_ClusterRoleBindingGrantsClusterScope(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("admin", rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("cluster-admins", "admin", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "alice"}),
	}

	result := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "secrets", Namespace: "default"}, "", nil, crs, nil, crbs, nil)

	if len(result.Subjects) != 1 {
		t.Fatalf("subjects = %d, want 1: %+v", len(result.Subjects), result.Subjects)
	}
	s := result.Subjects[0]
	if s.Name != "alice" || s.Kind != "User" {
		t.Fatalf("subject = %+v, want alice/User", s)
	}
	if !s.ClusterScope {
		t.Fatal("expected ClusterScope true for a ClusterRoleBinding grant")
	}
	if s.Via != "clusterrole/admin ← clusterrolebinding/cluster-admins" {
		t.Fatalf("via = %q", s.Via)
	}
}

func TestResolveWhoCan_RoleBindingScopedToNamespace(t *testing.T) {
	roles := []*rbacv1.Role{
		role("secret-reader", "default", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}}),
	}
	rbs := []*rbacv1.RoleBinding{
		roleBinding("secret-readers", "default", "Role", "secret-reader", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "bob"}),
	}

	inNamespace := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "secrets", Namespace: "default"}, "", nil, nil, roles, nil, rbs)
	if len(inNamespace.Subjects) != 1 || inNamespace.Subjects[0].Name != "bob" {
		t.Fatalf("expected bob granted in default, got %+v", inNamespace.Subjects)
	}
	if inNamespace.Subjects[0].ClusterScope {
		t.Fatal("expected ClusterScope false for a RoleBinding grant")
	}

	otherNamespace := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "secrets", Namespace: "staging"}, "", nil, nil, roles, nil, rbs)
	if len(otherNamespace.Subjects) != 0 {
		t.Fatalf("expected no subjects outside the binding's namespace, got %+v", otherNamespace.Subjects)
	}
}

func TestResolveWhoCan_WildcardVerbAndResourceMatch(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("everything", rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("binding", "everything", rbacv1.Subject{Kind: rbacv1.GroupKind, Name: "admins"}),
	}
	result := ResolveWhoCan(WhoCanQuery{Verb: "delete", Resource: "pods", Namespace: ""}, "", nil, crs, nil, crbs, nil)
	if len(result.Subjects) != 1 || result.Subjects[0].Kind != "Group" {
		t.Fatalf("expected the wildcard rule to match, got %+v", result.Subjects)
	}
}

func TestResolveWhoCan_DedupesSubjectAcrossBindings(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("view", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("b1", "view", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "carol"}),
		clusterRoleBinding("b2", "view", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "carol"}),
	}
	result := ResolveWhoCan(WhoCanQuery{Verb: "get", Resource: "pods"}, "", nil, crs, nil, crbs, nil)
	if len(result.Subjects) != 1 {
		t.Fatalf("expected carol deduped to one row, got %+v", result.Subjects)
	}
}

func TestResolveWhoCan_CurrentUserGrantedIsFlagged(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("view", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("viewers", "view", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "dev-readonly"}),
	}
	result := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "pods"}, "dev-readonly", nil, crs, nil, crbs, nil)
	if !result.CurrentUserGranted {
		t.Fatal("expected dev-readonly to be granted")
	}
	want := "clusterrole/view ← clusterrolebinding/viewers"
	if result.CurrentUserVia != want {
		t.Fatalf("CurrentUserVia = %q, want %q", result.CurrentUserVia, want)
	}
}

// TestResolveWhoCan_CurrentUserGrantedViaGroup covers the overwhelmingly
// common real-world shape this exact case used to miss: a client-cert
// identity (e.g. CN=kwok-admin, O=system:masters) is never itself a "User"
// subject on any binding — access comes entirely through a Group subject
// (system:masters) bound to cluster-admin. Before this fix,
// ResolveWhoCan only ever matched an exact User name, so a user granted
// purely through group membership was wrongly reported as having no
// access at all.
func TestResolveWhoCan_CurrentUserGrantedViaGroup(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("cluster-admin", rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("cluster-admin", "cluster-admin", rbacv1.Subject{Kind: rbacv1.GroupKind, Name: "system:masters"}),
	}
	result := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "secrets", Namespace: "default"}, "kwok-admin", []string{"system:masters"}, crs, nil, crbs, nil)
	if !result.CurrentUserGranted {
		t.Fatalf("expected kwok-admin to be granted via the system:masters group, got %+v", result)
	}
	want := "clusterrole/cluster-admin ← clusterrolebinding/cluster-admin · via group system:masters"
	if result.CurrentUserVia != want {
		t.Fatalf("CurrentUserVia = %q, want %q", result.CurrentUserVia, want)
	}
	if !result.CurrentUserClusterScope {
		t.Fatal("expected CurrentUserClusterScope true for a ClusterRoleBinding grant")
	}
}

// TestResolveWhoCan_UnknownGroupStillMisses ensures a Group subject that
// isn't among the caller's own groups doesn't spuriously grant access.
func TestResolveWhoCan_UnknownGroupStillMisses(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("cluster-admin", rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("cluster-admin", "cluster-admin", rbacv1.Subject{Kind: rbacv1.GroupKind, Name: "system:masters"}),
	}
	result := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "secrets"}, "dev-readonly", []string{"dev-team"}, crs, nil, crbs, nil)
	if result.CurrentUserGranted {
		t.Fatalf("expected dev-readonly (only in dev-team) not to be granted via system:masters, got %+v", result)
	}
}

func TestResolveWhoCan_CurrentUserClosestMissExplainsTheGap(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("view", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("viewers", "view", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "dev-readonly"}),
	}
	result := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "secrets"}, "dev-readonly", nil, crs, nil, crbs, nil)
	if result.CurrentUserGranted {
		t.Fatal("expected dev-readonly not to be granted secrets access")
	}
	want := "clusterrole/view grants get, list on pods — not secrets"
	if result.CurrentUserVia != want {
		t.Fatalf("CurrentUserVia = %q, want %q", result.CurrentUserVia, want)
	}
}

func TestResolveWhoCan_CurrentUserWithNoBindingsAtAll(t *testing.T) {
	result := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "secrets"}, "nobody", nil, nil, nil, nil, nil)
	if result.CurrentUserGranted {
		t.Fatal("expected no access")
	}
	want := "no bindings grant nobody access here"
	if result.CurrentUserVia != want {
		t.Fatalf("CurrentUserVia = %q, want %q", result.CurrentUserVia, want)
	}
}

func TestResolveWhoCan_SameNamedServiceAccountsInDifferentNamespacesStayDistinct(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("reader", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("a", "reader", rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: "team-a"}),
		clusterRoleBinding("b", "reader", rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: "team-b"}),
	}

	result := ResolveWhoCan(WhoCanQuery{Verb: "get", Resource: "secrets"}, "", nil, crs, nil, crbs, nil)

	if len(result.Subjects) != 2 {
		t.Fatalf("subjects = %+v, want both team-a/default and team-b/default", result.Subjects)
	}
	if result.Subjects[0].Namespace != "team-a" || result.Subjects[1].Namespace != "team-b" {
		t.Fatalf("subjects = %+v, want team-a then team-b", result.Subjects)
	}
	if result.Subjects[1].BindingName != "b" {
		t.Fatalf("team-b/default's binding = %q, want b", result.Subjects[1].BindingName)
	}
}

func TestResolveWhoCan_MatchesTheQueriedAPIGroup(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("metrics", rbacv1.PolicyRule{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}),
		clusterRole("deployer", rbacv1.PolicyRule{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("metrics", "metrics", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "metrics-user"}),
		clusterRoleBinding("deployers", "deployer", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "deployer"}),
	}

	pods := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "pods"}, "", nil, crs, nil, crbs, nil)
	if len(pods.Subjects) != 0 {
		t.Fatalf("a metrics.k8s.io pods rule must not grant core pods, got %+v", pods.Subjects)
	}
	metrics := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "pods", Group: "metrics.k8s.io"}, "", nil, crs, nil, crbs, nil)
	if len(metrics.Subjects) != 1 || metrics.Subjects[0].Name != "metrics-user" {
		t.Fatalf("expected metrics-user on metrics.k8s.io pods, got %+v", metrics.Subjects)
	}
	deployments := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "deployments", Group: "apps"}, "", nil, crs, nil, crbs, nil)
	if len(deployments.Subjects) != 1 || deployments.Subjects[0].Name != "deployer" {
		t.Fatalf("expected deployer on deployments.apps, got %+v", deployments.Subjects)
	}
}

func TestSplitAndJoinRBACResource(t *testing.T) {
	for _, tc := range []struct{ in, resource, group string }{
		{"secrets", "secrets", ""},
		{"deployments.apps", "deployments", "apps"},
		{"pods/ephemeralcontainers", "pods/ephemeralcontainers", ""},
		{"deployments.apps/scale", "deployments/scale", "apps"},
		{"certificates.cert-manager.io", "certificates", "cert-manager.io"},
	} {
		resource, group := SplitRBACResource(tc.in)
		if resource != tc.resource || group != tc.group {
			t.Errorf("SplitRBACResource(%q) = %q, %q; want %q, %q", tc.in, resource, group, tc.resource, tc.group)
		}
		if got := JoinRBACResource(resource, group); got != tc.in {
			t.Errorf("JoinRBACResource(%q, %q) = %q, want %q", resource, group, got, tc.in)
		}
	}
}

func TestResolveWhoCan_ResourceNamesRuleIsMarkedAndNeverGrantsList(t *testing.T) {
	crs := []*rbacv1.ClusterRole{
		clusterRole("one-secret", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"only-this"}, Verbs: []string{"get", "list"}}),
		clusterRole("reader", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}),
	}
	crbs := []*rbacv1.ClusterRoleBinding{
		clusterRoleBinding("narrow", "one-secret",
			rbacv1.Subject{Kind: rbacv1.UserKind, Name: "narrow-user"},
			rbacv1.Subject{Kind: rbacv1.UserKind, Name: "both"}),
		clusterRoleBinding("wide", "reader", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "both"}),
	}

	list := ResolveWhoCan(WhoCanQuery{Verb: "list", Resource: "secrets"}, "narrow-user", nil, crs, nil, crbs, nil)
	if len(list.Subjects) != 0 {
		t.Fatalf("a resourceNames rule must never grant list, got %+v", list.Subjects)
	}
	if list.CurrentUserGranted {
		t.Fatal("narrow-user must not be granted list secrets")
	}

	get := ResolveWhoCan(WhoCanQuery{Verb: "get", Resource: "secrets"}, "narrow-user", nil, crs, nil, crbs, nil)
	if len(get.Subjects) != 2 {
		t.Fatalf("subjects = %+v, want both and narrow-user", get.Subjects)
	}
	both, narrow := get.Subjects[0], get.Subjects[1]
	if both.Name != "both" || len(both.OnlyNames) != 0 || both.BindingName != "wide" {
		t.Fatalf("both = %+v, want the blanket grant from wide to supersede the named one", both)
	}
	if narrow.Name != "narrow-user" || len(narrow.OnlyNames) != 1 || narrow.OnlyNames[0] != "only-this" {
		t.Fatalf("narrow-user = %+v, want OnlyNames [only-this]", narrow)
	}
	if narrow.Via != "clusterrole/one-secret ← clusterrolebinding/narrow · only: only-this" {
		t.Fatalf("narrow-user via = %q", narrow.Via)
	}
	if !get.CurrentUserGranted || len(get.CurrentUserOnlyNames) != 1 {
		t.Fatalf("current user = granted %v only %v, want a name-limited grant", get.CurrentUserGranted, get.CurrentUserOnlyNames)
	}
}
