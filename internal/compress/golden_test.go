package compress_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yotatameguchi-boop/contextslim/internal/compress"
	"github.com/yotatameguchi-boop/contextslim/internal/parser"
	"github.com/yotatameguchi-boop/contextslim/internal/textutil"
	"github.com/yotatameguchi-boop/contextslim/internal/validate"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Real logs captured from the tools themselves (versions in testdata/README.md).
// Each case lists lines that must survive compression verbatim, on top of
// what the parser itself marks as must-keep.
var cases = []struct {
	log, parser string
	required    []string
	counts      map[string]int
}{
	{"vitest-fail", "vitest", []string{
		"   × pricing > computes total 6ms",
		"     → expected { total: 10, tax: 1 } to deeply equal { total: 11, tax: 1 }",
		" FAIL  vt/fail.test.ts > pricing > throws",
		"TypeError: Cannot read properties of undefined (reading 'id')",
		" ❯ vt/fail.test.ts:4:49",
		`-   "total": 11,`,
		`+   "total": 10,`,
		"   ↓ pricing > later",
		"debug value 42",
	}, map[string]int{"tests_failed": 3, "tests_passed": 97, "tests_skipped": 1, "tests_total": 101, "files_failed": 1, "files_passed": 12, "files_total": 13}},
	{"vitest-pass", "vitest", nil, map[string]int{"tests_passed": 40, "tests_total": 40, "files_passed": 5, "files_total": 5}},
	{"vitest-verbose-fail", "vitest", []string{" × vt/fail.test.ts > pricing > rounds 2ms", " ↓ vt/fail.test.ts > pricing > later"}, nil},
	{"vitest-big-fail", "vitest", []string{" FAIL  vbig/fail.test.ts > pricing > rounds"}, map[string]int{"tests_failed": 3, "tests_passed": 401, "tests_skipped": 1, "tests_total": 405, "files_failed": 1, "files_passed": 40, "files_total": 41}},
	{"jest-fail", "jest", []string{
		"FAIL jt/fail.test.js",
		"  ● pricing › computes total",
		`    Expected: "1.01"`,
		`    Received: "1.00"`,
		"      at Object.toBe (fail.test.js:3:53)",
		"      debug value 42",
	}, map[string]int{"suites_failed": 1, "suites_passed": 12, "suites_total": 13, "tests_failed": 2, "tests_skipped": 1, "tests_passed": 97, "tests_total": 100}},
	{"jest-verbose-fail", "jest", []string{"    ✕ computes total (4 ms)", "    ○ skipped later", "    debug value 42"}, nil},
	{"jest-big-fail", "jest", []string{"  ● pricing › rounds"}, nil},
	{"jest-big-verbose-fail", "jest", []string{"    ✕ rounds (1 ms)"}, nil},
	{"tsc-plain", "tsc", []string{"src/a.ts(7,25): error TS2322: Type 'string' is not assignable to type 'number'."}, map[string]int{"diagnostics": 7, "errors": 7}},
	{"tsc-pretty", "tsc", []string{"src/a.ts:6:7 - error TS2741: Property 'age' is missing in type '{ name: string; }' but required in type 'P'.", "    'age' is declared here.", "Found 7 errors in the same file, starting at: src/a.ts:1"}, map[string]int{"diagnostics": 7, "errors": 7, "found": 7}},
	{"pytest-fail", "pytest", []string{
		"tests/test_fail.py FFsx.F                                                [  3%]",
		"E       AssertionError: assert {'total': 10, 'tax': 1} == {'total': 11, 'tax': 1}",
		"tests/test_fail.py:25: KeyError",
		"FAILED tests/test_fail.py::test_error - KeyError: 'missing-key'",
		"debug value 42",
		"    warnings.warn(\"deprecated api\", DeprecationWarning)",
	}, map[string]int{"failed": 3, "passed": 151, "skipped": 1, "xfailed": 1, "warnings": 1}},
	{"pytest-q-fail", "pytest", []string{"FFsx.F.................................................................. [ 46%]"}, nil},
	{"pytest-v-fail", "pytest", []string{"tests/test_fail.py::test_skip SKIPPED (later)                            [  1%]", "tests/test_fail.py::test_xfail XFAIL                                     [  2%]"}, nil},
	{"pytest-pass", "pytest", nil, map[string]int{"passed": 30}},
	{"pytest-big-fail", "pytest", []string{"FAILED big/test_zfail.py::test_total - AssertionError: assert {'total': 10, '..."}, map[string]int{"failed": 3, "passed": 1001, "skipped": 1, "xfailed": 1, "warnings": 1}},
	{"pytest8-fail", "pytest", []string{"FAILED tests/test_fail.py::test_prints - AssertionError: values differ"}, nil},
	{"pytest8-v-fail", "pytest", nil, nil},
	{"ruff-full", "ruff", []string{"F821 Undefined name `undefined_name`", "  --> pkg/a.py:8:12", "help: Remove unused import: `re`", "Found 7 errors."}, map[string]int{"diagnostics": 7, "found": 7}},
	{"ruff-concise", "ruff", []string{"pkg/a.py:7:5: F841 Local variable `l` is assigned to but never used"}, map[string]int{"diagnostics": 7, "found": 7}},
	{"ruff-ok", "ruff", nil, map[string]int{"diagnostics": 2, "found": 2}},
}

func TestGolden(t *testing.T) {
	reg := parser.Registry()
	for _, c := range cases {
		t.Run(c.log, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "logs", c.log+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			norm, err := textutil.Normalize(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			p := reg[c.parser]
			if !p.Sniff(norm.Lines) {
				t.Fatalf("%s does not sniff its own output", c.parser)
			}
			for name, other := range reg {
				if name != c.parser && name != "tsc" && name != "ruff" && other.Sniff(norm.Lines) {
					t.Errorf("%s also sniffs %s output", name, c.parser)
				}
			}
			res, err := p.Parse(norm.Lines)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			body, st := compress.Body(res)
			st.OriginalLines, st.SGRRemoved = len(norm.Lines), norm.SGRRemoved
			if err := validate.Check(p, res, body); err != nil {
				t.Fatalf("validate: %v", err)
			}
			got := map[string]bool{}
			for _, l := range strings.Split(body, "\n") {
				got[l] = true
			}
			for _, r := range c.required {
				if !got[r] {
					t.Errorf("required line missing: %q", r)
				}
			}
			if c.counts != nil && !parser.EqualCounts(c.counts, res.Counts) {
				t.Errorf("counts = %v, want %v", res.Counts, c.counts)
			}
			// Every non-dropped original line appears, in order.
			assertSubsequence(t, res, body)
			// Determinism.
			res2, _ := p.Parse(norm.Lines)
			if body2, _ := compress.Body(res2); body2 != body {
				t.Error("compression is not deterministic")
			}
			golden := filepath.Join("..", "..", "testdata", "golden", c.log+".txt")
			header := compress.Header(c.parser, "cs_00000000000000000000", "contextslim", st)
			want := header + "\n" + body + "\n"
			if *update {
				os.MkdirAll(filepath.Dir(golden), 0o755)
				if err := os.WriteFile(golden, []byte(want), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			g, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/compress -update)", err)
			}
			if string(g) != want {
				t.Errorf("golden mismatch for %s (run with -update and review the diff)", c.log)
			}
			t.Logf("%s: %d -> %d bytes (%.0f%% saved)", c.log, len(raw), len(want), 100*(1-float64(len(want))/float64(len(raw))))
		})
	}
}

// assertSubsequence checks that the body is exactly the kept lines of the
// original in their original order (apart from merge annotations and blank
// lines collapsed next to removed lines).
func assertSubsequence(t *testing.T, res *parser.Result, body string) {
	t.Helper()
	var kept []string
	for _, b := range res.Blocks {
		if b.Kind != parser.DropPassed && b.Kind != parser.DropProgress {
			kept = append(kept, b.Lines...)
		}
	}
	i := 0
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, parser.MergeMarker) {
			continue
		}
		for i < len(kept) && kept[i] != l {
			if strings.TrimSpace(kept[i]) != "" && !isDuplicateOfEarlier(kept, i) {
				t.Fatalf("kept line missing or reordered: %q", kept[i])
			}
			i++
		}
		if i == len(kept) {
			t.Fatalf("body line not from original: %q", l)
		}
		i++
	}
}

func isDuplicateOfEarlier(lines []string, i int) bool {
	for j := 0; j < i; j++ {
		if lines[j] == lines[i] {
			return true
		}
	}
	return false
}
