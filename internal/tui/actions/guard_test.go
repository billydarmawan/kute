package actions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kute-dev/kute/internal/tui/verbs"
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

// neverConfirms are the controller screens with no confirm to route: every
// verb they Begin (keyed by its verbs.X identifier) resolves TierNone in
// every context, in both directions, so the controller never goes Active.
// TestEveryControllerScreenRoutesConfirmKeys checks that claim against
// verbs.Resolve and against the verbs each package actually Begins — Begin
// a tiered verb there and the screen must route through HandleConfirmKey.
var neverConfirms = map[string]map[string]verbs.Verb{
	"fluxtree":        {"FluxReconcile": verbs.FluxReconcile, "FluxSuspend": verbs.FluxSuspend},
	"fluxdetail":      {"FluxReconcile": verbs.FluxReconcile, "FluxSuspend": verbs.FluxSuspend},
	"cronjobschedule": {"CronJobSetSchedule": verbs.CronJobSetSchedule, "CronJobScheduleUndo": verbs.CronJobScheduleUndo},
}

// routerOnly are the Controller methods only HandleConfirmKey may call: a
// screen calling one is hand-routing confirm keys again.
var routerOnly = []string{"Confirm", "HandleTypeKey", "EscalateForceDelete"}

// confirmKeyLiterals are the y/N answers as bare strings — a screen
// matching one is a hand-written confirm router.
var confirmKeyLiterals = map[string]bool{`"y"`: true, `"n"`: true, `"Y"`: true, `"N"`: true}

// TestEveryControllerScreenRoutesConfirmKeys is review 10 L5's guard: the
// y/N, type-the-name and force-delete routing lived as a dozen hand-copied
// switches with literal keys and hand-built {y confirm}{esc cancel} hints,
// which is how ctrl+k came to be advertised under one key and handled under
// another, and how one screen came to advertise n where the rest say esc.
// Every controller screen must (1) hand its confirm keys to
// HandleConfirmKey, (2) never call the router's own Confirm/HandleTypeKey/
// EscalateForceDelete, (3) never match a bare "y"/"n" (a bespoke gate uses
// verbs.ConfirmAccept/ConfirmDecline/ConfirmCancel); and no task screen may
// (4) hand-build a "confirm" key hint (Controller.ConfirmHints or
// verbs.ConfirmHints). Screens that only match "y" as a copy key (yamlview,
// update) embed no controller, so (3) leaves them alone.
func TestEveryControllerScreenRoutesConfirmKeys(t *testing.T) {
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
		name := e.Name()
		pkg := parsePackage(t, filepath.Join(root, name))
		for _, where := range confirmHintLiterals(pkg) {
			t.Errorf("%s: %s hand-builds a confirm key hint — use Controller.ConfirmHints or verbs.ConfirmHints", name, where)
		}
		fields := controllerFields(pkg)
		if len(fields) == 0 {
			continue
		}
		checked++
		if allowed, ok := neverConfirms[name]; ok {
			checkNeverConfirms(t, name, pkg, allowed)
		} else if !callsMethod(pkg, fields, "HandleConfirmKey") {
			t.Errorf("%s embeds actions.Controller (%v) but never routes keys to HandleConfirmKey() — "+
				"its confirm would be a hand-written router", name, fields)
		}
		for _, method := range routerOnly {
			if callsMethod(pkg, fields, method) {
				t.Errorf("%s calls the controller's %s() directly — route confirm keys through HandleConfirmKey()", name, method)
			}
		}
		for _, where := range confirmKeyMatches(pkg) {
			t.Errorf("%s: %s matches a bare y/n key — use HandleConfirmKey, or verbs.ConfirmAccept/ConfirmDecline/ConfirmCancel in a bespoke gate", name, where)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d controller screens found — has internal/tui/tasks moved?", checked)
	}
}

// checkNeverConfirms verifies a neverConfirms entry: every verb the
// package Begins is listed, and each listed verb resolves TierNone
// everywhere.
func checkNeverConfirms(t *testing.T, name string, pkg []*ast.File, allowed map[string]verbs.Verb) {
	t.Helper()
	for _, v := range allowed {
		for _, prod := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				if p := verbs.Resolve(v, verbs.Target{Reverse: reverse}, prod); p.Tier != TierNone {
					t.Errorf("%s is in neverConfirms but %s resolves tier %v (prod=%v reverse=%v) — "+
						"route its keys through HandleConfirmKey and drop it from the list", name, v.ID, p.Tier, prod, reverse)
				}
			}
		}
	}
	begun := requestedVerbs(pkg)
	// A shared non-task package (jobactions, metapanel) builds the
	// Request{Verb: …} a screen hands to Begin, so a screen importing one
	// may Begin every verb that package names.
	for _, dir := range sharedTUIImports(pkg) {
		begun = append(begun, requestedVerbs(parsePackage(t, filepath.Join("..", dir)))...)
	}
	if len(begun) == 0 {
		t.Errorf("%s is in neverConfirms but Begins no verb — has its Request{Verb: …} moved?", name)
	}
	for _, verb := range begun {
		if _, ok := allowed[verb]; !ok {
			t.Errorf("%s is in neverConfirms but Begins verbs.%s, which isn't listed — "+
				"route its keys through HandleConfirmKey, or list the verb if it never confirms", name, verb)
		}
	}
}

// sharedTUIImports names the internal/tui subpackages (relative to
// internal/tui) pkg imports, other than tasks, actions and verbs themselves.
func sharedTUIImports(pkg []*ast.File) []string {
	const prefix = "github.com/kute-dev/kute/internal/tui/"
	seen := map[string]bool{}
	var out []string
	for _, f := range pkg {
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			rel, ok := strings.CutPrefix(path, prefix)
			if !ok || seen[rel] || rel == "actions" || rel == "verbs" || strings.HasPrefix(rel, "tasks/") {
				continue
			}
			seen[rel] = true
			out = append(out, rel)
		}
	}
	return out
}

// requestedVerbs names every verbs.X a Request{Verb: …} literal carries.
func requestedVerbs(pkg []*ast.File) []string {
	var out []string
	for _, f := range pkg {
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if sel, ok := cl.Type.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Request" {
				return true
			}
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Verb" {
					continue
				}
				if sel, ok := kv.Value.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "verbs" {
						out = append(out, sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	return out
}

// inFuncs runs visit over every function body in pkg, naming the function.
func inFuncs(pkg []*ast.File, visit func(fn string, n ast.Node)) {
	for _, f := range pkg {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				visit(fn.Name.Name, n)
				return true
			})
		}
	}
}

// confirmKeyMatches finds a bare "y"/"n" in a switch case or an ==/!=
// comparison.
func confirmKeyMatches(pkg []*ast.File) []string {
	var out []string
	inFuncs(pkg, func(fn string, n ast.Node) {
		var exprs []ast.Expr
		switch n := n.(type) {
		case *ast.CaseClause:
			exprs = n.List
		case *ast.BinaryExpr:
			if n.Op == token.EQL || n.Op == token.NEQ {
				exprs = []ast.Expr{n.X, n.Y}
			}
		}
		for _, e := range exprs {
			if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING && confirmKeyLiterals[lit.Value] {
				out = append(out, fn+"'s "+lit.Value)
			}
		}
	})
	return out
}

// confirmHintLiterals finds a composite literal with a Label: "confirm…"
// field — a hand-built tui.KeyHint for a confirm's accept key.
func confirmHintLiterals(pkg []*ast.File) []string {
	var out []string
	inFuncs(pkg, func(fn string, n ast.Node) {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Label" {
				continue
			}
			lit, ok := kv.Value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(v, "confirm") {
				out = append(out, fn+"'s {Label: "+lit.Value+"}")
			}
		}
	})
	return out
}
