package tui_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// hyphenChord matches a chord spelled "ctrl-x"/"alt-x". Rendered key text
// uses one spelling, the verb registry's "ctrl+x" (which is also what
// tea.KeyPressMsg.String() reports), so a keybar, help row and on-screen
// note never show the same chord two ways.
var hyphenChord = regexp.MustCompile(`\b(ctrl|alt)-[a-z]\b`)

// TestRenderedChordsUseOneSpelling scans every string literal outside tests
// under internal/ for the hyphen spelling.
func TestRenderedChordsUseOneSpelling(t *testing.T) {
	root := filepath.Join("..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && hyphenChord.MatchString(s) {
				t.Errorf("%s: %q spells a chord with '-'; use the registry's '+' spelling", fset.Position(lit.Pos()), s)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
