package pipeline

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/yotatameguchi-boop/contextslim/internal/claude"
	"github.com/yotatameguchi-boop/contextslim/internal/config"
	"github.com/yotatameguchi-boop/contextslim/internal/detect"
	"github.com/yotatameguchi-boop/contextslim/internal/metrics"
	"github.com/yotatameguchi-boop/contextslim/internal/parser"
	"github.com/yotatameguchi-boop/contextslim/internal/permissions"
)

// PermProvider supplies the permission information the wrap decision needs.
// Tests replace it; the default reads settings files and inspects the
// launching claude process.
type PermProvider interface {
	Rules(projectDir string) (permissions.Rules, error)
	CLI() (*permissions.CLIInfo, error)
}

type realPerm struct{}

func (realPerm) Rules(projectDir string) (permissions.Rules, error) {
	return permissions.Load(permissions.SettingsFiles(projectDir))
}
func (realPerm) CLI() (*permissions.CLIInfo, error) { return permissions.InspectClaude() }

// wrappedRE recognizes commands produced by Wrap.
var wrappedRE = regexp.MustCompile(` run --session [0-9a-f]{16} --cmd [A-Za-z0-9_-]+ -- `)

// IsWrapped reports whether a Bash command was rewritten by ContextSlim.
func IsWrapped(cmd string) bool { return wrappedRE.MatchString(cmd) }

// WrapCommand builds the rewritten command. The original command is kept
// verbatim after "--" so the shell still performs its quoting and globbing;
// the base64 copy lets the wrapper identify the tool exactly as the hook did.
func WrapCommand(exe, sessionKey, original string) string {
	return fmt.Sprintf("%s run --session %s --cmd %s -- %s",
		exe, sessionKey, base64.RawURLEncoding.EncodeToString([]byte(original)), original)
}

// Not-wrapped reasons in addition to permissions.No*.
const (
	NoWrapBackground = "run_in_background"
	NoWrapOption     = "unsupported_option"
	NoWrapDisabled   = "parser_disabled"
	NoWrapSchema     = "schema_mismatch"
)

// preToolUse decides whether to rewrite a supported Bash command so that it
// runs through `contextslim run`. It returns no output (the command runs
// unchanged) unless every safety condition holds.
func preToolUse(env Env, o *Outcome, in *claude.HookInput) *Outcome {
	o.Event.Hook = "PreToolUse"
	cfg := env.Config
	if cfg.Mode != config.ModeCompress || cfg.Method != config.MethodWrap || in.ToolName != "Bash" {
		o.NoRecord = true
		return skip(o, ReasonNotBash)
	}
	notWrapped := func(reason string) *Outcome {
		o.Event.Action = metrics.ActionNotWrapped
		o.Event.Reason = reason
		o.Output = nil
		return o
	}
	var ti map[string]json.RawMessage
	if err := json.Unmarshal(in.ToolInput, &ti); err != nil || ti == nil {
		o.NoRecord = true
		return skip(o, ReasonSchema)
	}
	command := in.Command()
	if command == "" || IsWrapped(command) || isSelfCommand(command) {
		o.NoRecord = true
		return skip(o, ReasonSelf)
	}
	match, err := detect.Analyze(command)
	if err != nil {
		// Not a supported command: nothing to record, nothing to do.
		o.NoRecord = true
		return skip(o, ReasonCommand)
	}
	if bg, ok := ti["run_in_background"]; ok && string(bg) == "true" {
		return notWrapped(NoWrapBackground)
	}
	reg := parser.Registry()
	enabled := 0
	for _, name := range match.Candidates {
		if !cfg.ParserEnabled(name) {
			continue
		}
		enabled++
		if err := reg[name].CheckArgs(match.Args); err != nil {
			return notWrapped(NoWrapOption)
		}
	}
	if enabled == 0 {
		return notWrapped(NoWrapDisabled)
	}
	perm := env.Perm
	if perm == nil {
		perm = realPerm{}
	}
	cli, err := perm.CLI()
	if err != nil {
		return notWrapped(permissions.NoClaudeProcess)
	}
	if cli.Risky != "" {
		return notWrapped(permissions.NoCLIRules)
	}
	projectDir := os.Getenv("CLAUDE_PROJECT_DIR")
	if projectDir == "" {
		projectDir = in.CWD
	}
	rules, err := perm.Rules(projectDir)
	if err != nil {
		return notWrapped(permissions.NoSettingsInvalid)
	}
	d := permissions.Check(command, in.PermissionMode, rules, cli.Allow)
	if !d.Wrap {
		return notWrapped(d.Reason)
	}
	newCmd, _ := marshalNoEscape(WrapCommand(env.ShowCmd, o.SessionKey, command))
	ti["command"] = newCmd
	updated, err := marshalNoEscape(ti)
	if err != nil {
		return notWrapped(NoWrapSchema)
	}
	reason, _ := marshalNoEscape("ContextSlim: the original command is " + d.Reason + "; it runs unchanged through `contextslim run`, which compresses its output and keeps its exit code.")
	out := []byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":` +
		string(reason) + `,"updatedInput":` + string(updated) + "}}\n")
	if !json.Valid(out) {
		return notWrapped(NoWrapSchema)
	}
	o.Output = out
	o.Event.Action = metrics.ActionWrapped
	o.Event.Parser = match.Candidates[0]
	if len(match.Candidates) > 1 {
		o.Event.Parser = ""
	}
	return o
}
