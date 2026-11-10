package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const existing = `{
  "permissions": {"allow": ["Bash(npm test:*)"]},
  "model": "opus",
  "hooks": {
    "PostToolUse": [
      {"matcher": "Edit|Write", "hooks": [{"type": "command", "command": "prettier --write"}]},
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "./audit.sh"}]}
    ],
    "Stop": [{"hooks": [{"type": "command", "command": "notify"}]}]
  },
  "zzz_unknown": {"nested": [1, 2.50, null]}
}
`

func TestInstallUninstallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude", "settings.local.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(existing), 0o644)
	exe := "/opt/Context Slim/contextslim"

	ch, err := Install(p, exe, time.Now())
	if err != nil || !ch.Changed || ch.Backup == "" {
		t.Fatalf("%v %+v", err, ch)
	}
	if b, _ := os.ReadFile(ch.Backup); string(b) != existing {
		t.Fatal("backup differs from original")
	}
	after, _ := os.ReadFile(p)
	if !strings.Contains(string(after), `"command": "\"/opt/Context Slim/contextslim\" hook --owner=contextslim"`) {
		t.Fatalf("hook not added:\n%s", after)
	}
	// Key order of the user's settings is kept.
	if i, j := strings.Index(string(after), `"permissions"`), strings.Index(string(after), `"zzz_unknown"`); i > j {
		t.Error("key order changed")
	}
	if !strings.Contains(string(after), "2.50") {
		t.Error("number formatting changed")
	}
	// Idempotent.
	ch2, err := Install(p, exe, time.Now())
	if err != nil || ch2.Changed {
		t.Fatalf("second install changed the file: %+v %v", ch2, err)
	}
	if n := strings.Count(string(after), "--owner=contextslim"); n != len(HookEvents) {
		t.Fatalf("hook registered %d times", n)
	}
	// New path updates in place.
	if _, err := Install(p, "/usr/local/bin/contextslim", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), "--owner=contextslim") != len(HookEvents) || !strings.Contains(string(b), "/usr/local/bin/contextslim hook --owner=contextslim") {
		t.Fatalf("update in place failed:\n%s", b)
	}
	// Uninstall restores the original content.
	if _, err := Uninstall(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	final, _ := os.ReadFile(p)
	if !semanticallyEqual(final, []byte(existing)) {
		t.Fatalf("uninstall did not restore settings:\n%s", final)
	}
	// Uninstall again: no change.
	if ch, err := Uninstall(p, time.Now()); err != nil || ch.Changed {
		t.Fatalf("%+v %v", ch, err)
	}
}

func TestInstallSharesGroupKeepsOthers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	// Our handler shares a group with a user handler.
	os.WriteFile(p, []byte(`{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"./audit.sh"},{"type":"command","command":"/x/contextslim hook","timeout":3}]}]}}`), 0o644)
	if _, err := Uninstall(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var v map[string]any
	json.Unmarshal(b, &v)
	if !strings.Contains(string(b), "audit.sh") || strings.Contains(string(b), "contextslim") {
		t.Fatalf("%s", b)
	}
}

func TestInstallNewFileAndInvalid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "settings.local.json")
	ch, err := Install(p, "/bin/contextslim", time.Now())
	if err != nil || !ch.Changed || ch.Backup != "" {
		t.Fatalf("%+v %v", ch, err)
	}
	refs, err := ScanPostToolUse(p)
	if err != nil || len(refs) != 1 || !refs[0].Owned {
		t.Fatalf("%+v %v", refs, err)
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"hooks": [`), 0o644)
	if _, err := Install(bad, "/bin/contextslim", time.Now()); err == nil {
		t.Fatal("modified invalid JSON")
	}
	if b, _ := os.ReadFile(bad); string(b) != `{"hooks": [` {
		t.Fatal("invalid file was changed")
	}
}

func TestOwnershipAndMatcher(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/usr/bin/contextslim hook":               true,
		`"C:\Program Files\contextslim.exe" hook`: true,
		"contextslim hook":                        true,
		"contextslim show cs_x":                   false,
		"mycontextslim hook":                      false,
		"./audit.sh":                              false,
	} {
		if IsOwnedCommand(cmd) != want {
			t.Errorf("%q: want %v", cmd, want)
		}
	}
	for m, want := range map[string]bool{"": true, "*": true, "Bash": true, "Edit|Bash": true, "Edit|Write": false, "mcp__.*": false, ".*": true} {
		if MatcherMatchesBash(m) != want {
			t.Errorf("matcher %q: want %v", m, want)
		}
	}
}

func TestReplaceFieldPreservesUnknown(t *testing.T) {
	raw := json.RawMessage(`{"stdout":"long","stderr":"","interrupted":false,"isImage":false,"futureField":{"a":[1,2]},"returnCodeInterpretation":"x"}`)
	out, err := ReplaceField(raw, "stdout", "<short> & \"q\"")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"stdout":"<short> & \"q\"","stderr":"","interrupted":false,"isImage":false,"futureField":{"a":[1,2]},"returnCodeInterpretation":"x"}`
	if string(out) != want {
		t.Fatalf("got %s", out)
	}
	if _, err := ParseBashResponse(json.RawMessage(`{"stdout":1,"stderr":"","interrupted":false}`)); err == nil {
		t.Error("accepted non-string stdout")
	}
	if _, err := ParseBashResponse(json.RawMessage(`{"stdout":"","stderr":""}`)); err == nil {
		t.Error("accepted missing interrupted")
	}
}
