package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func run(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(args, strings.NewReader(stdin), &out, &errb)
	return out.String(), errb.String(), code
}

func hookJSON(t *testing.T, session, cmd, stdout string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": session, "hook_event_name": "PostToolUse", "tool_name": "Bash",
		"tool_input":    map[string]any{"command": cmd},
		"tool_response": map[string]any{"stdout": stdout, "stderr": "", "interrupted": false, "isImage": false},
	})
	return string(b)
}

func TestEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CONTEXTSLIM_HOME", home)
	logB, err := os.ReadFile(filepath.Join("..", "..", "testdata", "logs", "jest-big-verbose-fail.txt"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(logB)
	in := hookJSON(t, "e2e", "npx jest --verbose", log)

	// Default is observe: no output change.
	if out, _, code := run(t, in, "hook"); out != "" || code != 0 {
		t.Fatalf("observe mode replaced output: %q", out)
	}
	if out, _, _ := run(t, "", "mode"); strings.TrimSpace(out) != "observe" {
		t.Fatalf("mode = %q", out)
	}
	run(t, "", "mode", "compress")
	out, _, code := run(t, in, "hook")
	if code != 0 || out == "" {
		t.Fatalf("no replacement (code %d)", code)
	}
	var v struct {
		H struct {
			U struct{ Stdout string } `json:"updatedToolOutput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	id := regexp.MustCompile(`cs_[0-9a-f]{20}`).FindString(v.H.U.Stdout)
	if id == "" {
		t.Fatalf("no raw id in:\n%s", v.H.U.Stdout)
	}
	// show returns the exact original.
	if got, _, code := run(t, "", "show", id); code != 0 || got != log {
		t.Fatalf("show differs (code %d)", code)
	}
	got, _, _ := run(t, "", "show", id, "--from", "2", "--to", "3")
	lines := strings.Split(log, "\n")
	if got != lines[1]+"\n"+lines[2]+"\n" {
		t.Fatalf("range = %q", got)
	}
	// show's own output is never recompressed.
	if out, _, _ := run(t, hookJSON(t, "e2e", "contextslim show "+id, log), "hook"); out != "" {
		t.Fatal("show output was recompressed")
	}
	if _, errOut, code := run(t, "", "show", "cs_0123456789abcdef0123"); code == 0 || !strings.Contains(errOut, "見つかりません") {
		t.Fatalf("missing id: %d %s", code, errOut)
	}
	if _, _, code := run(t, "", "show", "../../etc/passwd"); code == 0 {
		t.Fatal("accepted path as id")
	}
	// Broken input never fails the hook.
	if out, _, code := run(t, "garbage", "hook"); out != "" || code != 0 {
		t.Fatal("garbage input not passed through")
	}
	// stats
	st, _, _ := run(t, "", "stats", "--json")
	var s struct {
		HookRuns   int `json:"hook_runs"`
		Compressed int `json:"compressed"`
		Observed   int `json:"observed"`
		Shows      int `json:"raw_refetches"`
	}
	json.Unmarshal([]byte(st), &s)
	if s.Compressed != 1 || s.Observed != 1 || s.Shows != 2 || s.HookRuns != 4 {
		t.Fatalf("stats: %s", st)
	}
	text, _, _ := run(t, "", "stats")
	if !strings.Contains(text, "実測") || !strings.Contains(text, "token_estimator=none") {
		t.Fatalf("stats text:\n%s", text)
	}
	if out, _, code := run(t, "", "prune", "--dry-run"); code != 0 || !strings.Contains(out, "削除予定") {
		t.Fatalf("prune: %s", out)
	}
	// Broken config: hook passes through, doctor-relevant commands report it.
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"mode":"turbo"}`), 0o600)
	if out, _, code := run(t, in, "hook"); out != "" || code != 0 {
		t.Fatal("invalid config did not pass through")
	}
	if _, _, code := run(t, "", "mode"); code == 0 {
		t.Fatal("invalid config accepted")
	}
}

func TestInstallCommand(t *testing.T) {
	t.Setenv("CONTEXTSLIM_HOME", t.TempDir())
	proj := t.TempDir()
	if out, errOut, code := run(t, "", "install", "--project", proj); code != 0 {
		t.Fatalf("%s %s", out, errOut)
	}
	b, _ := os.ReadFile(filepath.Join(proj, ".claude", "settings.local.json"))
	if !strings.Contains(string(b), " hook") || !strings.Contains(string(b), `"matcher": "Bash"`) {
		t.Fatalf("%s", b)
	}
	if out, _, _ := run(t, "", "install", "--project", proj); !strings.Contains(out, "変更なし") {
		t.Fatalf("not idempotent: %s", out)
	}
	if _, _, code := run(t, "", "uninstall", "--project", proj); code != 0 {
		t.Fatal("uninstall failed")
	}
	b, _ = os.ReadFile(filepath.Join(proj, ".claude", "settings.local.json"))
	if strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("uninstall left: %s", b)
	}
	if _, _, code := run(t, "", "install", "--shared", "--user"); code == 0 {
		t.Fatal("conflicting flags accepted")
	}
}
