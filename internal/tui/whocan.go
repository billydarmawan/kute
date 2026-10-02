package tui

import (
	"cmp"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui/components/palette"
)

// This file wires real data into tasks/whocan's (22a) 'v'/'K' slot edits —
// the summary strip's "v verb · K pick resource kind" palette opens
// (docs/design README.md §22a) — following goto.go/namespace.go's pattern:
// the palette package stays pure UI, this file is where the root shell
// reaches into Session for real content. 'K' is capital: lowercase 'k'
// stays universal move-up (CLAUDE.md's "j/k ≡ ↑↓ everywhere"), which a
// lowercase intercept here would have permanently broken on this one screen.

// WhoCanScoped is implemented by tasks/whocan's Model so the root shell can
// read the current verb/resource slots to seed the palette on 'v'/'K' and
// gate those two keys to whocan's own screen — unlike g/n/c (global
// everywhere), whocan's query lives on the task itself, not Session, so
// there's nothing for the root shell to key off without this.
type WhoCanScoped interface {
	WhoCanQuery() (verb, resource string)
}

// whoCanVerbs is the fixed RBAC verb vocabulary the 'v' palette lists —
// standard verbs plus the wildcard.
var whoCanVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection", "*"}

// whoCanVerbTarget/whoCanResourceTarget are the opaque payloads carried in
// palette.Item.Data for the 'v'/'K' scopes.
type whoCanVerbTarget struct{ verb string }
type whoCanResourceTarget struct{ resource string }

func whoCanVerbItems(current string) []palette.Item {
	items := make([]palette.Item, 0, len(whoCanVerbs))
	for _, v := range whoCanVerbs {
		item := palette.Item{Label: v, Data: whoCanVerbTarget{verb: v}}
		if v == current {
			item.Tag = "current"
		}
		items = append(items, item)
	}
	return items
}

// whoCanResourceItems lists every registered kind's group-qualified plural
// resource name (resources.RBACResource — "deployments.apps", "secrets"), built
// from sess.Groups the same way goto's own gotoKindItems walks the
// registry. Forward/CustomResourceDefinition are synthetic built-in kinds
// (docs/design README.md's own "registry kind, not a Kubernetes API type"
// carve-outs), never real RBAC-governed resources, so they're excluded.
func whoCanResourceItems(sess *Session, current string) []palette.Item {
	if sess == nil {
		return nil
	}
	seen := make(map[kube.ResourceKind]bool)
	var items []palette.Item
	for _, group := range sess.Groups {
		for _, kind := range group.Kinds {
			if kind == kube.KindForward || kind == kube.KindCustomResourceDefinition || seen[kind] {
				continue
			}
			seen[kind] = true
			desc, ok := sess.Registry.Descriptor(kind)
			if !ok {
				continue
			}
			resource := resources.RBACResource(desc)
			item := palette.Item{Label: resource, Detail: desc.Display, Data: whoCanResourceTarget{resource: resource}}
			if resource == current {
				item.Tag = "current"
			}
			items = append(items, item)
		}
	}
	slices.SortFunc(items, func(a, b palette.Item) int { return cmp.Compare(a.Label, b.Label) })
	return items
}

func whoCanVerbDispatch(item palette.Item) tea.Cmd {
	target, ok := item.Data.(whoCanVerbTarget)
	if !ok {
		return nil
	}
	verb := target.verb
	return func() tea.Msg { return SetWhoCanVerbMsg{Verb: verb} }
}

func whoCanResourceDispatch(item palette.Item) tea.Cmd {
	target, ok := item.Data.(whoCanResourceTarget)
	if !ok {
		return nil
	}
	resource := target.resource
	return func() tea.Msg { return SetWhoCanResourceMsg{Resource: resource} }
}

// openVerbPalette/openResourcePalette open the shell's one palette instance
// scoped to ScopeVerb/ScopeResource, seeded from whocan's current query
// (read via WhoCanScoped before this is called).
func (m *Model) openVerbPalette(current string) tea.Cmd {
	m.stopContextProbe()
	m.palette = &palette.Model{Scope: palette.ScopeVerb, Prompt: "verb ›", Hint: "RBAC verb"}
	m.palette.Input = palette.NewInput(palette.ScopeVerb)
	m.palette.Input.SetStyles(TextInputStyles(m.Theme()))
	m.mode = ModeGoto
	m.whoCanVerbItemsCache = whoCanVerbItems(current)
	m.palette.Items = m.whoCanVerbItemsCache
	return nil
}

func (m *Model) openResourcePalette(current string) tea.Cmd {
	m.stopContextProbe()
	m.palette = &palette.Model{Scope: palette.ScopeResource, Prompt: "resource ›", Hint: "RBAC resource"}
	m.palette.Input = palette.NewInput(palette.ScopeResource)
	m.palette.Input.SetStyles(TextInputStyles(m.Theme()))
	m.mode = ModeGoto
	m.whoCanResourceItemsCache = whoCanResourceItems(m.session, current)
	m.palette.Items = m.whoCanResourceItemsCache
	return nil
}

// refreshWhoCanVerbPalette/refreshWhoCanResourcePalette re-filter their
// cached, unfiltered item list against the current query — mirrors
// refreshNamespacePalette's cache-filter shape, since both lists are static
// per palette-open rather than needing a live refetch per keystroke.
func (m *Model) refreshWhoCanVerbPalette() {
	items := m.whoCanVerbItemsCache
	if m.palette.Query() != "" {
		items = palette.Filter(items, m.palette.Query())
	}
	m.palette.Items = items
	m.palette.Sel = 0
}

func (m *Model) refreshWhoCanResourcePalette() {
	items := m.whoCanResourceItemsCache
	if m.palette.Query() != "" {
		items = palette.Filter(items, m.palette.Query())
	}
	m.palette.Items = items
	m.palette.Sel = 0
}
