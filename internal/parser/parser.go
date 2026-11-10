// Package parser turns known tool outputs into classified blocks.
//
// Parsers never summarize text. They only classify each line as
//   - kept verbatim (recognized or not),
//   - a passing-test line / all-pass progress line that may be replaced by a
//     count, or
//   - part of a diagnostic block that may be merged with an identical block.
//
// Each parser also reports the facts that must survive compression (failure
// identifiers, diagnostic headers, summary lines) and the counts from the
// tool's own summary, which the validator re-checks on the compressed text.
package parser

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Kind classifies a block.
type Kind uint8

const (
	// Keep is a recognized block kept verbatim.
	Keep Kind = iota
	// Unknown is an unrecognized block. It is kept verbatim, but if it looks
	// important (mentions errors/failures) the whole output is passed through.
	Unknown
	// DropPassed is a passing-test detail line replaced by a count.
	DropPassed
	// DropProgress is a progress line made only of passing marks.
	DropProgress
	// Diagnostic is a self-contained diagnostic block; identical blocks
	// (same text, hence same location, code, message and context) may be merged.
	Diagnostic
)

// Status of the tool run as reported by the tool itself.
type Status string

const (
	StatusPassed      Status = "passed"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
)

// Block is a run of lines with one classification.
type Block struct {
	Kind  Kind
	Lines []string
	// Passed is the number of passing tests a dropped block stands for.
	Passed int
}

// Result is a parsed output.
type Result struct {
	Parser      string
	Version     string // parser rule version, e.g. "vitest/1"
	ToolVersion string // tool version printed in the output, if any
	Status      Status
	Blocks      []Block
	// Counts from the tool's summary line(s), e.g. {"failed":3,"passed":97}.
	Counts map[string]int
	// MustKeep lists lines that must appear verbatim in the compressed output.
	MustKeep []string
}

// Parser is implemented by every supported tool format.
type Parser interface {
	Name() string
	Version() string
	// Sniff cheaply reports whether lines look like this tool's output.
	Sniff(lines []string) bool
	// CheckArgs rejects command-line options that change the output format.
	CheckArgs(args []string) error
	// Parse classifies lines. Any error means "pass the original through".
	Parse(lines []string) (*Result, error)
	// Counts re-reads the summary counts from (compressed) text lines.
	Counts(lines []string) (map[string]int, error)
}

// Errors.
var (
	ErrNotRecognized    = errors.New("output not recognized")
	ErrIncomplete       = errors.New("summary missing (incomplete or interrupted output)")
	ErrInconsistent     = errors.New("parsed facts inconsistent with tool summary")
	ErrUnknownImportant = errors.New("unrecognized block mentions an error or failure")
	ErrUnverified       = errors.New("tool version not verified")
	ErrOption           = errors.New("option changes the output format")
)

// Registry returns all parsers by name.
func Registry() map[string]Parser {
	return map[string]Parser{
		"vitest": Vitest{},
		"jest":   Jest{},
		"tsc":    TSC{},
		"pytest": Pytest{},
		"ruff":   Ruff{},
	}
}

// importantRE flags unrecognized lines that must not be passed silently.
var importantRE = regexp.MustCompile(`(?i)\b(error|errors|fail|failed|failure|failures|failing|exception|panic|fatal|traceback|abort(ed)?|interrupted|timed? ?out)\b|✕|×|✗`)

// importantHints are lower-case substrings at least one of which must occur
// for importantRE to match; checking them first avoids running the regex on
// the vast majority of lines.
var importantHints = []string{"error", "fail", "exception", "panic", "fatal", "traceback", "abort", "interrupt", "time", "✕", "×", "✗"}

// IsImportant reports whether a line mentions an error, failure or similar.
func IsImportant(line string) bool {
	lower := strings.ToLower(line)
	for _, h := range importantHints {
		if strings.Contains(lower, h) {
			return importantRE.MatchString(line)
		}
	}
	return false
}

// builder accumulates blocks, merging adjacent blocks of the same mergeable kind.
type builder struct {
	blocks []Block
}

func (b *builder) add(k Kind, line string, passed int) {
	if n := len(b.blocks); n > 0 && k != Diagnostic && b.blocks[n-1].Kind == k {
		b.blocks[n-1].Lines = append(b.blocks[n-1].Lines, line)
		b.blocks[n-1].Passed += passed
		return
	}
	b.blocks = append(b.blocks, Block{Kind: k, Lines: []string{line}, Passed: passed})
}

// startDiag begins a new diagnostic block.
func (b *builder) startDiag(line string) {
	b.blocks = append(b.blocks, Block{Kind: Diagnostic, Lines: []string{line}})
}

// appendDiag appends to the current diagnostic block.
func (b *builder) appendDiag(line string) {
	n := len(b.blocks)
	b.blocks[n-1].Lines = append(b.blocks[n-1].Lines, line)
}

// checkUnknown fails if an unrecognized line looks important.
func checkUnknown(blocks []Block) error {
	for _, bl := range blocks {
		if bl.Kind != Unknown {
			continue
		}
		for _, l := range bl.Lines {
			if IsImportant(l) {
				return fmt.Errorf("%w: %q", ErrUnknownImportant, truncate(l, 80))
			}
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func droppedPassed(blocks []Block) int {
	n := 0
	for _, b := range blocks {
		if b.Kind == DropPassed || b.Kind == DropProgress {
			n += b.Passed
		}
	}
	return n
}

var countRE = regexp.MustCompile(`(\d+) ([a-z][a-z ]*?[a-z])(?:,|\s*\||\s*\(|\s*$| in )`)

// parseCounts extracts "N word" pairs such as "3 failed | 97 passed".
func parseCounts(s string, allowed map[string]string) (map[string]int, error) {
	out := map[string]int{}
	for _, m := range countRE.FindAllStringSubmatch(s+" ", -1) {
		key, ok := allowed[m[2]]
		if !ok {
			return nil, fmt.Errorf("unknown summary word %q", m[2])
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, err
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate summary word %q", m[2])
		}
		out[key] = n
	}
	if len(out) == 0 {
		return nil, errors.New("no counts")
	}
	return out, nil
}

// hasFlag reports whether args contain any of the flags (exact or --flag=value).
func hasFlag(args []string, flags ...string) (string, bool) {
	for _, a := range args {
		if a == "--" {
			continue
		}
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return a, true
			}
		}
	}
	return "", false
}

// flagValue returns the value of --flag=v or --flag v.
func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"="), true
		}
	}
	return "", false
}

func indent(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// EqualCounts compares two count maps.
func EqualCounts(a, b map[string]int) bool { return equalCounts(a, b) }
