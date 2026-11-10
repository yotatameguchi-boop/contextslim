package pipeline

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yotatameguchi-boop/contextslim/internal/config"
	"github.com/yotatameguchi-boop/contextslim/internal/metrics"
	"github.com/yotatameguchi-boop/contextslim/internal/permissions"
	"github.com/yotatameguchi-boop/contextslim/internal/store"
)

type fakePerm struct {
	rules  permissions.Rules
	cli    *permissions.CLIInfo
	cliErr error
	err    error
}

func (f fakePerm) Rules(string) (permissions.Rules, error) { return f.rules, f.err }
func (f fakePerm) CLI() (*permissions.CLIInfo, error) {
	if f.cliErr != nil {
		return nil, f.cliErr
	}
	if f.cli == nil {
		return &permissions.CLIInfo{PID: 2}, nil
	}
	return f.cli, nil
}

func preInput(cmd, mode string, extra map[string]any) []byte {
	ti := map[string]any{"command": cmd, "description": "Run tests", "timeout": 120000}
	for k, v := range extra {
		ti[k] = v
	}
	b, _ := json.Marshal(map[string]any{"session_id": "sess", "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"permission_mode": mode, "cwd": "/proj", "tool_input": ti})
	return b
}

func TestPreToolUseWraps(t *testing.T) {
	env := testEnv(t, config.ModeCompress)
	env.ShowCmd = `"/opt/Context Slim/contextslim"`
	env.Perm = fakePerm{rules: permissions.Rules{Allow: []string{"Bash(npx vitest:*)"}}}
	o := Process(env, preInput("npx vitest run --reporter=verbose", "default", nil))
	if o.Event.Action != metrics.ActionWrapped || o.Output == nil {
		t.Fatalf("%+v", o.Event)
	}
	var v struct {
		H struct {
			Event    string         `json:"hookEventName"`
			Decision string         `json:"permissionDecision"`
			Reason   string         `json:"permissionDecisionReason"`
			Input    map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(o.Output, &v); err != nil {
		t.Fatal(err)
	}
	if v.H.Event != "PreToolUse" || v.H.Decision != "allow" || !strings.Contains(v.H.Reason, "Bash(npx vitest:*)") {
		t.Fatalf("%+v", v.H)
	}
	cmd := v.H.Input["command"].(string)
	want := `"/opt/Context Slim/contextslim" run --session ` + store.SessionKey("sess") + " --cmd " +
		base64.RawURLEncoding.EncodeToString([]byte("npx vitest run --reporter=verbose")) + " -- npx vitest run --reporter=verbose"
	if cmd != want {
		t.Fatalf("command\n got %s\nwant %s", cmd, want)
	}
	if v.H.Input["description"] != "Run tests" || v.H.Input["timeout"].(float64) != 120000 {
		t.Error("other tool_input fields not preserved")
	}
	if !IsWrapped(cmd) {
		t.Error("IsWrapped does not recognize the rewrite")
	}
	// The rewritten command is never wrapped again.
	if o2 := Process(env, preInput(cmd, "default", nil)); o2.Output != nil {
		t.Fatal("wrapped twice")
	}
}

func TestPreToolUseDoesNotWrap(t *testing.T) {
	allowAll := permissions.Rules{Allow: []string{"Bash"}}
	cases := []struct {
		name   string
		cmd    string
		mode   string
		extra  map[string]any
		perm   fakePerm
		mutate func(*Env)
		reason string // "" = nothing recorded
	}{
		{"observe mode", "pytest", "default", nil, fakePerm{rules: allowAll}, func(e *Env) { e.Config.Mode = config.ModeObserve }, ""},
		{"post_tool_use method", "pytest", "default", nil, fakePerm{rules: allowAll}, func(e *Env) { e.Config.Method = config.MethodPostToolUse }, ""},
		{"unsupported command", "ls -la", "default", nil, fakePerm{rules: allowAll}, nil, ""},
		{"compound command", "cd x && pytest", "default", nil, fakePerm{rules: allowAll}, nil, ""},
		{"show command", "contextslim show cs_0123456789abcdef0123", "default", nil, fakePerm{rules: allowAll}, nil, ""},
		{"background", "pytest", "default", map[string]any{"run_in_background": true}, fakePerm{rules: allowAll}, nil, NoWrapBackground},
		{"format option", "npx vitest run --reporter=json", "default", nil, fakePerm{rules: allowAll}, nil, NoWrapOption},
		{"parser disabled", "pytest", "default", nil, fakePerm{rules: allowAll}, func(e *Env) { e.Config.EnabledParsers = []string{"jest"} }, NoWrapDisabled},
		{"not allowed", "pytest", "default", nil, fakePerm{}, nil, permissions.NoAllowRule},
		{"denied", "pytest -x", "bypassPermissions", nil, fakePerm{rules: permissions.Rules{Deny: []string{"Bash(pytest:*)"}}}, nil, permissions.NoDenyMatch},
		{"ask", "pytest", "default", nil, fakePerm{rules: permissions.Rules{Allow: []string{"Bash"}, Ask: []string{"Bash(pytest)"}}}, nil, permissions.NoAskMatch},
		{"plan", "pytest", "plan", nil, fakePerm{rules: allowAll}, nil, permissions.NoPlanMode},
		{"cli disallow", "pytest", "default", nil, fakePerm{rules: allowAll, cli: &permissions.CLIInfo{PID: 2, Risky: "--disallowedTools"}}, nil, permissions.NoCLIRules},
		{"no claude process", "pytest", "default", nil, fakePerm{rules: allowAll, cliErr: errors.New("x")}, nil, permissions.NoClaudeProcess},
		{"settings broken", "pytest", "default", nil, fakePerm{err: errors.New("bad")}, nil, permissions.NoSettingsInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := testEnv(t, config.ModeCompress)
			env.Perm = c.perm
			if c.mutate != nil {
				c.mutate(&env)
			}
			o := Process(env, preInput(c.cmd, c.mode, c.extra))
			if o.Output != nil {
				t.Fatalf("wrapped: %s", o.Output)
			}
			if c.reason == "" {
				if !o.NoRecord {
					t.Errorf("recorded %+v", o.Event)
				}
				return
			}
			if o.NoRecord || o.Event.Action != metrics.ActionNotWrapped || o.Event.Reason != c.reason {
				t.Errorf("event %+v", o.Event)
			}
		})
	}
}

func TestPostToolUseOfWrappedCommandIsNotRecorded(t *testing.T) {
	env := testEnv(t, config.ModeCompress)
	cmd := WrapCommand("contextslim", store.SessionKey("s"), "pytest -v")
	o := Process(env, hookInput("s", cmd, readLog(t, "pytest-v-fail"), nil))
	if o.Output != nil || !o.NoRecord || o.Event.Reason != ReasonWrapped {
		t.Fatalf("%+v", o)
	}
}

func TestCompressOutput(t *testing.T) {
	env := testEnv(t, config.ModeCompress)
	orig := readLog(t, "pytest-v-fail") + "<tag> & \"q\"\n"
	o := CompressOutput(env, store.SessionKey("s"), "pytest -v", orig)
	if o.Event.Action != metrics.ActionCompressed || o.Text == "" || o.Event.Via != "wrapper" {
		t.Fatalf("%+v", o.Event)
	}
	rec, err := env.Store.Load(o.Event.RawID)
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Stdout string }
	json.Unmarshal(rec.ToolResponse, &v)
	if v.Stdout != orig || rec.Command != "pytest -v" {
		t.Fatal("raw not preserved")
	}
	if o := CompressOutput(env, "0000000000000000", "ls", orig); o.Event.Action == metrics.ActionCompressed {
		t.Fatal("compressed unsupported command")
	}
}
