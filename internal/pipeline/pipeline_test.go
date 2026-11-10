package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yotatameguchi-boop/contextslim/internal/claude"
	"github.com/yotatameguchi-boop/contextslim/internal/config"
	"github.com/yotatameguchi-boop/contextslim/internal/metrics"
	"github.com/yotatameguchi-boop/contextslim/internal/parser"
	"github.com/yotatameguchi-boop/contextslim/internal/store"
)

func readLog(t testing.TB, name string) string {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "logs", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hookInput(session, command, stdout string, extra map[string]any) []byte {
	resp := map[string]any{"stdout": stdout, "stderr": "", "interrupted": false, "isImage": false, "noOutputExpected": false}
	for k, v := range extra {
		resp[k] = v
	}
	b, _ := json.Marshal(map[string]any{
		"session_id": session, "hook_event_name": "PostToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": command}, "tool_response": resp, "tool_use_id": "toolu_1",
	})
	return b
}

func testEnv(t testing.TB, mode string) Env {
	cfg := config.Default()
	cfg.Mode = mode
	return Env{Config: cfg, Store: store.New(t.TempDir()), ShowCmd: "contextslim", Now: time.Now}
}

func decodeOutput(t testing.TB, out []byte) map[string]any {
	var v struct {
		H struct {
			Event   string         `json:"hookEventName"`
			Updated map[string]any `json:"updatedToolOutput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("invalid hook output: %v\n%s", err, out)
	}
	if v.H.Event != "PostToolUse" {
		t.Fatalf("hookEventName = %q", v.H.Event)
	}
	return v.H.Updated
}

func TestCompressAndShowRoundTrip(t *testing.T) {
	env := testEnv(t, config.ModeCompress)
	orig := readLog(t, "pytest-v-fail")
	o := Process(env, hookInput("sess-1", "pytest -v", orig, map[string]any{"returnCodeInterpretation": "keep-me"}))
	if o.Event.Action != metrics.ActionCompressed {
		t.Fatalf("not compressed: %+v", o.Event)
	}
	upd := decodeOutput(t, o.Output)
	stdout := upd["stdout"].(string)
	if upd["returnCodeInterpretation"] != "keep-me" || upd["interrupted"] != false || upd["noOutputExpected"] != false {
		t.Errorf("other fields not preserved: %v", upd)
	}
	if !strings.Contains(stdout, o.Event.RawID) || !strings.Contains(stdout, "contextslim show") {
		t.Error("raw reference missing from compressed output")
	}
	if len(stdout) >= len(orig) {
		t.Error("not smaller")
	}
	rec, err := env.Store.Load(o.Event.RawID)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := claude.ParseBashResponse(rec.ToolResponse)
	if resp.Stdout != orig {
		t.Fatal("stored original differs")
	}
	if rec.SessionKey != store.SessionKey("sess-1") || rec.Command != "pytest -v" {
		t.Errorf("%+v", rec)
	}
}

func TestObserveModeDoesNotChangeOutput(t *testing.T) {
	env := testEnv(t, config.ModeObserve)
	o := Process(env, hookInput("s", "pytest -v", readLog(t, "pytest-v-fail"), nil))
	if o.Output != nil || o.Event.Action != metrics.ActionObserved {
		t.Fatalf("%+v", o.Event)
	}
	if o.Event.BytesOut == 0 || o.Event.BytesOut >= o.Event.BytesIn {
		t.Errorf("candidate sizes not recorded: %+v", o.Event)
	}
	if entries, _ := os.ReadDir(filepath.Join(env.Store.Dir, "raw")); len(entries) != 0 {
		t.Error("observe mode stored raw output")
	}
}

func TestFallbacks(t *testing.T) {
	big := readLog(t, "pytest-v-fail")
	vitest := readLog(t, "vitest-verbose-fail")
	cases := []struct {
		name   string
		input  []byte
		reason string
		mutate func(*Env)
	}{
		{"invalid json", []byte("{not json"), ReasonBadJSON, nil},
		{"other event", []byte(`{"hook_event_name":"Notification","tool_name":"Bash"}`), ReasonNotPostToolUse, nil},
		{"other tool", []byte(`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_response":{}}`), ReasonNotBash, nil},
		{"schema", []byte(`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"pytest"},"tool_response":"text"}`), ReasonSchema, nil},
		{"self", hookInput("s", "contextslim show cs_0123456789abcdef0123", big, nil), ReasonSelf, nil},
		{"pipe", hookInput("s", "pytest -v | tail -n 100", big, nil), ReasonCommand, nil},
		{"chain", hookInput("s", "cd x && pytest -v", big, nil), ReasonCommand, nil},
		{"unsupported tool", hookInput("s", "go test ./...", big, nil), ReasonCommand, nil},
		{"interrupted", hookInput("s", "pytest -v", big, map[string]any{"interrupted": true}), ReasonInterrupted, nil},
		{"background", hookInput("s", "pytest -v", big, map[string]any{"backgroundTaskId": "b1"}), ReasonBackground, nil},
		{"persisted", hookInput("s", "pytest -v", big, map[string]any{"persistedOutputPath": "/tmp/x"}), ReasonBackground, nil},
		{"image", hookInput("s", "pytest -v", big, map[string]any{"isImage": true}), ReasonImage, nil},
		{"small", hookInput("s", "pytest", readLog(t, "pytest-pass"), nil), ReasonTooSmall, nil},
		{"too large", hookInput("s", "pytest -v", big, nil), ReasonTooLarge, func(e *Env) { e.Config.MaxInputBytes = 5000 }},
		{"wrong tool output", hookInput("s", "pytest -v", vitest, nil), ReasonFormat, nil},
		{"option", hookInput("s", "npx vitest run --reporter=json", vitest, nil), ReasonOption, nil},
		{"disabled", hookInput("s", "pytest -v", big, nil), ReasonDisabled, func(e *Env) { e.Config.EnabledParsers = []string{"jest"} }},
		{"savings", hookInput("s", "pytest -v", big, nil), ReasonNotSmaller, func(e *Env) { e.Config.MinSavingsRatio = 0.95 }},
		{"progress CR", hookInput("s", "pytest -v", strings.Replace(big, "\n", "\r", 3), nil), ReasonText, nil},
		{"cursor escape", hookInput("s", "pytest -v", "\x1b[2K"+big, nil), ReasonText, nil},
		{"binary", hookInput("s", "pytest -v", big+"\x00", nil), ReasonText, nil},
		{"incomplete", hookInput("s", "pytest -v", big[:len(big)*2/3], nil), ReasonFormat, nil},
		{"incomplete vitest", hookInput("s", "npx vitest run --reporter=verbose", strings.Split(vitest, " Test Files")[0], nil), ReasonFormat, nil},
		{"unknown important block", hookInput("s", "npx vitest run --reporter=verbose", strings.Replace(vitest, " ✓ vt/mod1.test.ts > module 1 > case 1 works 1ms", "Error: worker crashed unexpectedly", 1), nil), ReasonUnknownBlock, nil},
		{"unverified version", hookInput("s", "pytest -v", strings.Replace(big, "pytest-9.2.0", "pytest-6.2.5", 1), nil), ReasonUnverified, nil},
		{"vitest 4", hookInput("s", "npx vitest run --reporter=verbose", strings.Replace(vitest, "RUN  v3.2.7", "RUN  v4.0.0", 1), nil), ReasonUnverified, nil},
		{"count mismatch", hookInput("s", "pytest -v", strings.Replace(big, "151 passed", "15 passed", 1), nil), ReasonParse, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := testEnv(t, config.ModeCompress)
			if c.mutate != nil {
				c.mutate(&env)
			}
			o := Process(env, c.input)
			if o.Output != nil {
				t.Fatalf("output replaced; want pass-through")
			}
			if o.Event.Reason != c.reason {
				t.Fatalf("reason = %q, want %q", o.Event.Reason, c.reason)
			}
		})
	}
}

func TestStorageFailureDoesNotCompress(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permission test needs a non-root POSIX user")
	}
	env := testEnv(t, config.ModeCompress)
	os.MkdirAll(env.Store.Dir, 0o500)
	os.Chmod(env.Store.Dir, 0o500)
	defer os.Chmod(env.Store.Dir, 0o700)
	o := Process(env, hookInput("s", "pytest -v", readLog(t, "pytest-v-fail"), nil))
	if o.Output != nil || o.Event.Reason != ReasonStorage {
		t.Fatalf("%+v", o.Event)
	}
}

func TestStorageLimitDoesNotCompress(t *testing.T) {
	env := testEnv(t, config.ModeCompress)
	env.Config.MaxStorageBytes = 1000
	o := Process(env, hookInput("s", "pytest -v", readLog(t, "pytest-v-fail"), nil))
	if o.Output != nil || o.Event.Reason != ReasonStorageFull {
		t.Fatalf("%+v", o.Event)
	}
}

func TestNpmTestPicksParserFromOutput(t *testing.T) {
	env := testEnv(t, config.ModeCompress)
	for log, want := range map[string]string{"jest-big-verbose-fail": "jest", "vitest-verbose-fail": "vitest"} {
		o := Process(env, hookInput("s", "npm test", readLog(t, log), nil))
		if o.Event.Parser != want || o.Event.Action != metrics.ActionCompressed {
			t.Errorf("%s: %+v", log, o.Event)
		}
	}
}

func TestDuplicateDiagnosticsMerged(t *testing.T) {
	block := "F401 [*] `os` imported but unused\n --> pkg/a.py:1:8\n  |\n1 | import os\n  |        ^^\n  |\nhelp: Remove unused import: `os`\n\n"
	other := "F401 [*] `os` imported but unused\n --> pkg/b.py:1:8\n  |\n1 | import os\n  |        ^^\n  |\nhelp: Remove unused import: `os`\n\n"
	out := strings.Repeat(block, 40) + other + "Found 41 errors.\n[*] 41 fixable with the `--fix` option.\n"
	env := testEnv(t, config.ModeCompress)
	o := Process(env, hookInput("s", "ruff check .", out, nil))
	if o.Event.Action != metrics.ActionCompressed {
		t.Fatalf("%+v", o.Event)
	}
	s := decodeOutput(t, o.Output)["stdout"].(string)
	if strings.Count(s, "pkg/a.py:1:8") != 1 || !strings.Contains(s, "repeated 40 times") {
		t.Fatalf("not merged:\n%s", s)
	}
	if !strings.Contains(s, "pkg/b.py:1:8") {
		t.Fatal("distinct location was merged away")
	}
	if !strings.Contains(s, "Found 41 errors.") {
		t.Fatal("summary lost")
	}
}

func TestStderrFieldAndCRLF(t *testing.T) {
	env := testEnv(t, config.ModeCompress)
	jest := strings.ReplaceAll(readLog(t, "jest-big-verbose-fail"), "\n", "\r\n")
	b, _ := json.Marshal(map[string]any{
		"session_id": "s", "hook_event_name": "PostToolUse", "tool_name": "Bash",
		"tool_input":    map[string]any{"command": "npx jest --verbose"},
		"tool_response": map[string]any{"stdout": "", "stderr": jest, "interrupted": false},
	})
	o := Process(env, b)
	if o.Event.Action != metrics.ActionCompressed {
		t.Fatalf("%+v", o.Event)
	}
	upd := decodeOutput(t, o.Output)
	if upd["stdout"] != "" || !strings.Contains(upd["stderr"].(string), "✕ rounds") {
		t.Fatal("wrong field compressed")
	}
}

func TestParallelSessionsDoNotMix(t *testing.T) {
	env := testEnv(t, config.ModeCompress)
	logs := []string{"pytest-v-fail", "jest-big-verbose-fail", "vitest-verbose-fail", "pytest8-v-fail"}
	cmds := []string{"pytest -v", "npx jest --verbose", "npx vitest run --reporter=verbose", "pytest -v"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]string{} // raw id -> expected original
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := i % len(logs)
			orig := readLog(t, logs[k])
			o := Process(env, hookInput("session-"+string(rune('a'+i%3)), cmds[k], orig, nil))
			if o.Event.Action != metrics.ActionCompressed {
				t.Errorf("%d: %+v", i, o.Event)
				return
			}
			metrics.Append(env.Store.MetricsDir(), o.SessionKey, o.Event)
			mu.Lock()
			got[o.Event.RawID] = orig
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	for id, orig := range got {
		rec, err := env.Store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		resp, _ := claude.ParseBashResponse(rec.ToolResponse)
		if resp.Stdout != orig {
			t.Fatalf("raw %s holds another output", id)
		}
	}
	events, sessions, _ := metrics.ReadAll(env.Store.MetricsDir(), "")
	if len(events) != 24 || sessions != 3 {
		t.Fatalf("events=%d sessions=%d", len(events), sessions)
	}
}

// FuzzProcess mutates real logs and checks the safety properties: no panic,
// and whenever output is replaced, every line the tool marked as a failure or
// summary is still present and nothing that mentions a failure was removed.
func FuzzProcess(f *testing.F) {
	type seed struct{ cmd, log string }
	seeds := []seed{{"pytest -v", "pytest-v-fail"}, {"pytest", "pytest-big-fail"}, {"npx jest --verbose", "jest-big-verbose-fail"},
		{"npx vitest run --reporter=verbose", "vitest-verbose-fail"}, {"npx vitest run", "vitest-big-fail"},
		{"npx tsc --pretty", "tsc-pretty"}, {"ruff check .", "ruff-full"}}
	for i, s := range seeds {
		f.Add(i, s.cmd, readLog(f, s.log), 0, "")
	}
	f.Fuzz(func(t *testing.T, which int, cmd, log string, pos int, insert string) {
		if pos < 0 {
			pos = -pos
		}
		if len(log) > 0 {
			pos %= len(log) + 1
			log = log[:pos] + insert + log[pos:]
		}
		env := testEnv(t, config.ModeCompress)
		env.Config.MinInputBytes, env.Config.MinSavingsBytes, env.Config.MinSavingsRatio = 0, 0, 0
		env.Config.ProcessingBudgetMS = 5000
		o := Process(env, hookInput("s", cmd, log, nil))
		if o.Output == nil {
			return
		}
		out := decodeOutput(t, o.Output)["stdout"].(string)
		kept := map[string]bool{}
		for _, l := range strings.Split(out, "\n") {
			kept[l] = true
		}
		norm := strings.ReplaceAll(log, "\r\n", "\n")
		for _, l := range strings.Split(norm, "\n") {
			l = stripSGR(l)
			if parser.IsImportant(l) && !kept[l] {
				t.Fatalf("line mentioning a failure was removed: %q", l)
			}
		}
	})
}

func stripSGR(s string) string {
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			return s
		}
		j := i + 2
		for j < len(s) && (s[j] == ';' || s[j] >= '0' && s[j] <= '9') {
			j++
		}
		if j < len(s) && s[j] == 'm' {
			s = s[:i] + s[j+1:]
		} else {
			return s
		}
	}
}

func TestFailureEventIsObservedOnly(t *testing.T) {
	for _, mode := range []string{config.ModeCompress, config.ModeObserve} {
		env := testEnv(t, mode)
		b, _ := json.Marshal(map[string]any{
			"session_id": "s", "hook_event_name": "PostToolUseFailure", "tool_name": "Bash",
			"tool_input": map[string]any{"command": "pytest -v"}, "tool_use_id": "t",
			"error": "Exit code 1\n" + strings.TrimSuffix(readLog(t, "pytest-v-fail"), "\n"), "is_interrupt": false,
		})
		o := Process(env, b)
		if o.Output != nil {
			t.Fatal("failure event must never produce output")
		}
		if o.Event.Action != metrics.ActionObserved || o.Event.Reason != ReasonFailureEvent || o.Event.BytesOut >= o.Event.BytesIn {
			t.Fatalf("%s: %+v", mode, o.Event)
		}
		if entries, _ := os.ReadDir(filepath.Join(env.Store.Dir, "raw")); len(entries) != 0 {
			t.Error("failure event stored raw output")
		}
	}
}

// Hook input exactly as Claude Code sends it: literal <, >, & (not \u003e).
func TestRealisticJSONEncodingRoundTrip(t *testing.T) {
	env := testEnv(t, config.ModeCompress)
	orig := readLog(t, "jest-big-verbose-fail") + "<tag> & more\n"
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]any{"session_id": "s", "hook_event_name": "PostToolUse", "tool_name": "Bash",
		"tool_input":    map[string]any{"command": "npx jest --verbose"},
		"tool_response": map[string]any{"stdout": orig, "stderr": "", "interrupted": false}})
	if !strings.Contains(sb.String(), "<tag> & more") {
		t.Fatal("test input is escaped")
	}
	o := Process(env, []byte(sb.String()))
	if o.Event.Action != metrics.ActionCompressed {
		t.Skipf("not compressed: %+v", o.Event)
	}
	rec, err := env.Store.Load(o.Event.RawID)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := claude.ParseBashResponse(rec.ToolResponse)
	if resp.Stdout != orig {
		t.Fatal("original changed")
	}
}
