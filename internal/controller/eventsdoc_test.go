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
// `GovernanceLost` (#316), then `PolicyConflict` and
// `ProvisioningEstimateAboveReadyTimeout` (#317). Nothing tied the tables to the
// source, so an Event added without a row looked exactly like one that needed no
// row.
//
// It is the §4.3 counterpart of internal/schedule's TestFindingCodesAreClassified
// and works the same way: derive the answer from the source rather than from a
// second hand-maintained list, so the test cannot go stale alongside the thing it
// checks. The two sets must match exactly. A reason the tables omit is an Event
// an operator cannot look up; a reason only the tables carry is a row left behind
// by an Event that no longer exists — and, because the source set deliberately
// excludes Fatal findings, it also catches a Fatal code documented as an Event,
// which §4.3 itself says it is not.
//
// If this test fails: add (or remove) the row in BOTH docs/specification and
// docs/ja/specification, never in one alone.
//
// The CI change classifier (.github/scripts/detect-ci-changes.sh) treats the two
// 04-operations.md files as Go changes for the same reason, so a docs-only edit to
// a table cannot skip the job that runs this.
func TestEveryEventReasonIsInTheOperationsTable(t *testing.T) {
	want := emittableReasons(t)
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

// emittableReasons is every reason string that can reach an Event: the reason*
// constants the controller emits directly, plus the Warn schedule findings that
// EmitFindings turns into Events. Fatal findings are excluded deliberately —
// §4.3 states they are not Events but block rotation start.
func emittableReasons(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, r := range reasonConstants(t, ".") {
		out[r] = true
	}
	for _, c := range warnFindingCodes(t, filepath.Join("..", "schedule")) {
		out[c] = true
	}
	if len(out) == 0 {
		t.Fatal("found no Event reasons in the source; did the reason* naming or the Finding shape change?")
	}
	return out
}

// reasonConstants collects every `reasonX = "Y"` string constant declared in the
// package's non-test files. The action* constants next to them name the operation
// rather than the condition, and are not Event reasons.
func reasonConstants(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, path := range goSourceFiles(t, dir) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
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
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s in %s: %v", spec.Names[0].Name, path, err)
			}
			out = append(out, v)
			return true
		})
	}
	return out
}

// warnFindingCodes collects the Code of every Finding literal under dir whose
// Severity is Warn.
//
// It matches on the Severity/Code fields rather than on the literal's type,
// because the findings are written both as Finding{...} and, inside a
// []Finding{...} slice, as an elided {...} whose type the AST does not carry.
// Requiring the type silently dropped two of them — including the very finding
// #317 was filed about, which would have made this test pass vacuously.
func warnFindingCodes(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, path := range goSourceFiles(t, dir) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			var severity, code string
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "Severity":
					if id, ok := kv.Value.(*ast.Ident); ok {
						severity = id.Name
					}
				case "Code":
					if b, ok := kv.Value.(*ast.BasicLit); ok && b.Kind == token.STRING {
						v, err := strconv.Unquote(b.Value)
						if err != nil {
							t.Fatalf("unquote Code in %s: %v", path, err)
						}
						code = v
					}
				}
			}
			if code == "" {
				return true
			}
			// A finding whose severity this walk cannot see would be dropped
			// silently, and a dropped Warn is exactly the hole #317 describes.
			switch severity {
			case "Warn":
				out = append(out, code)
			case "Fatal": // not an Event, by §4.3
			default:
				t.Errorf("%s: finding %q has severity %q; this walk reads only inline Warn/Fatal, so it would be dropped", path, code, severity)
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("found no Warn Finding codes under %s; did the Finding shape change?", dir)
	}
	return out
}

// reasonsInEventTable reads the backticked identifiers out of the Reason column
// of the Events table in path. The table is located by its header rather than by
// line number, and only the Reason cell is read — the When cell backticks things
// like `spec.replicas` and `Normal`, which are not reasons.
func reasonsInEventTable(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]bool{}
	inTable := false
	for _, line := range strings.Split(string(b), "\n") {
		cells := tableCells(line)
		switch {
		case len(cells) < 2:
			if inTable {
				return out // the table ended
			}
		case !inTable:
			// The header row naming the Reason column starts the table; both
			// translations keep "Reason" as that column's name.
			inTable = cells[1] == "Reason"
		case strings.HasPrefix(cells[0], "---"):
			// the header/body separator
		default:
			for _, r := range strings.Split(cells[1], ",") {
				if r = strings.Trim(strings.TrimSpace(r), "`"); r != "" {
					out[r] = true
				}
			}
		}
	}
	if !inTable {
		t.Fatalf("%s: found no Events table (no header row naming a Reason column)", path)
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

// goSourceFiles lists dir's non-test Go files.
func goSourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	if len(out) == 0 {
		t.Fatalf("no Go source files under %s", dir)
	}
	return out
}
