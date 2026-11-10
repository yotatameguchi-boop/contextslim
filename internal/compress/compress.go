// Package compress turns a parsed Result into the text Claude will read.
//
// Rules (design section 8):
//   - passing-test lines and all-pass progress lines are removed and counted;
//   - identical diagnostic blocks are emitted once with an occurrence count;
//   - everything else, including every failure block, is kept verbatim;
//   - nothing is ever truncated by length.
package compress

import (
	"fmt"
	"strings"

	"github.com/yotatameguchi-boop/contextslim/internal/parser"
)

// Stats describes what was removed.
type Stats struct {
	PassedLinesRemoved   int // passing-test detail lines
	ProgressLinesRemoved int // all-pass progress lines
	PassingTestsInLines  int // passing tests those lines represented
	DiagnosticsMerged    int // duplicate diagnostic blocks folded into a count
	SGRRemoved           int // colour sequences removed during normalization
	OriginalLines        int
}

// Body builds the compressed body (without the ContextSlim header).
func Body(res *parser.Result) (string, Stats) {
	var st Stats
	counts := map[string]int{}
	for _, b := range res.Blocks {
		if b.Kind == parser.Diagnostic {
			counts[key(b)]++
		}
	}
	emitted := map[string]bool{}
	var out []string
	afterDrop := false
	for _, b := range res.Blocks {
		if b.Kind == parser.DropPassed || b.Kind == parser.DropProgress {
			afterDrop = true
		} else if afterDrop {
			// Blank lines that surrounded removed lines would otherwise pile
			// up; drop a blank line that directly follows another one.
			// Blank runs inside kept blocks are never touched.
			lines := b.Lines
			for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" && len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				lines = lines[1:]
			}
			b.Lines = lines
			afterDrop = len(lines) == 0
		}
		switch b.Kind {
		case parser.DropPassed:
			st.PassedLinesRemoved += len(b.Lines)
			st.PassingTestsInLines += b.Passed
		case parser.DropProgress:
			st.ProgressLinesRemoved += len(b.Lines)
			st.PassingTestsInLines += b.Passed
		case parser.Diagnostic:
			k := key(b)
			if emitted[k] {
				st.DiagnosticsMerged++
				continue
			}
			emitted[k] = true
			out = append(out, b.Lines...)
			if n := counts[k]; n > 1 {
				out = append(out, fmt.Sprintf("%s%d times in total (identical file, position, code, message and context)", parser.MergeMarker, n))
			}
		default:
			out = append(out, b.Lines...)
		}
	}
	return strings.Join(out, "\n"), st
}

func key(b parser.Block) string { return strings.Join(b.Lines, "\n") }

// Header is the ContextSlim notice placed above the compressed body. It
// states what was removed and how to read the original; it never restates
// the tool's result itself (the tool's own summary lines are kept verbatim).
func Header(tool, rawID, showCmd string, st Stats) string {
	var removed []string
	if st.PassedLinesRemoved > 0 {
		removed = append(removed, fmt.Sprintf("%d passing-test lines", st.PassedLinesRemoved))
	}
	if st.ProgressLinesRemoved > 0 {
		removed = append(removed, fmt.Sprintf("%d all-pass progress lines", st.ProgressLinesRemoved))
	}
	if st.DiagnosticsMerged > 0 {
		removed = append(removed, fmt.Sprintf("%d duplicate diagnostics (merged with a count)", st.DiagnosticsMerged))
	}
	if st.SGRRemoved > 0 {
		removed = append(removed, fmt.Sprintf("%d ANSI colour codes", st.SGRRemoved))
	}
	what := "nothing"
	if len(removed) > 0 {
		what = strings.Join(removed, ", ")
	}
	return fmt.Sprintf("[contextslim] %s output compressed. Removed: %s. Failures, errors, diagnostics and the tool's own summary are verbatim below.\n"+
		"[contextslim] Full original (%d lines): %s show %s   (range: --from N --to M)",
		tool, what, st.OriginalLines, showCmd, rawID)
}
