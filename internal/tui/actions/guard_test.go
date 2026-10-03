package actions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryControllerScreenRendersResults is review 10 H1's guard: a
// screen that embeds an actions.Controller but never renders its Result()
// makes every failed delete/drain/cordon/restart look exactly like a
// success. Every task package with a Controller field must (1) implement
// tui.ActionResulter by returning the controller's Result() — tui.Frame
// renders it on the keybar — (2) DismissResult on a key press, so the
// line lasts until the user acts, and (3) route actions.RunningMsg to
// HandleRunning (review 10 L6): a slow write's ResultMsg arrives through
// the command HandleRunning returns, so a screen that drops it never hears
// how the write ended. Static on purpose: a new screen fails
// here before anyone has to remember to write its own failed-result test.
func TestEveryControllerScreenRendersResults(t *testing.T) {
	root := filepath.Join("..", "tasks")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkg := parsePackage(t, filepath.Join(root, e.Name()))
		fields := controllerFields(pkg)
		if len(fields) == 0 {
			continue
		}
		checked++
		if !hasActionResult(pkg, fields) {
			t.Errorf("%s embeds actions.Controller (%v) but has no ActionResult() returning its Result() — "+
				"a failed mutation would render like a success", e.Name(), fields)
		}
		if !callsMethod(pkg, fields, "HandleRunning") {
			t.Errorf("%s embeds actions.Controller (%v) but never routes actions.RunningMsg to HandleRunning() — "+
				"a slow write's result would never arrive", e.Name(), fields)
		}
		if !callsMethod(pkg, fields, "DismissResult") {
			t.Errorf("%s embeds actions.Controller (%v) but never calls DismissResult() — "+
				"the result line would never clear", e.Name(), fields)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d controller screens found — has internal/tui/tasks moved?", checked)
	}
}

// parsePackage parses dir's non-test Go files.
func parsePackage(t *testing.T, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		files = append(files, f)
	}
	return files
}

// controllerFields names every struct field typed actions.Controller.
func controllerFields(pkg []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range pkg {
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				sel, ok := fld.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Controller" {
					continue
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "actions" {
					continue
				}
				for _, name := range fld.Names {
					out[name.Name] = true
				}
			}
			return true
		})
	}
	return out
}

// fieldCall reports whether n is a call <recv>.<field>.<method>().
func fieldCall(n ast.Node, fields map[string]bool, method string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && fields[inner.Sel.Name]
}

func hasActionResult(pkg []*ast.File, fields map[string]bool) bool {
	for _, f := range pkg {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "ActionResult" || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if fieldCall(n, fields, "Result") {
					found = true
				}
				return !found
			})
			if found {
				return true
			}
		}
	}
	return false
}

func callsMethod(pkg []*ast.File, fields map[string]bool, method string) bool {
	found := false
	for _, f := range pkg {
		ast.Inspect(f, func(n ast.Node) bool {
			if fieldCall(n, fields, method) {
				found = true
			}
			return !found
		})
	}
	return found
}

// resolveOutsideBegin are the only confirms that resolve verbs.Resolve
// themselves instead of through Controller.Begin, each because it never
// reaches kube.Mutator through the controller: the kubectl edit/debug tty
// handoffs (a bespoke PROD y/N before tea.ExecProcess).
var resolveOutsideBegin = map[string]bool{
	"tasks/browse/edit.go":       true,
	"tasks/poddetail/edit.go":    true,
	"tasks/nodedetail/edit.go":   true,
	"tasks/debugpanel/update.go": true,
}

// TestConfirmPolicyOnlyResolvedByBegin is review 10 M1's guard: the confirm
// tier is decided in one place. Begin takes the verb, not a tier, so a call
// site can't pass a wrong one; this catches the other way around it — a
// screen calling verbs.Resolve (or building a verbs.Policy) to make its own
// decision. A new bespoke gate needs a reason and an entry above.
func TestConfirmPolicyOnlyResolvedByBegin(t *testing.T) {
	root := ".."
	var dirs []string
	for _, pattern := range []string{"tasks/*", "*"} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, matches...)
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		rel, _ := filepath.Rel(root, dir)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() ||
			rel == "verbs" || rel == "actions" || rel == "tasks" {
			continue
		}
		paths, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			file, _ := filepath.Rel(root, path)
			file = filepath.ToSlash(file)
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Resolve" && sel.Sel.Name != "Policy") {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "verbs" {
					return true
				}
				seen[file] = true
				if !resolveOutsideBegin[file] {
					t.Errorf("%s uses verbs.%s directly — start the action with Controller.Begin(Request{Verb: …}) "+
						"so the policy is resolved in one place", file, sel.Sel.Name)
				}
				return true
			})
		}
	}
	for file := range resolveOutsideBegin {
		if !seen[file] {
			t.Errorf("%s is allow-listed but no longer calls verbs.Resolve — drop it from resolveOutsideBegin", file)
		}
	}
}
