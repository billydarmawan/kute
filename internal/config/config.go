// Package config reads and writes the kute user config file
// ($XDG_CONFIG_HOME/kute/config.yaml, else ~/.config/kute/config.yaml — see
// Path). Its one MVP field, prodContexts, is the
// sole source of PROD status (mvp-plan.md §Decisions already made #2) — a
// deliberate deviation from the design handoff's kubeconfig-annotation
// approach, and never a name heuristic. SetProd is the one write path (7a's
// ctrl+p mark/unmark-prod key), and it rewrites prodContexts alone;
// everything else only ever reads the file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// Config is the parsed config.yaml (see Path).
type Config struct {
	ProdContexts StringList `yaml:"prodContexts,omitempty"`
	// Theme overrides terminal-background detection: "dark" or "light".
	// Empty (or any other value) falls back to detection. A --theme flag
	// takes precedence over this — see decision #3 in mvp-plan.md.
	Theme string `yaml:"theme,omitempty"`
	// NodeShellImage overrides the debug-container image the node-shell
	// verb ('s' on a node) hands to kubectl debug — for clusters that can't
	// pull from Docker Hub. Empty falls back to kube.DefaultNodeShellImage.
	NodeShellImage string `yaml:"nodeShellImage,omitempty"`
	// ArgoCDNamespace is Argo CD's control-plane namespace — where its
	// argocd-cm lives — for an install outside the default "argocd". Read
	// only by §33a's 'u' dashboard-link lookup, which checks it first;
	// read-only like every key but prodContexts (SetProd never writes it).
	ArgoCDNamespace string `yaml:"argoCDNamespace,omitempty"`
	// Update holds 28a/28b's update-check toggle — relevant behind
	// egress-flagging proxies (docs/design README.md §28a).
	Update UpdateConfig `yaml:"update,omitempty"`

	// LoadErr is why the file on disk could not be read in full — nil for a
	// clean read and for a file that simply doesn't exist. Every field that
	// did decode is still populated; the UI shows LoadErr as a header chip
	// (tui.BuildConfigChip) and the composition root logs it to the
	// diagnostics sink, so a broken hand edit is never silent.
	LoadErr error `yaml:"-"`
	// prodUnknown is set when prodContexts itself couldn't be read (an
	// unreadable or syntactically broken file, or a prodContexts value that
	// is neither a string nor a list of strings). IsProd then answers true
	// for every context: a broken config must never quietly lower the
	// destructive-action tier (fail closed, CLAUDE.md's destructive-action
	// policy).
	prodUnknown bool
	// updateCheckOff is --no-update-check: a per-invocation override that is
	// deliberately not a persisted field (see DisableUpdateCheck).
	updateCheckOff bool
}

// StringList is a YAML list of strings that also accepts a lone scalar as a
// one-element list — `prodContexts: prod-eu` is the hand edit people
// actually make, and it means exactly what it looks like.
type StringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*l = StringList{s}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
}

// UpdateConfig is Config's "update:" block.
type UpdateConfig struct {
	// Check disables the ambient release-feed GET entirely when explicitly
	// set to false. A nil pointer (the key absent from the file) means
	// enabled — see UpdateCheckEnabled.
	Check *bool `yaml:"check,omitempty"`
}

// Path returns $XDG_CONFIG_HOME/kute/config.yaml, or
// ~/.config/kute/config.yaml when XDG_CONFIG_HOME is unset (or relative,
// which the XDG spec says to ignore) — matching state.Path's use of
// XDG_STATE_HOME. A config that only exists at the ~/.config path keeps
// being used, for reads and ctrl+p's writes alike, until one is created
// under XDG_CONFIG_HOME: builds before this honoured only the home path,
// and a user's prodContexts must never silently stop applying.
func Path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	legacy := filepath.Join(home, ".config", "kute", "config.yaml")
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		return legacy
	}
	path := filepath.Join(dir, "kute", "config.yaml")
	if path != legacy {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			if _, err := os.Stat(legacy); err == nil {
				return legacy
			}
		}
	}
	return path
}

// Load reads Path(). A missing file yields the zero value (nothing is prod)
// — an absent config file must never block startup. An unreadable or
// partly unparsable one never blocks startup either, but it never fails
// open: every field that decodes is kept, the problem is carried on
// LoadErr, and if prodContexts itself is the casualty every context reads
// as PROD (see IsProd).
func Load() Config {
	return loadFrom(Path())
}

func loadFrom(path string) Config {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}
	}
	if err != nil {
		return Config{LoadErr: fmt.Errorf("read %s: %w", path, err), prodUnknown: true}
	}
	var c Config
	err = yaml.Unmarshal(data, &c)
	if err == nil {
		return c
	}
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		// A syntax error: nothing in the file can be trusted.
		return Config{LoadErr: fmt.Errorf("parse %s: %w", path, err), prodUnknown: true}
	}
	// A type error: yaml.v3 has already decoded every field it could into
	// c. Whether prodContexts is among the casualties is answered by
	// decoding it alone — any error there (including one bad element of an
	// otherwise valid list, which yaml.v3 would silently drop) means the
	// list can't be trusted.
	c.LoadErr = fmt.Errorf("parse %s: %w", path, err)
	var prod struct {
		ProdContexts StringList `yaml:"prodContexts"`
	}
	if yaml.Unmarshal(data, &prod) != nil {
		c.prodUnknown = true
	}
	return c
}

// ProdUnknown reports whether prodContexts couldn't be read, so every
// context is being treated as PROD until the file is fixed.
func (c Config) ProdUnknown() bool {
	return c.prodUnknown
}

// UpdateCheckEnabled reports whether the ambient release-feed check (28a)
// should run at all — true unless update.check is explicitly set to false.
func (c Config) UpdateCheckEnabled() bool {
	return !c.updateCheckOff && (c.Update.Check == nil || *c.Update.Check)
}

// IsProd reports whether contextName is listed under prodContexts — or,
// when prodContexts couldn't be read at all, true for every context.
func (c Config) IsProd(contextName string) bool {
	return c.prodUnknown || slices.Contains(c.ProdContexts, contextName)
}

// SetProd adds or removes contextName from prodContexts on disk and mirrors
// the result in memory — the write-side counterpart to IsProd, backing 7a's
// ctrl+p mark/unmark-prod key (docs/design README.md §7a). A no-op (no
// write) when the context's in-memory status already matches prod. Every
// other kute session reads the same file, so this is the one place that
// status can change short of hand-editing the YAML.
//
// The write is a surgical read-modify-write of prodContexts alone against
// the file as it is *now* (see writeProdContexts): every other key, comment
// and unknown field — and any edit made since this session loaded it, by
// hand or by another kute — survives, and nothing held only in memory (a
// per-invocation flag such as --no-update-check) can reach the file.
//
// SetProd refuses (ErrUnparsable, nothing written) when the file failed to
// load, or fails to parse at write time: kute never rewrites a file it can't
// read in full.
func (c *Config) SetProd(contextName string, prod bool) error {
	if c.LoadErr != nil {
		return fmt.Errorf("%w: %w", ErrUnparsable, c.LoadErr)
	}
	if prod == c.IsProd(contextName) {
		return nil
	}
	list, err := writeProdContexts(Path(), func(list StringList) StringList {
		list = slices.DeleteFunc(list, func(s string) bool { return s == contextName })
		if prod {
			list = append(list, contextName)
		}
		return list
	})
	if err != nil {
		return err
	}
	c.ProdContexts = list
	return nil
}

// ErrUnparsable is SetProd's refusal to overwrite a config file that doesn't
// parse cleanly.
var ErrUnparsable = errors.New("config.yaml has errors; fix it by hand before changing PROD contexts")

// DisableUpdateCheck turns the update check off for this process only
// (--no-update-check). It lives outside the persisted fields, and SetProd
// never marshals Config anyway, so the flag can't leak into config.yaml.
func (c *Config) DisableUpdateCheck() {
	c.updateCheckOff = true
}

// writeProdContexts re-reads path, applies edit to its prodContexts list and
// writes the file back atomically, touching nothing else: the document is
// round-tripped as a yaml.Node, so comments, key order and keys this build
// doesn't know survive. An absent file (and its directory) is created — SetProd
// may be the first thing ever to persist config for a user who never wrote
// one. An empty result removes the key rather than leaving `prodContexts: []`.
// It returns the list as written.
func writeProdContexts(path string, edit func(StringList) StringList) (StringList, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved // write through a dotfile-manager symlink, don't replace it
	}
	mode := fs.FileMode(0o600)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if fi, statErr := os.Stat(path); statErr == nil {
			mode = fi.Mode().Perm()
		}
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %w", ErrUnparsable, path, err)
	}
	// A file with no document at all (empty, or comments only — yaml.v3
	// keeps no node for those comments) gets a fresh mapping, written after
	// the original bytes so the comments stay.
	var prefix []byte
	if doc.Kind == 0 || len(doc.Content) == 0 {
		if prefix = bytes.TrimRight(data, " \t\r\n"); len(prefix) > 0 {
			prefix = append(prefix, '\n')
		}
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		*root = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: root.HeadComment, LineComment: root.LineComment, FootComment: root.FootComment}
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: %s is not a YAML mapping", ErrUnparsable, path)
	}

	idx := -1
	var list StringList
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "prodContexts" {
			idx = i
			if err := root.Content[i+1].Decode(&list); err != nil {
				return nil, fmt.Errorf("%w: parse %s: prodContexts: %w", ErrUnparsable, path, err)
			}
			break
		}
	}
	before := slices.Clone(list)
	list = edit(list)
	if slices.Equal(before, list) {
		return list, nil
	}

	switch {
	case len(list) == 0 && idx >= 0:
		root.Content = slices.Delete(root.Content, idx, idx+2)
	case len(list) > 0:
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		var kept []*yaml.Node
		if idx >= 0 {
			old := root.Content[idx+1]
			if old.Kind == yaml.SequenceNode {
				// Edit the list in place: its flow/block style, its
				// comments and every surviving entry's comments stay.
				seq, kept = old, old.Content
			} else if key := root.Content[idx]; key.LineComment == "" {
				// A lone scalar becomes a block list; its trailing
				// comment moves to the key's line, where it was.
				key.LineComment = old.LineComment
			}
		}
		seq.Content = nil
		for _, s := range list {
			i := slices.IndexFunc(kept, func(n *yaml.Node) bool { return n.Value == s })
			if i >= 0 {
				seq.Content = append(seq.Content, kept[i])
				kept = slices.Delete(kept, i, i+1)
			} else {
				seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s})
			}
		}
		if idx >= 0 {
			root.Content[idx+1] = seq
		} else {
			key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "prodContexts"}
			root.Content = append(root.Content, key, seq)
		}
	}

	buf := bytes.NewBuffer(prefix)
	enc := yaml.NewEncoder(buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, buf.Bytes(), mode); err != nil {
		return nil, err
	}
	return list, nil
}

// writeFileAtomic replaces path with data via a temp file in the same
// directory, fsynced and renamed into place, so a crash or full disk leaves
// either the old file or the new one — never a truncated one.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config.yaml.*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
