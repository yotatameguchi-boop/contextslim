// Package permissions decides whether wrapping a Bash command keeps Claude
// Code's permission behaviour unchanged.
//
// Verified on Claude Code 2.1.322 (docs/phase0.md): when a PreToolUse hook
// rewrites a command with updatedInput, permission rules are evaluated only
// against the rewritten command. Allow rules for the original no longer apply
// and, more importantly, deny/ask rules for the original are bypassed.
//
// ContextSlim therefore wraps a command only when it can show that Claude
// Code would have run the original without asking:
//   - no deny or ask rule in any settings file could match it (matching is
//     deliberately generous: an unparsable rule counts as a match),
//   - an allow rule matches it strictly, or the session runs in
//     bypassPermissions mode,
//   - the launching `claude` process carries no command-line option that adds
//     rules or settings ContextSlim cannot read (e.g. --disallowedTools).
//
// Any doubt means "do not wrap"; the command then runs exactly as Claude Code
// would run it without ContextSlim.
package permissions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Rules collected from settings.
type Rules struct {
	Allow, Deny, Ask []string
	// Sources lists the files that were read (for diagnostics).
	Sources []string
}

// Decision is the result of Check.
type Decision struct {
	Wrap   bool
	Reason string // why not, or which rule allowed it
}

// Reasons for not wrapping.
const (
	NoDenyMatch       = "denied_by_rule"
	NoAskMatch        = "ask_rule_matches"
	NoAllowRule       = "not_explicitly_allowed"
	NoPlanMode        = "plan_mode"
	NoUnknownMode     = "unknown_permission_mode"
	NoCLIRules        = "cli_permission_flags"
	NoClaudeProcess   = "claude_process_not_inspectable"
	NoSettingsInvalid = "settings_unreadable"
)

// Check decides whether command may be wrapped.
func Check(command, permissionMode string, rules Rules, cliAllow []string) Decision {
	for _, r := range rules.Deny {
		if bashRuleMayMatch(r, command) {
			return Decision{Reason: NoDenyMatch}
		}
	}
	for _, r := range rules.Ask {
		if bashRuleMayMatch(r, command) {
			return Decision{Reason: NoAskMatch}
		}
	}
	switch permissionMode {
	case "plan":
		return Decision{Reason: NoPlanMode}
	case "bypassPermissions":
		return Decision{Wrap: true, Reason: "bypassPermissions mode"}
	case "default", "acceptEdits", "auto", "dontAsk", "":
	default:
		return Decision{Reason: NoUnknownMode}
	}
	for _, r := range append(append([]string(nil), rules.Allow...), cliAllow...) {
		if bashRuleAllows(r, command) {
			return Decision{Wrap: true, Reason: "allowed by " + r}
		}
	}
	return Decision{Reason: NoAllowRule}
}

// parseRule splits "Tool(content)" into tool and content. ok=false for
// malformed rules.
func parseRule(r string) (tool, content string, hasContent, ok bool) {
	r = strings.TrimSpace(r)
	i := strings.IndexByte(r, '(')
	if i < 0 {
		return r, "", false, r != ""
	}
	if !strings.HasSuffix(r, ")") {
		return "", "", false, false
	}
	return strings.TrimSpace(r[:i]), r[i+1 : len(r)-1], true, true
}

// bashRuleMayMatch reports whether a deny/ask rule could match the command.
// It errs towards true.
func bashRuleMayMatch(rule, command string) bool {
	tool, content, has, ok := parseRule(rule)
	if !ok {
		return true // unparsable: assume it matters
	}
	if !strings.EqualFold(tool, "Bash") && tool != "*" {
		return false
	}
	if !has || content == "" || content == "*" {
		return true
	}
	forms := commandForms(command)
	for _, f := range forms {
		if matchContent(content, f, true) {
			return true
		}
	}
	return false
}

// bashRuleAllows reports whether an allow rule certainly matches the command.
func bashRuleAllows(rule, command string) bool {
	tool, content, has, ok := parseRule(rule)
	if !ok || tool != "Bash" {
		return false
	}
	if !has || content == "*" {
		return true
	}
	return matchContent(content, strings.TrimSpace(command), false)
}

// matchContent implements the Bash rule forms: "prefix:*", globs with "*",
// and exact strings. generous widens matching for deny/ask checks.
func matchContent(content, cmd string, generous bool) bool {
	if strings.HasSuffix(content, ":*") {
		p := strings.TrimSuffix(content, ":*")
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return true
		}
		return generous && strings.HasPrefix(cmd, p)
	}
	if strings.Contains(content, "*") {
		if globMatch(content, cmd) {
			return true
		}
		// Generous: a glob that matches any token prefix of the command.
		return generous && globMatch(strings.TrimRight(content, "* "), firstWords(cmd, strings.Count(strings.TrimRight(content, "* "), " ")+1))
	}
	if cmd == content {
		return true
	}
	return generous && (strings.HasPrefix(cmd, content+" ") || strings.HasPrefix(content, cmd))
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

// globMatch matches * as any sequence (including spaces); other characters
// are literal.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i, p := range parts[1:] {
		last := i == len(parts)-2
		if last {
			return strings.HasSuffix(s, p)
		}
		j := strings.Index(s, p)
		if j < 0 {
			return false
		}
		s = s[j+len(p):]
	}
	return true
}

// commandForms returns the command and variants without leading environment
// assignments, `env`, or wrappers such as `timeout`, so deny rules written for
// the bare program still count.
func commandForms(cmd string) []string {
	cmd = strings.TrimSpace(cmd)
	forms := []string{cmd}
	f := strings.Fields(cmd)
	i := 0
	if i < len(f) && f[i] == "env" {
		i++
	}
	for i < len(f) && strings.Contains(f[i], "=") && !strings.HasPrefix(f[i], "-") {
		i++
	}
	if i > 0 && i < len(f) {
		forms = append(forms, strings.Join(f[i:], " "))
	}
	if strings.HasSuffix(cmd, " 2>&1") {
		for _, x := range append([]string(nil), forms...) {
			forms = append(forms, strings.TrimSuffix(x, " 2>&1"))
		}
	}
	// Also the program name without its directory: ./node_modules/.bin/jest
	if i < len(f) {
		base := path.Base(strings.ReplaceAll(f[i], `\`, "/"))
		if base != f[i] {
			forms = append(forms, strings.Join(append([]string{base}, f[i+1:]...), " "))
		}
	}
	return forms
}

// SettingsFiles returns the settings files Claude Code reads, most global
// first. projectDir is CLAUDE_PROJECT_DIR (or the hook's cwd).
func SettingsFiles(projectDir string) []string {
	var files []string
	files = append(files, managedSettings()...)
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			configDir = filepath.Join(home, ".claude")
		}
	}
	if configDir != "" {
		files = append(files, filepath.Join(configDir, "settings.json"), filepath.Join(configDir, "settings.local.json"))
	}
	if projectDir != "" {
		files = append(files, filepath.Join(projectDir, ".claude", "settings.json"), filepath.Join(projectDir, ".claude", "settings.local.json"))
	}
	return files
}

func managedSettings() []string {
	var dir string
	switch runtime.GOOS {
	case "darwin":
		dir = "/Library/Application Support/ClaudeCode"
	case "windows":
		dir = `C:\Program Files\ClaudeCode`
		if pd := os.Getenv("ProgramData"); pd != "" {
			return append(managedIn(filepath.Join(pd, "ClaudeCode")), managedIn(dir)...)
		}
	default:
		dir = "/etc/claude-code"
	}
	return managedIn(dir)
}

func managedIn(dir string) []string {
	out := []string{filepath.Join(dir, "managed-settings.json")}
	if m, _ := filepath.Glob(filepath.Join(dir, "managed-settings.d", "*.json")); len(m) > 0 {
		sort.Strings(m)
		out = append(out, m...)
	}
	return out
}

// ErrUnreadable means a settings file exists but cannot be parsed; wrapping
// must then be disabled because its rules are unknown.
var ErrUnreadable = errors.New("settings file unreadable")

// Load reads permission rules from files. Missing files are skipped.
func Load(files []string) (Rules, error) {
	var r Rules
	for _, f := range files {
		b, err := os.ReadFile(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return r, fmt.Errorf("%w: %s: %v", ErrUnreadable, f, err)
		}
		var s struct {
			Permissions struct {
				Allow []string `json:"allow"`
				Deny  []string `json:"deny"`
				Ask   []string `json:"ask"`
			} `json:"permissions"`
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return r, fmt.Errorf("%w: %s: %v", ErrUnreadable, f, err)
		}
		r.Allow = append(r.Allow, s.Permissions.Allow...)
		r.Deny = append(r.Deny, s.Permissions.Deny...)
		r.Ask = append(r.Ask, s.Permissions.Ask...)
		r.Sources = append(r.Sources, f)
	}
	return r, nil
}
