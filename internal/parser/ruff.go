package parser

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Ruff handles `ruff check` output in the concise format and in both "full"
// layouts (the older "path:line:col: CODE msg" + frame, and the newer
// "CODE msg" / "--> path:line:col" layout). Like tsc, only byte-identical
// diagnostic blocks are merged.
//
// Verified: ruff 0.17 (full, concise).
type Ruff struct{}

var (
	ruffLocHdrRE = regexp.MustCompile(`^(.+?):(\d+):(\d+): ([A-Z]+[0-9]+|SyntaxError:|invalid-syntax:) .*$`)
	ruffCodeRE   = regexp.MustCompile(`^([A-Z]+[0-9]+|invalid-syntax:|SyntaxError:) \S.*$`)
	ruffArrowRE  = regexp.MustCompile(`^\s*--> (.+):(\d+):(\d+)$`)
	ruffFoundRE  = regexp.MustCompile(`^Found (\d+) errors?(?: \((\d+) fixed, (\d+) remaining\))?\.$`)
	ruffTailRE   = regexp.MustCompile(`^(\[\*\] \d+ fixable|No fixes available|All checks passed!|\d+ hidden fixes? can be enabled|warning: )`)
)

func (Ruff) Name() string    { return "ruff" }
func (Ruff) Version() string { return "ruff/1" }

// ruffHeaderAt reports whether lines[i] starts a ruff diagnostic.
func ruffHeaderAt(lines []string, i int) bool {
	if ruffLocHdrRE.MatchString(lines[i]) {
		return true
	}
	return ruffCodeRE.MatchString(lines[i]) && i+1 < len(lines) && ruffArrowRE.MatchString(lines[i+1])
}

func (Ruff) Sniff(lines []string) bool {
	for _, l := range lines {
		if ruffFoundRE.MatchString(l) || l == "All checks passed!" {
			return true
		}
	}
	return false
}

func (Ruff) CheckArgs(args []string) error {
	if a, ok := hasFlag(args, "--statistics", "--show-files", "--show-settings", "--watch", "-w", "--show-fixes",
		"--diff", "--add-noqa", "--version", "-V", "--help", "-h", "--quiet", "-q", "--silent", "--exit-zero"); ok {
		if a != "--exit-zero" {
			return fmt.Errorf("%w: %s", ErrOption, a)
		}
	}
	if v, ok := flagValue(args, "--output-format"); ok && v != "full" && v != "concise" {
		return fmt.Errorf("%w: --output-format %s", ErrOption, v)
	}
	if v, ok := flagValue(args, "--format"); ok && v != "full" && v != "concise" && v != "text" {
		return fmt.Errorf("%w: --format %s", ErrOption, v)
	}
	return nil
}

func (r Ruff) Parse(lines []string) (*Result, error) {
	res := &Result{Parser: r.Name(), Version: r.Version()}
	var b builder
	inDiag := false
	diags := 0
	found := -1
	sawTail := false
	for i, l := range lines {
		if ruffHeaderAt(lines, i) {
			b.startDiag(l)
			inDiag = true
			diags++
			res.MustKeep = append(res.MustKeep, l)
			continue
		}
		if m := ruffFoundRE.FindStringSubmatch(l); m != nil {
			found, _ = strconv.Atoi(m[1])
			if m[3] != "" {
				found, _ = strconv.Atoi(m[3]) // remaining after --fix
			}
			inDiag = false
			sawTail = true
			res.MustKeep = append(res.MustKeep, l)
			b.add(Keep, l, 0)
			continue
		}
		switch {
		case ruffTailRE.MatchString(l):
			inDiag = false
			sawTail = true
			b.add(Keep, l, 0)
		case inDiag:
			b.appendDiag(l)
		case strings.TrimSpace(l) == "":
			b.add(Keep, l, 0)
		default:
			b.add(Unknown, l, 0)
		}
	}
	if !sawTail {
		return nil, ErrIncomplete
	}
	if diags == 0 && found != 0 && found != -1 {
		return nil, ErrNotRecognized
	}
	if found >= 0 && found != diags {
		return nil, fmt.Errorf("%w: summary says %d errors, parsed %d", ErrInconsistent, found, diags)
	}
	res.Blocks = trimDiagTails(b.blocks)
	if err := checkUnknown(res.Blocks); err != nil {
		return nil, err
	}
	res.Counts = map[string]int{"diagnostics": diags}
	if found >= 0 {
		res.Counts["found"] = found
	}
	res.Status = StatusPassed
	if diags > 0 {
		res.Status = StatusFailed
	}
	return res, nil
}

// Counts recounts diagnostic headers, honouring merge annotations.
func (Ruff) Counts(lines []string) (map[string]int, error) {
	diags, found := 0, -1
	for i, l := range lines {
		if m := ruffFoundRE.FindStringSubmatch(l); m != nil {
			found, _ = strconv.Atoi(m[1])
			if m[3] != "" {
				found, _ = strconv.Atoi(m[3])
			}
		}
		if ruffHeaderAt(lines, i) {
			diags += mergedCount(lines, i)
		}
	}
	out := map[string]int{"diagnostics": diags}
	if found >= 0 {
		out["found"] = found
	}
	return out, nil
}
