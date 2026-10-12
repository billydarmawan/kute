package verbs

import (
	"strings"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// Screen-local keys: a key one screen (or one family of screens) gives its
// own meaning. Registered for the same reason the cross-screen verbs are —
// the screen's keybar hint and its key handler both read the entry, so the
// key it advertises is the key it answers to (review 13 L15: ~130 hand-
// written hints had nothing tying them to their handlers). None is
// Mutating: each is read-only UI state, a clipboard copy, or the step
// before a write that goes through its own policy row.

// 8a's YAML view (and its Secret mode, §21a).
var (
	YAMLFold      = Verb{ID: "yaml-fold", Key: "tab", Label: "fold/unfold"}
	YAMLUnfoldAll = Verb{ID: "yaml-unfold-all", Key: "f", Label: "unfold all"}
	// YAMLCopy copies the whole document — a screen-local 'Y', the same
	// reuse CopyRouteYAML makes.
	YAMLCopy = Verb{ID: "yaml-copy", Key: "Y", Label: "copy"}
	// SecretRevealToggle/SecretRevealAll/SecretCopyValue are 8a's Secret
	// mode (§21a): x reveals or masks the cursor key, X reveals every key
	// behind an inline y/N, y copies the decoded cursor value.
	SecretRevealToggle = Verb{ID: "secret-reveal-toggle", Key: "x", Label: "reveal/mask", Kinds: []kube.ResourceKind{kube.KindSecret}}
	SecretRevealAll    = Verb{ID: "secret-reveal-all", Key: "X", Label: "reveal all", Kinds: []kube.ResourceKind{kube.KindSecret}}
	SecretCopyValue    = Verb{ID: "secret-copy-value", Key: "y", Label: "copy value", Kinds: []kube.ResourceKind{kube.KindSecret}}
)

// 9b's event feed and 16a/16b's timeline.
var (
	// WarningsOnly toggles the feed between warnings and every event; the
	// keybar labels the direction pressing it takes ("all events" once on).
	WarningsOnly = Verb{ID: "warnings-only", Key: "w", Label: "warnings only"}
	// TimeWindow cycles the feed's window — a screen-local 't', since
	// Timeline's own 't' already put the user here.
	TimeWindow = Verb{ID: "time-window", Key: "t", Label: "time window"}
	// TimelineFocusRail moves 16b's focus between the revision rail and
	// the feed.
	TimelineFocusRail = Verb{ID: "timeline-focus-rail", Key: "tab", Label: "focus rail/feed"}
	// CopyRevision copies a revision or sync row's git revision (§32a/§34a).
	CopyRevision = Verb{ID: "copy-revision", Key: "v", Label: "copy revision"}
)

// 26a's labels/annotations panel.
var (
	// MetaAddKey opens 26a's one-row add on the focused grid.
	MetaAddKey = Verb{ID: "meta-add-key", Key: "a/insert", Label: "add", Presses: []string{"a", "insert"}}
)

// 13a's port-forward picker.
var (
	// ForwardLocalPort starts typing a local port into the selected row.
	ForwardLocalPort = Verb{
		ID: "forward-local-port", Key: "0-9", Label: "type local port",
		Presses: []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9"},
	}
	// ForwardOtherPort starts typing the port itself into the free-port row
	// — the remote half, or kubectl's whole `local:remote` pair.
	ForwardOtherPort = Verb{
		ID: "forward-other-port", Key: "0-9", Label: "type port",
		Presses: []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9"},
	}
)

// 27a's ConfigMap Data view.
var (
	// ConfigMapBufferEdit opens a folded multi-line value in the buffer
	// editor (whose apply is ConfigMapBufferApply).
	ConfigMapBufferEdit = Verb{ID: "configmap-buffer-edit", Key: "e", Label: "buffer editor", Kinds: []kube.ResourceKind{kube.KindConfigMap}}
)

// §41 debug panel fields. Each key edits or cycles one field of the
// launch the will-run line spells out; the launch itself is PodDebug/
// NodeDebug, under their own policy row.
var (
	DebugMode       = Verb{ID: "debug-mode", Key: "m", Label: "mode"}
	DebugImage      = Verb{ID: "debug-image", Key: "i", Label: "image"}
	DebugCopyName   = Verb{ID: "debug-copy-name", Key: "i", Label: "copy name"}
	DebugTarget     = Verb{ID: "debug-target", Key: "t", Label: "target"}
	DebugContainer  = Verb{ID: "debug-container", Key: "t", Label: "container"}
	DebugProfile    = Verb{ID: "debug-profile", Key: "p", Label: "profile"}
	DebugEntrypoint = Verb{ID: "debug-entrypoint", Key: "e", Label: "entrypoint"}
	DebugProcesses  = Verb{ID: "debug-processes", Key: "s", Label: "processes"}
	// DebugRecents cycles the image field through recently used images.
	DebugRecents = Verb{ID: "debug-recents", Key: "tab", Label: "recents"}
)

// 4c/10b's setup screen — the two ways into the kubeconfig-path buffer.
var (
	SetupEditPath  = Verb{ID: "setup-edit-path", Key: "e", Label: "edit kubeconfig path"}
	SetupEnterPath = Verb{ID: "setup-enter-path", Key: "k", Label: "enter kubeconfig path"}
)

// browse's error cards and inline editors (17b scale, 24a set image, 25a
// set resources).
var (
	// CopyError copies a 4b 403 card's or a load error's text — a
	// screen-local 'y', only while no rows are showing.
	CopyError = Verb{ID: "copy-error", Key: "y", Label: "copy error"}
	// Nudge steps 17b's replica count, or 25a's focused field by 64Mi/50m.
	// The display "+/−" is pressed as Scale's ScaleUpKey/ScaleDownKey.
	Nudge = Verb{ID: "nudge", Key: Scale.Key, Label: "nudge", Presses: []string{ScaleUpKey, ScaleDownKey}}
	// SetImageFullRef expands 24a's buffer to the full image reference.
	SetImageFullRef = Verb{ID: "set-image-full-ref", Key: "ctrl+e", Label: "full ref"}
	// SetResourcesUnset clears 25a's focused field so the apply removes it.
	SetResourcesUnset = Verb{ID: "set-resources-unset", Key: "u", Label: "unset field"}
)

// 14d's generic object detail.
var (
	// ObjectSiblings steps to the next/previous object of the kind — j/k
	// on this screen, which has no row cursor of its own to move.
	ObjectSiblings = Verb{ID: "object-siblings", Key: "j/k", Label: "next/prev", Presses: []string{"j", "k", "down", "up"}}
	// OpenOwner opens the object's first ownerReference.
	OpenOwner = Verb{ID: "open-owner", Key: "1", Label: "owner"}
)

// 28a/28b's update chip and what's-new panel.
var (
	// WhatsNew opens 28b from anywhere; the root shell handles it, hence
	// the Key living in tui.
	WhatsNew          = Verb{ID: "whats-new", Key: tui.KeyWhatsNew, Label: "what's new"}
	UpdateCopyCommand = Verb{ID: "update-copy-command", Key: "y", Label: "copy command"}
	UpdateNotes       = Verb{ID: "update-release-notes", Key: "o", Label: "release notes ↗"}
	UpdateSkip        = Verb{ID: "update-skip", Key: "x", Label: "skip"}
	UpdateRecheck     = Verb{ID: "update-recheck", Key: "r", Label: "re-check now"}
)

// 22a's who-can palettes — handled by the root shell (tui.WhoCanScoped),
// hence the Keys living in tui.
var (
	WhoCanVerb     = Verb{ID: "who-can-verb", Key: tui.KeyWhoCanVerb, Label: "verb"}
	WhoCanResource = Verb{ID: "who-can-resource", Key: tui.KeyWhoCanResource, Label: "pick resource kind"}
)

// screenVerbs is every entry above, for All.
var screenVerbs = []Verb{
	YAMLFold, YAMLUnfoldAll, YAMLCopy, SecretRevealToggle, SecretRevealAll, SecretCopyValue,
	WarningsOnly, TimeWindow, TimelineFocusRail, CopyRevision,
	MetaAddKey, ForwardLocalPort, ForwardOtherPort, ConfigMapBufferEdit,
	DebugMode, DebugImage, DebugCopyName, DebugTarget, DebugContainer, DebugProfile,
	DebugEntrypoint, DebugProcesses, DebugRecents,
	SetupEditPath, SetupEnterPath,
	CopyError, Nudge, SetImageFullRef, SetResourcesUnset,
	ObjectSiblings, OpenOwner,
	WhatsNew, UpdateCopyCommand, UpdateNotes, UpdateSkip, UpdateRecheck,
	WhoCanVerb, WhoCanResource,
}

// Either renders one hint for two keys that do the same thing here
// ("d/esc back"), joined the way the keybar spells alternatives.
func Either(label string, vs ...Verb) tui.KeyHint {
	keys := make([]string, len(vs))
	for i, v := range vs {
		keys[i] = v.Key
	}
	return tui.KeyHint{Key: strings.Join(keys, "/"), Label: label}
}
