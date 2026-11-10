package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yotatameguchi-boop/contextslim/internal/claude"
	"github.com/yotatameguchi-boop/contextslim/internal/store"
)

func posixOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper tests use sh")
	}
}

func setupCompress(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("CONTEXTSLIM_HOME", home)
	if _, _, code := run(t, "", "mode", "compress"); code != 0 {
		t.Fatal("mode")
	}
	return home
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func logPath(t *testing.T, name string) string {
	p, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "logs", name+".txt"))
	return p
}

// The program prints a failing pytest log and exits 1: output is compressed,
// the exit code is kept and the original is retrievable byte for byte.
func TestRunCompressesFailingRunAndKeepsExitCode(t *testing.T) {
	posixOnly(t)
	setupCompress(t)
	lp := logPath(t, "pytest-v-fail")
	orig, _ := os.ReadFile(lp)
	key := store.SessionKey("s")
	out, errOut, code := run(t, "", "run", "--session", key, "--cmd", b64("pytest -v"), "--", "sh", "-c", "cat "+lp+"; exit 1")
	if code != 1 {
		t.Fatalf("exit code %d (%s)", code, errOut)
	}
	if !strings.HasPrefix(out, "[contextslim] pytest output compressed.") || len(out) >= len(orig) {
		t.Fatalf("not compressed:\n%.300s", out)
	}
	for _, must := range []string{"FAILED tests/test_fail.py::test_error - KeyError: 'missing-key'", "3 failed, 151 passed"} {
		if !strings.Contains(out, must) {
			t.Errorf("missing %q", must)
		}
	}
	id := regexp.MustCompile(`cs_[0-9a-f]{20}`).FindString(out)
	got, _, _ := run(t, "", "show", id)
	if got != string(orig) {
		t.Fatal("show differs from the program's output")
	}
	st, _, _ := run(t, "", "stats", "--json")
	if !strings.Contains(st, `"compressed": 1`) || !strings.Contains(st, `"wrapper_runs": 1`) {
		t.Fatalf("stats: %s", st)
	}
}

func TestRunPassesThroughWhenNotCompressible(t *testing.T) {
	posixOnly(t)
	setupCompress(t)
	// Unrecognized output: printed byte for byte, stderr merged, exit code kept.
	out, _, code := run(t, "", "run", "--session", store.SessionKey("s"), "--cmd", b64("pytest"), "--",
		"sh", "-c", "printf 'out1\\n'; printf 'err1\\n' >&2; printf 'tail-no-newline'; exit 7")
	if code != 7 || out != "out1\nerr1\ntail-no-newline" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	// Env assignments before the program still apply.
	out, _, code = run(t, "", "run", "--session", store.SessionKey("s"), "--cmd", b64("FOO=bar pytest"), "--",
		"FOO=bar", "env", "sh", "-c", "echo FOO=$FOO")
	if code != 0 || out != "FOO=bar\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	// Missing program behaves like the shell.
	out, _, code = run(t, "", "run", "--session", store.SessionKey("s"), "--cmd", b64("pytest"), "--", "no-such-program-xyz")
	if code != 127 || !strings.Contains(out, "command not found") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestRunWithBrokenConfigStillRuns(t *testing.T) {
	posixOnly(t)
	home := setupCompress(t)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"mode":"nope"}`), 0o600)
	out, _, code := run(t, "", "run", "--session", store.SessionKey("s"), "--cmd", b64("pytest"), "--", "sh", "-c", "echo hi; exit 3")
	if code != 3 || out != "hi\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestRunStdinAndLargeOutput(t *testing.T) {
	posixOnly(t)
	home := setupCompress(t)
	cfg := map[string]any{"mode": "compress", "max_input_bytes": 1000}
	b, _ := json.Marshal(cfg)
	os.WriteFile(filepath.Join(home, "config.json"), b, 0o600)
	var out, errb bytes.Buffer
	code := Main([]string{"run", "--session", store.SessionKey("s"), "--cmd", b64("pytest"), "--", "sh", "-c", "cat; head -c 5000 /dev/zero | tr '\\0' 'x'"},
		strings.NewReader("from-stdin\n"), &out, &errb)
	if code != 0 || !strings.HasPrefix(out.String(), "from-stdin\nxxx") || out.Len() != len("from-stdin\n")+5000 {
		t.Fatalf("code=%d len=%d", code, out.Len())
	}
}

// Claude Code stops a command on timeout by signalling it: the program gets
// the signal, its output so far is printed unchanged, exit code is 128+N.
func TestRunForwardsSignals(t *testing.T) {
	posixOnly(t)
	setupCompress(t)
	bin := filepath.Join(t.TempDir(), "contextslim")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/contextslim").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	cmd := exec.Command(bin, "run", "--session", store.SessionKey("s"), "--cmd", b64("pytest"), "--",
		"sh", "-c", "echo started; trap 'echo got-term; exit 143' TERM; while :; do sleep 0.05; done")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	if code != 143 || !strings.Contains(out.String(), "started") || !strings.Contains(out.String(), "got-term") {
		t.Fatalf("code=%d err=%v out=%q", code, err, out.String())
	}
}

func TestInstallRegistersPreToolUse(t *testing.T) {
	t.Setenv("CONTEXTSLIM_HOME", t.TempDir())
	proj := t.TempDir()
	run(t, "", "install", "--project", proj)
	refs, err := claude.ScanEvent(filepath.Join(proj, ".claude", "settings.local.json"), "PreToolUse")
	if err != nil || len(refs) != 1 || !refs[0].Owned {
		t.Fatalf("%+v %v", refs, err)
	}
}
