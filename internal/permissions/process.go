package permissions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// riskyFlags add permission rules or settings that ContextSlim cannot see.
// If the launching claude process uses any of them, wrapping is disabled.
var riskyFlags = []string{
	"--disallowedTools", "--disallowed-tools",
	"--settings", "--setting-sources",
	"--permission-prompt-tool",
}

// CLIInfo describes the launching claude process.
type CLIInfo struct {
	PID   int
	Args  []string // exact argv when Exact, else whitespace-split command line
	Exact bool
	// Allow holds --allowedTools rules (only when Exact).
	Allow []string
	// Risky is the first rule-changing flag found, if any.
	Risky string
}

// ErrNoClaude means no claude ancestor process could be inspected.
var ErrNoClaude = errors.New("claude process not found")

// InspectClaude walks up from the current process to the nearest `claude`
// process and reads its command line.
func InspectClaude() (*CLIInfo, error) {
	pid := os.Getppid()
	for depth := 0; depth < 10 && pid > 1; depth++ {
		args, ppid, exact, err := procInfo(pid)
		if err != nil {
			return nil, err
		}
		if isClaude(args) {
			info := &CLIInfo{PID: pid, Args: args, Exact: exact}
			info.Risky, info.Allow = scanFlags(args, exact)
			return info, nil
		}
		pid = ppid
	}
	return nil, ErrNoClaude
}

func isClaude(args []string) bool {
	if len(args) == 0 {
		return false
	}
	b := strings.ToLower(filepath.Base(args[0]))
	if b == "claude" || b == "claude.exe" {
		return true
	}
	for _, a := range args[1:] {
		if strings.Contains(a, "@anthropic-ai/claude-code") || strings.HasSuffix(a, "claude-code/cli.js") {
			return true
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
	}
	return false
}

func scanFlags(args []string, exact bool) (risky string, allow []string) {
	for i := 1; i < len(args); i++ {
		a := args[i]
		for _, f := range riskyFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return a, nil
			}
		}
		if !exact {
			continue
		}
		var vals []string
		switch {
		case a == "--allowedTools" || a == "--allowed-tools":
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				vals = append(vals, args[i])
			}
		case strings.HasPrefix(a, "--allowedTools="), strings.HasPrefix(a, "--allowed-tools="):
			vals = append(vals, a[strings.IndexByte(a, '=')+1:])
		}
		for _, v := range vals {
			allow = append(allow, splitRules(v)...)
		}
	}
	return "", allow
}

// splitRules splits "Bash(a:*),Read Edit" on commas/spaces outside parentheses.
func splitRules(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		case (r == ',' || r == ' ') && depth == 0:
			if t := strings.TrimSpace(s[start:i]); t != "" {
				out = append(out, t)
			}
			start = i + 1
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, t)
	}
	return out
}

// procInfo returns argv and parent pid of pid.
func procInfo(pid int) (args []string, ppid int, exact bool, err error) {
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return nil, 0, false, err
		}
		args = strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return nil, 0, false, err
		}
		// Field 4 after the parenthesised command name.
		s := string(st)
		j := strings.LastIndexByte(s, ')')
		if j < 0 {
			return nil, 0, false, errors.New("bad /proc stat")
		}
		f := strings.Fields(s[j+1:])
		if len(f) < 2 {
			return nil, 0, false, errors.New("bad /proc stat")
		}
		ppid, err = strconv.Atoi(f[1])
		return args, ppid, true, err
	case "darwin", "freebsd", "openbsd", "netbsd":
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return nil, 0, false, err
		}
		line := strings.TrimSpace(string(bytes.TrimSpace(out)))
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, 0, false, errors.New("unexpected ps output")
		}
		ppid, err = strconv.Atoi(f[0])
		return f[1:], ppid, false, err
	default:
		return nil, 0, false, fmt.Errorf("process inspection not supported on %s", runtime.GOOS)
	}
}
