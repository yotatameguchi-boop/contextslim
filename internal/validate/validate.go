// Package validate checks that a compressed body preserves the facts the
// parser extracted (design 8.5). It is a safety net, not a proof: parser
// blind spots are covered by real-log review and regression tests.
package validate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yotatameguchi-boop/contextslim/internal/parser"
)

// ErrLost means compression would lose or alter information.
var ErrLost = errors.New("information check failed")

// Check verifies body against the parse result of the original output.
func Check(p parser.Parser, res *parser.Result, body string) error {
	lines := strings.Split(body, "\n")
	present := make(map[string]int, len(lines))
	for _, l := range lines {
		present[l]++
	}
	// 1. Every must-keep line (failure ids, diagnostic headers, summary lines)
	//    is present verbatim.
	for _, m := range res.MustKeep {
		if present[m] == 0 {
			return fmt.Errorf("%w: missing %q", ErrLost, m)
		}
	}
	// 2. Summary counts re-read from the compressed text equal the original.
	got, err := p.Counts(lines)
	if err != nil {
		return fmt.Errorf("%w: summary unreadable after compression: %v", ErrLost, err)
	}
	if !parser.EqualCounts(got, res.Counts) {
		return fmt.Errorf("%w: counts changed %v -> %v", ErrLost, res.Counts, got)
	}
	// 3. Nothing that mentions an error/failure was dropped.
	for _, b := range res.Blocks {
		if b.Kind != parser.DropPassed && b.Kind != parser.DropProgress {
			continue
		}
		for _, l := range b.Lines {
			if parser.IsImportant(l) {
				return fmt.Errorf("%w: dropped line mentions a failure: %q", ErrLost, l)
			}
		}
	}
	// 4. A failed or interrupted run is never presented without its failure
	//    markers (the tool's own status lines are must-keep, checked above).
	if res.Status != parser.StatusPassed && len(res.MustKeep) == 0 {
		return fmt.Errorf("%w: non-passing run without retained failure markers", ErrLost)
	}
	return nil
}
