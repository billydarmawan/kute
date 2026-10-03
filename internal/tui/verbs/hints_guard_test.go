package verbs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// handWrittenKeyAllowed lists the KeyHint literals whose Key is not a key at
// all, by file (relative to internal/tui) and literal. Keep each entry to
// one line of why.
var handWrittenKeyAllowed = map[string]string{
	// The type-the-name modal's instruction ("type name · then ↵") — prose
	// in the key slot, not a key press.
	`tasks/debugpanel/keys.go:"type name"`: "type-the-name instruction, not a key",
}

// TestNoHandWrittenKeyHints is review 13 L15's guard (CLAUDE.md: "Every
// verb goes through the command registry … never hand-wire a keybinding in
// a view"). A tui.KeyHint whose Key is a string literal — or the bare ↹
// glyph — advertises a key nothing ties to the handler that answers it, so
// the two can drift apart silently; that's how a keybar came to show one
// key while the screen answered another. Take the key from a verbs entry
// (Hint, HintAs, Either) instead, and match the handler on the same entry.
func TestNoHandWrittenKeyHints(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..")
	seenAllowed := map[string]bool{}
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "verbs/") {
			return nil // the registry itself is where keys are spelled
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		for _, lit := range keyHintLiterals(f) {
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := kv.Key.(*ast.Ident); !ok || id.Name != "Key" {
					continue
				}
				bad := handSpelledKey(kv.Value)
				if bad == "" {
					continue
				}
				id := rel + ":" + bad
				if _, ok := handWrittenKeyAllowed[id]; ok {
					seenAllowed[id] = true
					continue
				}
				t.Errorf("%s: KeyHint with hand-written Key %s — take it from a verbs registry entry (Hint/HintAs/Either) and match the handler on the same entry",
					fset.Position(kv.Pos()), bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("only %d files scanned — has internal/tui moved?", scanned)
	}
	for id := range handWrittenKeyAllowed {
		if !seenAllowed[id] {
			t.Errorf("allow-list entry %s no longer matches anything — remove it", id)
		}
	}
}

// keyHintLiterals returns every composite literal in f that builds a
// KeyHint: written out (tui.KeyHint{…}, KeyHint{…}) or elided inside a
// []KeyHint / [][]KeyHint literal.
func keyHintLiterals(f *ast.File) []*ast.CompositeLit {
	var out []*ast.CompositeLit
	var visit func(lit *ast.CompositeLit, depth int)
	visit = func(lit *ast.CompositeLit, depth int) {
		if depth == 0 {
			out = append(out, lit)
			return
		}
		for _, elt := range lit.Elts {
			if child, ok := elt.(*ast.CompositeLit); ok && child.Type == nil {
				visit(child, depth-1)
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || lit.Type == nil {
			return true
		}
		if depth, ok := keyHintDepth(lit.Type); ok {
			visit(lit, depth)
		}
		return true
	})
	return out
}

// keyHintDepth reports how many slice levels wrap KeyHint in typ.
func keyHintDepth(typ ast.Expr) (int, bool) {
	switch t := typ.(type) {
	case *ast.Ident:
		return 0, t.Name == "KeyHint"
	case *ast.SelectorExpr:
		return 0, t.Sel.Name == "KeyHint"
	case *ast.ArrayType:
		d, ok := keyHintDepth(t.Elt)
		return d + 1, ok
	}
	return 0, false
}

// handSpelledKey returns the first string literal inside a Key expression,
// or "GlyphTab" when the expression is the bare tab glyph; "" when the key
// comes from somewhere else (a verbs entry, a computed buffer value).
func handSpelledKey(e ast.Expr) string {
	if sel, ok := e.(*ast.SelectorExpr); ok && sel.Sel.Name == "GlyphTab" {
		return "GlyphTab"
	}
	found := ""
	ast.Inspect(e, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				found = strconv.Quote(s)
			} else {
				found = lit.Value
			}
		}
		return true
	})
	return found
}

// TestEveryDeclaredVerbIsInAll catches an entry declared but never added to
// All (or screenVerbs) — ByID, the site's keyboard-reference check and the
// policy tests would all silently skip it.
func TestEveryDeclaredVerbIsInAll(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	declared := map[string]bool{}
	listed := map[string]bool{}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					switch name.Name {
					case "All", "screenVerbs":
						ast.Inspect(vs.Values[i], func(n ast.Node) bool {
							if id, ok := n.(*ast.Ident); ok {
								listed[id.Name] = true
							}
							return true
						})
						continue
					}
					if lit, ok := vs.Values[i].(*ast.CompositeLit); ok {
						if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "Verb" {
							declared[name.Name] = true
						}
					}
				}
			}
		}
	}
	if len(declared) < 50 {
		t.Fatalf("only %d verbs declared — has the registry moved?", len(declared))
	}
	for name := range declared {
		if !listed[name] {
			t.Errorf("verb %s is declared but missing from All", name)
		}
	}
}
