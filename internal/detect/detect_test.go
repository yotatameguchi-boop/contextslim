package detect

import (
	"errors"
	"reflect"
	"testing"
)

func TestAnalyze(t *testing.T) {
	ok := []struct {
		cmd   string
		cands []string
		args  []string
	}{
		{"npx vitest run", []string{"vitest"}, []string{}},
		{"npx vitest run src/a.test.ts --reporter=verbose", []string{"vitest"}, []string{"src/a.test.ts", "--reporter=verbose"}},
		{"vitest", []string{"vitest"}, []string{}},
		{"pnpm vitest run 2>&1", []string{"vitest"}, []string{}},
		{"pnpm exec vitest", []string{"vitest"}, []string{}},
		{"bunx vitest run", []string{"vitest"}, []string{}},
		{"CI=1 FORCE_COLOR=0 npx --yes jest --verbose", []string{"jest"}, []string{"--verbose"}},
		{"./node_modules/.bin/jest", []string{"jest"}, []string{}},
		{`'node_modules\.bin\tsc.cmd' --noEmit`, []string{"tsc"}, []string{"--noEmit"}},
		{"npx tsc -p tsconfig.json", []string{"tsc"}, []string{"-p", "tsconfig.json"}},
		{"npm test", []string{"vitest", "jest"}, []string{}},
		{"npm run test -- --silent", []string{"vitest", "jest"}, []string{"--silent"}},
		{"yarn test", []string{"vitest", "jest"}, []string{}},
		{"pytest", []string{"pytest"}, []string{}},
		{"pytest -q tests/test_a.py::test_x", []string{"pytest"}, []string{"-q", "tests/test_a.py::test_x"}},
		{"python -m pytest -v", []string{"pytest"}, []string{"-v"}},
		{"python3 -B -m pytest", []string{"pytest"}, []string{}},
		{"uv run pytest -x", []string{"pytest"}, []string{"-x"}},
		{"poetry run pytest", []string{"pytest"}, []string{}},
		{".venv/bin/pytest 'tests/test a.py'", []string{"pytest"}, []string{"tests/test a.py"}},
		{"ruff check .", []string{"ruff"}, []string{"."}},
		{"uv run ruff check src --output-format concise", []string{"ruff"}, []string{"src", "--output-format", "concise"}},
		{`pytest -k "not slow"`, []string{"pytest"}, []string{"-k", "not slow"}},
	}
	for _, c := range ok {
		m, err := Analyze(c.cmd)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.cmd, err)
			continue
		}
		if !reflect.DeepEqual(m.Candidates, c.cands) {
			t.Errorf("%q: candidates %v, want %v", c.cmd, m.Candidates, c.cands)
		}
		if len(m.Args) != len(c.args) || (len(c.args) > 0 && !reflect.DeepEqual(m.Args, c.args)) {
			t.Errorf("%q: args %q, want %q", c.cmd, m.Args, c.args)
		}
	}
	bad := []struct {
		cmd string
		err error
	}{
		{"npx vitest run | tail -50", ErrCompound},
		{"cd app && npx vitest run", ErrCompound},
		{"pytest; echo done", ErrCompound},
		{"pytest > out.txt", ErrCompound},
		{"pytest $ARGS", ErrCompound},
		{"pytest $(cat args)", ErrCompound},
		{"pytest `cat args`", ErrCompound},
		{"pytest \"$HOME\"", ErrCompound},
		{"pytest &", ErrCompound},
		{"pytest\nrm -rf x", ErrCompound},
		{"pytest 'unterminated", ErrCompound},
		{"pytest 2>&1 | head", ErrCompound},
		{"pytest 2>&1 2>&1", ErrCompound},
		{"vitest watch", ErrSubcommand},
		{"ruff format .", ErrSubcommand},
		{"ruff .", ErrSubcommand},
		{"npm run build", ErrUnsupported},
		{"go test ./...", ErrUnsupported},
		{"cat vitest.log", ErrUnsupported},
		{"echo pytest", ErrUnsupported},
		{"", ErrEmpty},
		{"FOO=1", ErrEmpty},
	}
	for _, c := range bad {
		if _, err := Analyze(c.cmd); !errors.Is(err, c.err) {
			t.Errorf("%q: err %v, want %v", c.cmd, err, c.err)
		}
	}
}

func FuzzTokenize(f *testing.F) {
	for _, s := range []string{"npx vitest run", `pytest -k "a b" 'c'`, "a\\ b", "x 2>&1", "$(x)"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = Tokenize(s)
		_, _ = Analyze(s)
	})
}
