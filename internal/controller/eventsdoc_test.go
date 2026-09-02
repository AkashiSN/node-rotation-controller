package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// specEventTables are the §4.3 "Kubernetes Events" tables — the operator-facing
// enumeration of every Event this controller emits, and the list an operator
// reads to decide what to alert on. English is canonical and Japanese is a
// mandatory translation, so both must name the same Events.
var specEventTables = []string{
	filepath.Join("..", "..", "docs", "specification", "04-operations.md"),
	filepath.Join("..", "..", "docs", "ja", "specification", "04-operations.md"),
}

// TestEveryEventReasonIsInTheOperationsTable guards the §4.3 tables against the
// drift that put this test here: three rows went missing from them one at a
// time, each shipped with the Event and each noticed by hand much later —
// GovernanceLost (#316), then PolicyConflict and
// ProvisioningEstimateAboveReadyTimeout (#317). Nothing tied the tables to the
// source, so an Event added without a row looked exactly like one that needed no
// row.
//
// It is the §4.3 counterpart of internal/schedule's TestFindingCodesAreClassified
// and works the same way: derive the answer from the source rather than from a
// second hand-maintained list, so the test cannot go stale alongside the thing it
// checks. The two sets must match exactly. A reason the tables omit is an Event
// an operator cannot look up; a reason only the tables carry is a row left behind
// by an Event that no longer exists — and, because the emitted set covers Warn
// findings only, it also catches a Fatal code documented as an Event, which §4.3
// itself says it is not.
//
// If this test fails: add (or remove) the row in BOTH docs/specification and
// docs/ja/specification, never in one alone.
//
// The CI change classifier (.github/scripts/detect-ci-changes.sh) treats the two
// 04-operations.md files as Go changes for the same reason, so a docs-only edit to
// a table cannot skip the job that runs this.
func TestEveryEventReasonIsInTheOperationsTable(t *testing.T) {
	want := emittedReasons(t)
	for _, path := range specEventTables {
		got := reasonsInEventTable(t, path)
		for r := range want {
			if !got[r] {
				t.Errorf("%s: Event reason %q is emitted but has no row in the §4.3 table", path, r)
			}
		}
		for r := range got {
			if !want[r] {
				t.Errorf("%s: the §4.3 table names %q, which the controller never emits as an Event", path, r)
			}
		}
	}
}

// emittedReasons is every reason string that can reach an Event, read from the
// Eventf call sites themselves rather than from the reason* constants beside
// them. The constants are only the vocabulary: one that no longer reaches an
// Eventf is not an Event, and a call site that passes something else still is
// (issue #317). Reading the calls keeps both directions honest.
//
// Three argument forms are understood, and anything else fails rather than being
// skipped — a form this walk cannot read is exactly how a reason goes missing:
//
//   - a reason* constant, resolved to its value;
//   - a string literal, taken as-is;
//   - <ident>.Code inside EmitFindings, which stands for every Warn finding that
//     function turns into an Event. Fatal findings are excluded deliberately:
//     §4.3 states they are not Events but block rotation start.
//
// The .Code form is pinned to EmitFindings rather than accepted from any
// receiver, because without type information `.Code` alone does not say the value
// came from a Finding: a future policy.Code would otherwise be silently read as
// the whole Warn set, and its own missing row would pass. Renaming EmitFindings
// fails this test, which is the intended prompt to re-check what the guard reads.
func emittedReasons(t *testing.T) map[string]bool {
	t.Helper()
	consts := reasonConstants(t, ".")
	warn := warnFindingCodes(t, filepath.Join("..", "schedule"))

	out := map[string]bool{}
	calls := 0
	for path, file := range parseDir(t, ".") {
		fset := file.fset
		for _, decl := range file.f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			enclosing := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Eventf" {
					return true
				}
				calls++
				// Eventf(object, related, eventtype, reason, action, note, args...)
				const reasonArg = 3
				if len(call.Args) <= reasonArg {
					t.Errorf("%s: Eventf call with %d args; did the reason argument move?",
						fset.Position(call.Pos()), len(call.Args))
					return true
				}
				switch v := call.Args[reasonArg].(type) {
				case *ast.Ident:
					value, known := consts[v.Name]
					if !known {
						t.Errorf("%s: Eventf reason %s is not a reason* string constant of this package, so this guard cannot tell which Event it raises",
							fset.Position(v.Pos()), v.Name)
						return true
					}
					out[value] = true
				case *ast.BasicLit:
					if v.Kind != token.STRING {
						t.Errorf("%s: Eventf reason is a non-string literal", fset.Position(v.Pos()))
						return true
					}
					out[mustUnquote(t, path, v.Value)] = true
				case *ast.SelectorExpr:
					_, plainReceiver := v.X.(*ast.Ident)
					if v.Sel.Name != "Code" || !plainReceiver || enclosing != "EmitFindings" {
						t.Errorf("%s: Eventf reason is a field selector this guard does not understand; only a Finding's .Code inside EmitFindings stands for the Warn findings",
							fset.Position(v.Pos()))
						return true
					}
					for _, c := range warn {
						out[c] = true
					}
				default:
					t.Errorf("%s: Eventf reason is an expression this guard cannot read; pass a reason* constant so the Event stays checkable",
						fset.Position(call.Args[reasonArg].Pos()))
				}
				return true
			})
		}
	}
	if calls == 0 {
		t.Fatal("found no Eventf calls; did the recorder's method name change?")
	}
	if len(out) == 0 {
		t.Fatal("found no Event reasons at the Eventf call sites")
	}
	return out
}

// reasonConstants maps every `reasonX = "Y"` string constant declared in the
// package's non-test files to its value. It is the lookup table for resolving an
// Eventf argument, not the answer itself.
func reasonConstants(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for path, file := range parseDir(t, dir) {
		ast.Inspect(file.f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 {
				return true
			}
			if !strings.HasPrefix(spec.Names[0].Name, "reason") {
				return true
			}
			lit, ok := spec.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			out[spec.Names[0].Name] = mustUnquote(t, path, lit.Value)
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("found no reason* string constants under %s", dir)
	}
	return out
}

// warnFindingCodes is the Code of every Warn Finding declared under dir.
//
// It reads the two shapes the findings are actually written in — a typed
// Finding{...} and, inside a []Finding{...}, an elided {...} the AST gives no
// type — and fails on anything else rather than skipping it. That is not
// defensiveness: the first version of this walk required the type and silently
// dropped both elided findings, one of which was the very finding #317 documents,
// so the test would have been green while the gap it guards was still open. A
// severity or code this walk cannot read now fails loudly for the same reason.
func warnFindingCodes(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, file := range parseDir(t, dir) {
		fset := file.fset
		read := func(lit *ast.CompositeLit) {
			if severity, code, ok := findingSeverityAndCode(t, fset, lit); ok && severity == "Warn" {
				out = append(out, code)
			}
		}
		// The elided elements of a []Finding are read through their parent, so
		// they are not the unrecognized literals the fallback below is about.
		viaParent := map[*ast.CompositeLit]bool{}
		ast.Inspect(file.f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			switch typ := lit.Type.(type) {
			case *ast.Ident:
				if typ.Name == "Finding" {
					read(lit)
					return true
				}
			case *ast.ArrayType:
				if id, isIdent := typ.Elt.(*ast.Ident); isIdent && id.Name == "Finding" {
					// Only the elided elements are read here; a typed Finding{...}
					// element is visited on its own by this walk.
					for _, el := range lit.Elts {
						if e, isLit := el.(*ast.CompositeLit); isLit && e.Type == nil {
							viaParent[e] = true
							read(e)
						}
					}
					return true
				}
			}
			// Not a shape this walk reads. Ignoring it silently is how a finding
			// disappears, so a literal that is Finding-shaped — a named type or an
			// alias over Finding, say — fails here instead of vanishing.
			if !viaParent[lit] && hasFindingFields(lit) {
				t.Errorf("%s: Finding-shaped literal of a type this guard does not read; give it the Finding or []Finding form, or teach this walk the new one",
					fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("found no Warn Finding codes under %s; did the Finding shape change?", dir)
	}
	return out
}

// findingSeverityAndCode reads a Finding literal's Severity and Code. It reports
// ok=false — after failing the test — for any literal it cannot read: a
// positional one, a non-identifier severity, a non-literal code, a severity that
// is neither Warn nor Fatal, or a missing field.
func findingSeverityAndCode(t *testing.T, fset *token.FileSet, lit *ast.CompositeLit) (severity, code string, ok bool) {
	t.Helper()
	at := fset.Position(lit.Pos())
	for _, el := range lit.Elts {
		kv, isKV := el.(*ast.KeyValueExpr)
		if !isKV {
			t.Errorf("%s: positional Finding literal; this guard reads keyed Severity/Code only, so its code would go unchecked", at)
			return "", "", false
		}
		key, isIdent := kv.Key.(*ast.Ident)
		if !isIdent {
			continue
		}
		switch key.Name {
		case "Severity":
			id, isIdent := kv.Value.(*ast.Ident)
			if !isIdent {
				t.Errorf("%s: Finding Severity is not a bare Warn/Fatal identifier", at)
				return "", "", false
			}
			severity = id.Name
		case "Code":
			b, isLit := kv.Value.(*ast.BasicLit)
			if !isLit || b.Kind != token.STRING {
				t.Errorf("%s: Finding Code is not a string literal, so this guard cannot tell which Event it raises", at)
				return "", "", false
			}
			code = mustUnquote(t, at.Filename, b.Value)
		}
	}
	if severity == "" || code == "" {
		t.Errorf("%s: Finding literal is missing Severity or Code", at)
		return "", "", false
	}
	if severity != "Warn" && severity != "Fatal" {
		t.Errorf("%s: Finding %q has severity %q, which is neither Warn nor Fatal", at, code, severity)
		return "", "", false
	}
	return severity, code, true
}

// hasFindingFields reports whether a composite literal keys any field a Finding
// has, which is how an unrecognized literal is spotted as one.
func hasFindingFields(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && (key.Name == "Severity" || key.Name == "Code") {
			return true
		}
	}
	return false
}

// reasonsInEventTable reads the backticked identifiers out of the Reason column
// of the Events table in path. The table is found by walking from the
// "Kubernetes Events" heading to the first row naming a Reason column — both
// translations keep that heading and that column name — rather than by line
// number. Only the Reason cell is read: the When cell backticks things like
// `spec.replicas` and `Normal`, which are not reasons.
func reasonsInEventTable(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]bool{}
	var atHeading, inTable bool
	for line := range strings.SplitSeq(string(b), "\n") {
		cells := tableCells(line)
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "#"):
			if inTable {
				return out
			}
			atHeading = strings.Contains(line, "Kubernetes Events")
		case !atHeading:
			// not in the Events section yet
		case len(cells) < 2:
			if inTable {
				return out // the table ended
			}
		case !inTable:
			inTable = cells[1] == "Reason"
		case strings.HasPrefix(cells[0], "---"):
			// the header/body separator
		default:
			for r := range strings.SplitSeq(cells[1], ",") {
				r = strings.Trim(strings.TrimSpace(r), "`")
				if r == "" {
					continue
				}
				if out[r] {
					t.Errorf("%s: the §4.3 table names %q twice", path, r)
				}
				out[r] = true
			}
		}
	}
	if !inTable {
		t.Fatalf("%s: found no Events table under a Kubernetes Events heading", path)
	}
	return out
}

// tableCells splits a Markdown table row into its cells, or returns nil when the
// line is not one.
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	cells := strings.Split(strings.Trim(line, "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// parsedFile is a parsed source file with the FileSet its positions belong to.
type parsedFile struct {
	f    *ast.File
	fset *token.FileSet
}

// parseDir parses dir's non-test Go files, keyed by path.
func parseDir(t *testing.T, dir string) map[string]parsedFile {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]parsedFile{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		out[path] = parsedFile{f: f, fset: fset}
	}
	if len(out) == 0 {
		t.Fatalf("no Go source files under %s", dir)
	}
	return out
}

func mustUnquote(t *testing.T, path, lit string) string {
	t.Helper()
	v, err := strconv.Unquote(lit)
	if err != nil {
		t.Fatalf("unquote %s in %s: %v", lit, path, err)
	}
	return v
}
