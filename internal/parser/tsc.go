package parser

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// TSC handles TypeScript compiler diagnostics in plain (non-TTY default) and
// --pretty formats. Compression is limited to colour-code removal and merging
// of byte-identical diagnostic blocks (same file, position, code, message and
// context). Distinct diagnostics are never merged.
//
// Verified: TypeScript 5.x (5.9.3).
type TSC struct{}

var (
	tscPlainRE  = regexp.MustCompile(`^(.+)\((\d+),(\d+)\): (error|warning|message) TS(\d+): .*$`)
	tscPrettyRE = regexp.MustCompile(`^(.+):(\d+):(\d+) - (error|warning|message) TS(\d+): .*$`)
	tscGlobalRE = regexp.MustCompile(`^(error|warning|message) TS(\d+): .*$`)
	tscFoundRE  = regexp.MustCompile(`^Found (\d+) errors?(?:\.| in .*)$`)
	tscTableRE  = regexp.MustCompile(`^(Errors\s+Files|\s+\d+\s+\S.*)$`)
	tscWatchRE  = regexp.MustCompile(`Starting compilation in watch mode|File change detected`)
)

func (TSC) Name() string    { return "tsc" }
func (TSC) Version() string { return "tsc/1" }

func tscHeader(l string) (sev string, ok bool) {
	if m := tscPlainRE.FindStringSubmatch(l); m != nil {
		return m[4], true
	}
	if m := tscPrettyRE.FindStringSubmatch(l); m != nil {
		return m[4], true
	}
	if m := tscGlobalRE.FindStringSubmatch(l); m != nil {
		return m[1], true
	}
	return "", false
}

func (TSC) Sniff(lines []string) bool {
	for _, l := range lines {
		if _, ok := tscHeader(l); ok {
			return true
		}
	}
	return false
}

func (TSC) CheckArgs(args []string) error {
	if a, ok := hasFlag(args, "--watch", "-w", "--listFiles", "--listFilesOnly", "--showConfig", "--version", "-v",
		"--help", "-h", "--all", "--init", "--traceResolution", "--extendedDiagnostics", "--diagnostics",
		"--explainFiles", "--listEmittedFiles", "--verbose", "--generateTrace"); ok {
		return fmt.Errorf("%w: %s", ErrOption, a)
	}
	return nil
}

func (t TSC) Parse(lines []string) (*Result, error) {
	res := &Result{Parser: t.Name(), Version: t.Version()}
	var b builder
	inDiag := false
	errorsSeen, diags := 0, 0
	found := -1
	for _, l := range lines {
		if tscWatchRE.MatchString(l) {
			return nil, fmt.Errorf("%w: watch mode output", ErrOption)
		}
		if sev, ok := tscHeader(l); ok {
			b.startDiag(l)
			inDiag = true
			diags++
			if sev == "error" {
				errorsSeen++
			}
			res.MustKeep = append(res.MustKeep, l)
			continue
		}
		if m := tscFoundRE.FindStringSubmatch(l); m != nil {
			found, _ = strconv.Atoi(m[1])
			inDiag = false
			res.MustKeep = append(res.MustKeep, l)
			b.add(Keep, l, 0)
			continue
		}
		switch {
		case inDiag:
			// Continuation: message chain, code frame, related information.
			b.appendDiag(l)
		case tscTableRE.MatchString(l), strings.TrimSpace(l) == "":
			b.add(Keep, l, 0)
		default:
			b.add(Unknown, l, 0)
		}
	}
	if diags == 0 {
		return nil, ErrNotRecognized
	}
	if found >= 0 && found != errorsSeen {
		return nil, fmt.Errorf("%w: summary says %d errors, parsed %d", ErrInconsistent, found, errorsSeen)
	}
	res.Blocks = trimDiagTails(b.blocks)
	if err := checkUnknown(res.Blocks); err != nil {
		return nil, err
	}
	res.Counts = map[string]int{"diagnostics": diags, "errors": errorsSeen}
	if found >= 0 {
		res.Counts["found"] = found
	}
	res.Status = StatusPassed
	if errorsSeen > 0 {
		res.Status = StatusFailed
	}
	return res, nil
}

// Counts recounts diagnostic headers, honouring "×N" merge annotations.
func (TSC) Counts(lines []string) (map[string]int, error) {
	diags, errs, found := 0, 0, -1
	for i, l := range lines {
		sev, ok := tscHeader(l)
		if m := tscFoundRE.FindStringSubmatch(l); m != nil {
			found, _ = strconv.Atoi(m[1])
		}
		if !ok {
			continue
		}
		n := mergedCount(lines, i)
		diags += n
		if sev == "error" {
			errs += n
		}
	}
	out := map[string]int{"diagnostics": diags, "errors": errs}
	if found >= 0 {
		out["found"] = found
	}
	return out, nil
}

// MergeMarker prefixes the annotation the compressor adds after a merged block.
const MergeMarker = "[contextslim] ↑ identical diagnostic repeated "

var mergeRE = regexp.MustCompile(`^\[contextslim\] ↑ identical diagnostic repeated (\d+) times`)

// mergedCount returns how many occurrences the diagnostic starting at line i
// stands for: 1, or N when the compressor annotated it as merged.
func mergedCount(lines []string, i int) int {
	for j := i + 1; j < len(lines); j++ {
		if m := mergeRE.FindStringSubmatch(lines[j]); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		if strings.HasPrefix(lines[j], "[contextslim]") {
			break
		}
		// Stop at the next diagnostic header of any supported format.
		if _, ok := tscHeader(lines[j]); ok {
			break
		}
		if ruffHeaderAt(lines, j) {
			break
		}
	}
	return 1
}

// trimDiagTails moves trailing blank lines of diagnostic blocks out into Keep
// blocks so that identical diagnostics compare equal regardless of spacing.
func trimDiagTails(blocks []Block) []Block {
	var out []Block
	for _, bl := range blocks {
		if bl.Kind != Diagnostic {
			out = append(out, bl)
			continue
		}
		end := len(bl.Lines)
		for end > 1 && strings.TrimSpace(bl.Lines[end-1]) == "" {
			end--
		}
		out = append(out, Block{Kind: Diagnostic, Lines: bl.Lines[:end]})
		if end < len(bl.Lines) {
			out = append(out, Block{Kind: Keep, Lines: bl.Lines[end:]})
		}
	}
	return out
}
