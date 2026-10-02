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
		clusterRole("view", rbacv1.PolicyRule{Resources: []string{"pods"}, Verbs: []string{"get"}}),
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
		clusterRole("view", rbacv1.PolicyRule{Resources: []string{"pods"}, Verbs: []string{"get", "list"}}),
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
		clusterRole("view", rbacv1.PolicyRule{Resources: []string{"pods"}, Verbs: []string{"get", "list"}}),
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
