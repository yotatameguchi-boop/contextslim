package permissions

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCheck(t *testing.T) {
	cases := []struct {
		name  string
		cmd   string
		mode  string
		rules Rules
		cli   []string
		wrap  bool
		why   string
	}{
		{"allowed by prefix", "npx vitest run", "default", Rules{Allow: []string{"Bash(npx vitest:*)"}}, nil, true, ""},
		{"allowed exact", "pytest -q", "acceptEdits", Rules{Allow: []string{"Bash(pytest -q)"}}, nil, true, ""},
		{"allowed glob", "pytest -q tests/a.py", "default", Rules{Allow: []string{"Bash(pytest *)"}}, nil, true, ""},
		{"allowed all bash", "pytest", "default", Rules{Allow: []string{"Bash"}}, nil, true, ""},
		{"allowed via cli", "pytest", "default", Rules{}, []string{"Bash(pytest:*)"}, true, ""},
		{"no allow", "pytest", "default", Rules{Allow: []string{"Bash(npm test:*)", "Read"}}, nil, false, NoAllowRule},
		{"prefix must end at word", "pytestx", "default", Rules{Allow: []string{"Bash(pytest:*)"}}, nil, false, NoAllowRule},
		{"bypass mode", "pytest", "bypassPermissions", Rules{}, nil, true, ""},
		{"plan mode", "pytest", "plan", Rules{Allow: []string{"Bash"}}, nil, false, NoPlanMode},
		{"unknown mode", "pytest", "weird", Rules{Allow: []string{"Bash"}}, nil, false, NoUnknownMode},
		{"deny beats allow", "npx vitest run", "default", Rules{Allow: []string{"Bash"}, Deny: []string{"Bash(npx vitest:*)"}}, nil, false, NoDenyMatch},
		{"deny in bypass mode", "pytest", "bypassPermissions", Rules{Deny: []string{"Bash(pytest:*)"}}, nil, false, NoDenyMatch},
		{"deny all bash", "pytest", "bypassPermissions", Rules{Deny: []string{"Bash"}}, nil, false, NoDenyMatch},
		{"deny wildcard tool", "pytest", "bypassPermissions", Rules{Deny: []string{"*"}}, nil, false, NoDenyMatch},
		{"deny ignores env prefix", "CI=1 pytest -x", "bypassPermissions", Rules{Deny: []string{"Bash(pytest:*)"}}, nil, false, NoDenyMatch},
		{"deny ignores path", "./node_modules/.bin/jest", "bypassPermissions", Rules{Deny: []string{"Bash(jest:*)"}}, nil, false, NoDenyMatch},
		{"deny ignores 2>&1", "pytest 2>&1", "bypassPermissions", Rules{Deny: []string{"Bash(pytest)"}}, nil, false, NoDenyMatch},
		{"deny glob", "npx tsc --noEmit", "bypassPermissions", Rules{Deny: []string{"Bash(npx *)"}}, nil, false, NoDenyMatch},
		{"deny generous prefix", "pytest", "bypassPermissions", Rules{Deny: []string{"Bash(pytes:*)"}}, nil, false, NoDenyMatch},
		{"deny unparsable", "pytest", "bypassPermissions", Rules{Deny: []string{"Bash(pytest"}}, nil, false, NoDenyMatch},
		{"deny other tool", "pytest", "bypassPermissions", Rules{Deny: []string{"Read(./secret)", "WebFetch"}}, nil, true, ""},
		{"deny unrelated", "pytest", "bypassPermissions", Rules{Deny: []string{"Bash(rm:*)"}}, nil, true, ""},
		{"ask matches", "pytest", "default", Rules{Allow: []string{"Bash"}, Ask: []string{"Bash(pytest:*)"}}, nil, false, NoAskMatch},
	}
	for _, c := range cases {
		d := Check(c.cmd, c.mode, c.rules, c.cli)
		if d.Wrap != c.wrap || (!c.wrap && d.Reason != c.why) {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
}

func TestScanFlags(t *testing.T) {
	risky, allow := scanFlags([]string{"claude", "-p", "x", "--allowedTools", "Bash(npm test:*),Read", "Bash(pytest:*)", "--verbose"}, true)
	if risky != "" || !reflect.DeepEqual(allow, []string{"Bash(npm test:*)", "Read", "Bash(pytest:*)"}) {
		t.Fatalf("%q %q", risky, allow)
	}
	for _, args := range [][]string{
		{"claude", "--disallowedTools", "Bash(rm:*)"},
		{"claude", "--disallowed-tools=Bash"},
		{"claude", "--settings", "/x.json"},
		{"claude", "--setting-sources", "user"},
	} {
		if r, _ := scanFlags(args, true); r == "" {
			t.Errorf("%v not flagged", args)
		}
	}
	// Inexact (ps) command lines never yield allow rules.
	if _, allow := scanFlags([]string{"claude", "--allowedTools", "Bash(pytest:*)"}, false); allow != nil {
		t.Error("allow rules from inexact argv")
	}
	if !isClaude([]string{"/usr/local/bin/claude", "-p"}) || !isClaude([]string{"node", "/x/node_modules/@anthropic-ai/claude-code/cli.js"}) || isClaude([]string{"/bin/sh", "-c", "claude"}) {
		t.Error("isClaude")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	os.WriteFile(a, []byte(`{"permissions":{"allow":["Bash(pytest:*)"],"deny":["Bash(rm:*)"]},"model":"x"}`), 0o600)
	os.WriteFile(b, []byte(`{"permissions":{"ask":["Bash(git push:*)"]}}`), 0o600)
	r, err := Load([]string{a, filepath.Join(dir, "missing.json"), b})
	if err != nil || len(r.Allow) != 1 || len(r.Deny) != 1 || len(r.Ask) != 1 || len(r.Sources) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	os.WriteFile(b, []byte(`{not json`), 0o600)
	if _, err := Load([]string{a, b}); err == nil {
		t.Fatal("broken settings accepted")
	}
}

func TestInspectClaudeRuns(t *testing.T) {
	// The test binary is not started by claude; inspection must fail safely
	// (or find a real claude ancestor when tests run inside Claude Code).
	if info, err := InspectClaude(); err == nil && info.PID <= 1 {
		t.Fatalf("bad info %+v", info)
	}
}
