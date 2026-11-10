package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// HookEvents are the events ContextSlim registers for. PreToolUse wraps
// supported commands with `contextslim run` (method "wrap"); PostToolUse
// replaces output of unwrapped commands that exit 0; PostToolUseFailure
// (non-zero exit, cannot replace output) is used for statistics only.
var HookEvents = []string{"PreToolUse", "PostToolUse", "PostToolUseFailure"}

// HookTimeoutSeconds is the timeout registered for the hook. The processing
// budget (config processing_budget_ms) is kept well below it.
const HookTimeoutSeconds = 10

// OwnerFlag marks hook commands registered by ContextSlim, so ownership does
// not depend on the binary's file name. `contextslim hook` ignores it.
const OwnerFlag = "--owner=contextslim"

// ownedRE also recognizes a bare `<path>/contextslim[.exe] hook` written by hand.
var ownedRE = regexp.MustCompile(`(?i)(^|[/\\"' ])contextslim(\.exe)?["']?\s+hook(\s|$)`)

// IsOwnedCommand reports whether a hook command belongs to ContextSlim.
func IsOwnedCommand(cmd string) bool {
	f := strings.Fields(cmd)
	return (len(f) >= 2 && f[len(f)-1] == OwnerFlag && f[len(f)-2] == "hook") || ownedRE.MatchString(cmd)
}

// HookCommand builds the command string for the executable path.
func HookCommand(exe string) string {
	return QuoteArg(exe) + " hook " + OwnerFlag
}

// QuoteArg quotes a path for the shell Claude Code uses to run hooks when it
// contains characters that need quoting.
func QuoteArg(p string) string {
	if p == "" {
		return `""`
	}
	if strings.ContainsAny(p, " \t'\"$`\\()&;|<>*?[]{}!#~") {
		if !strings.ContainsAny(p, `"$`+"`") {
			return `"` + p + `"`
		}
		return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return p
}

// handler is a hook handler entry inside a matcher group.
type handler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// Change describes what Install/Uninstall did.
type Change struct {
	Path    string
	Backup  string // backup file written before the change ("" if none)
	Changed bool
	Message string
}

// Install registers the hook in a settings file. Existing settings are kept
// verbatim (key order included). Re-running updates the command in place and
// never adds a duplicate. A backup is written before any modification.
func Install(path, exe string, now time.Time) (Change, error) {
	ch := Change{Path: path}
	orig, root, err := loadSettings(path)
	if err != nil {
		return ch, err
	}
	cmd := HookCommand(exe)
	hooks, err := childObject(root, "hooks")
	if err != nil {
		return ch, err
	}
	for _, ev := range HookEvents {
		if err := installEvent(hooks, ev, cmd); err != nil {
			return ch, err
		}
	}
	hb, err := hooks.MarshalJSON()
	if err != nil {
		return ch, err
	}
	root.Set("hooks", hb)
	return writeIfChanged(path, orig, root, now, ch)
}

// installEvent registers (or updates) the handler under one hook event.
func installEvent(hooks *Object, event, cmd string) error {
	groups, err := arrayOf(hooks, event)
	if err != nil {
		return err
	}
	found := false
	for gi, g := range groups {
		gobj, err := ParseObject(g)
		if err != nil {
			continue
		}
		hs, err := arrayOf(gobj, "hooks")
		if err != nil {
			continue
		}
		modified := false
		var kept []json.RawMessage
		for _, h := range hs {
			var hd handler
			if json.Unmarshal(h, &hd) == nil && hd.Type == "command" && IsOwnedCommand(hd.Command) {
				if found {
					modified = true // drop duplicate registrations
					continue
				}
				found = true
				hobj, err := ParseObject(h)
				if err != nil {
					return err
				}
				if hd.Command != cmd {
					v, _ := marshalNoEscape(cmd)
					hobj.Set("command", v)
					modified = true
				}
				if _, ok := hobj.Get("timeout"); !ok {
					hobj.Set("timeout", json.RawMessage(fmt.Sprint(HookTimeoutSeconds)))
					modified = true
				}
				nb, _ := hobj.MarshalJSON()
				h = nb
			}
			kept = append(kept, h)
		}
		if modified {
			setArray(gobj, "hooks", kept)
			nb, err := gobj.MarshalJSON()
			if err != nil {
				return err
			}
			groups[gi] = nb
		}
	}
	if !found {
		h := NewObject()
		h.Set("type", json.RawMessage(`"command"`))
		v, _ := marshalNoEscape(cmd)
		h.Set("command", v)
		h.Set("timeout", json.RawMessage(fmt.Sprint(HookTimeoutSeconds)))
		hb, _ := h.MarshalJSON()
		g := NewObject()
		g.Set("matcher", json.RawMessage(`"Bash"`))
		g.Set("hooks", json.RawMessage("["+string(hb)+"]"))
		gb, _ := g.MarshalJSON()
		groups = append(groups, gb)
	}
	setArray(hooks, event, groups)
	return nil
}

// Uninstall removes only ContextSlim's hook handlers. Groups and keys that
// become empty because of the removal are removed too; anything else is kept.
func Uninstall(path string, now time.Time) (Change, error) {
	ch := Change{Path: path}
	orig, root, err := loadSettings(path)
	if err != nil {
		return ch, err
	}
	if orig == nil {
		ch.Message = "settings file does not exist"
		return ch, nil
	}
	hv, ok := root.Get("hooks")
	if !ok {
		ch.Message = "no hooks configured"
		return ch, nil
	}
	hooks, err := ParseObject(hv)
	if err != nil {
		return ch, fmt.Errorf("%s: hooks: %w", path, err)
	}
	removed := 0
	for _, ev := range HookEvents {
		n, err := uninstallEvent(hooks, ev)
		if err != nil {
			return ch, err
		}
		removed += n
	}
	if removed == 0 {
		ch.Message = "ContextSlim hook not found"
		return ch, nil
	}
	if hooks.Len() == 0 {
		root.Delete("hooks")
	} else {
		hb, _ := hooks.MarshalJSON()
		root.Set("hooks", hb)
	}
	return writeIfChanged(path, orig, root, now, ch)
}

// uninstallEvent removes ContextSlim handlers under one hook event.
func uninstallEvent(hooks *Object, event string) (int, error) {
	removed := 0
	groups, err := arrayOf(hooks, event)
	if err != nil {
		return 0, err
	}
	var keptGroups []json.RawMessage
	for _, g := range groups {
		gobj, err := ParseObject(g)
		if err != nil {
			keptGroups = append(keptGroups, g)
			continue
		}
		hs, err := arrayOf(gobj, "hooks")
		if err != nil {
			keptGroups = append(keptGroups, g)
			continue
		}
		var kept []json.RawMessage
		for _, h := range hs {
			var hd handler
			if json.Unmarshal(h, &hd) == nil && hd.Type == "command" && IsOwnedCommand(hd.Command) {
				removed++
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == len(hs) {
			keptGroups = append(keptGroups, g)
			continue
		}
		if len(kept) == 0 {
			continue // the group only held our handler
		}
		setArray(gobj, "hooks", kept)
		nb, _ := gobj.MarshalJSON()
		keptGroups = append(keptGroups, nb)
	}
	if removed == 0 {
		return 0, nil
	}
	if len(keptGroups) == 0 {
		hooks.Delete(event)
	} else {
		setArray(hooks, event, keptGroups)
	}
	return removed, nil
}

func loadSettings(path string) ([]byte, *Object, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, NewObject(), nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return b, NewObject(), nil
	}
	o, err := ParseObject(b)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: not valid JSON (%v); refusing to modify it", path, err)
	}
	return b, o, nil
}

func childObject(o *Object, k string) (*Object, error) {
	v, ok := o.Get(k)
	if !ok || string(bytes.TrimSpace(v)) == "null" {
		return NewObject(), nil
	}
	c, err := ParseObject(v)
	if err != nil {
		return nil, fmt.Errorf("%q is not an object: refusing to modify it", k)
	}
	return c, nil
}

func arrayOf(o *Object, k string) ([]json.RawMessage, error) {
	v, ok := o.Get(k)
	if !ok || string(bytes.TrimSpace(v)) == "null" {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(v, &arr); err != nil {
		return nil, fmt.Errorf("%q is not an array: refusing to modify it", k)
	}
	return arr, nil
}

func setArray(o *Object, k string, arr []json.RawMessage) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, v := range arr {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(v)
	}
	buf.WriteByte(']')
	o.Set(k, buf.Bytes())
}

func writeIfChanged(path string, orig []byte, root *Object, now time.Time, ch Change) (Change, error) {
	raw, err := root.MarshalJSON()
	if err != nil {
		return ch, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return ch, err
	}
	out.WriteByte('\n')
	if orig != nil && semanticallyEqual(orig, out.Bytes()) {
		ch.Message = "already up to date"
		return ch, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ch, err
	}
	mode := fs.FileMode(0o644)
	if orig != nil {
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
		ch.Backup = fmt.Sprintf("%s.contextslim-backup-%s", path, now.UTC().Format("20060102T150405.000000000Z"))
		if err := os.WriteFile(ch.Backup, orig, mode); err != nil {
			return ch, fmt.Errorf("backup failed, settings not modified: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".contextslim-settings-*")
	if err != nil {
		return ch, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return ch, err
	}
	if err := tmp.Close(); err != nil {
		return ch, err
	}
	_ = os.Chmod(tmp.Name(), mode)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return ch, err
	}
	ch.Changed = true
	return ch, nil
}

func semanticallyEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

// HookRef is a PostToolUse handler found in a settings file.
type HookRef struct {
	File    string
	Matcher string
	Command string
	Owned   bool
}

// ScanPostToolUse lists PostToolUse command handlers whose matcher can match
// the Bash tool.
func ScanPostToolUse(path string) ([]HookRef, error) { return ScanEvent(path, "PostToolUse") }

// ScanEvent lists command handlers of one event whose matcher can match Bash.
func ScanEvent(path, event string) ([]HookRef, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string    `json:"matcher"`
			Hooks   []handler `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out []HookRef
	for _, g := range s.Hooks[event] {
		if !MatcherMatchesBash(g.Matcher) {
			continue
		}
		for _, h := range g.Hooks {
			out = append(out, HookRef{File: path, Matcher: g.Matcher, Command: h.Command, Owned: h.Type == "command" && IsOwnedCommand(h.Command)})
		}
	}
	return out, nil
}

// MatcherMatchesBash approximates Claude Code's matcher semantics: empty or
// "*" match everything; otherwise the matcher is a regex/alternation of tool
// names.
func MatcherMatchesBash(m string) bool {
	if m == "" || m == "*" {
		return true
	}
	re, err := regexp.Compile("^(?:" + m + ")$")
	if err != nil {
		return strings.Contains(m, "Bash")
	}
	return re.MatchString("Bash")
}
