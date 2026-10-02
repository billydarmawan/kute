package tui_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoTaskSwitchesOnBareConnState guards the message type: the root fans
// out kube.ConnStateMsg, and nothing ever sends a bare kube.ConnState as a
// tea.Msg. A `case kube.ConnState:` in a type switch therefore never fires,
// silently leaving that screen "connected" through every outage — which is
// how fluxdetail and certchain went offline-blind.
func TestNoTaskSwitchesOnBareConnState(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	err := filepath.WalkDir("tasks", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSwitchStmt)
			if !ok {
				return true
			}
			for _, stmt := range ts.Body.List {
				clause, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range clause.List {
					sel, ok := expr.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "kube" && sel.Sel.Name == "ConnState" {
						t.Errorf("%s: type switch matches kube.ConnState; the root sends kube.ConnStateMsg", fset.Position(clause.Pos()))
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
