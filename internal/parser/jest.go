package parser

import (
	"fmt"
	"regexp"
	"strings"
)

// Jest handles Jest's default reporter output (with or without --verbose).
//
// Verified: Jest 29.x (29.7.0). Jest does not print its version, so the
// format itself is checked line by line.
type Jest struct{}

var (
	jsSuiteRE    = regexp.MustCompile(`^(PASS|FAIL) \S`)
	jsPassRE     = regexp.MustCompile(`^\s+✓ \S`)
	jsFailRE     = regexp.MustCompile(`^\s+✕ \S`)
	jsSkipRE     = regexp.MustCompile(`^\s+[○✎] \S`)
	jsBulletRE   = regexp.MustCompile(`^\s+● \S`)
	jsConsoleHdr = regexp.MustCompile(`^\s+● Console$`)
	jsConsoleRE  = regexp.MustCompile(`^\s+console\.(log|info|warn|error|debug|trace|dir|table|group)$`)
	jsStackRE    = regexp.MustCompile(`^\s+at \S`)
	jsSuitesRE   = regexp.MustCompile(`^Test Suites:\s+(.+)$`)
	jsTestsRE    = regexp.MustCompile(`^Tests:\s+(.+)$`)
	jsSummaryRE  = regexp.MustCompile(`^(Snapshots:|Time:|Ran all test suites|Ran \d+ tests? of|Snapshot Summary|Summary of all failing tests|Force exiting Jest|Jest did not exit|Test run was interrupted)`)
	jsSnapBullet = regexp.MustCompile(`^\s+› `)
)

var jestWords = map[string]string{
	"passed": "passed", "failed": "failed", "skipped": "skipped", "todo": "todo", "total": "total",
	"obsolete": "obsolete", "written": "written", "updated": "updated", "of": "of",
}

func (Jest) Name() string    { return "jest" }
func (Jest) Version() string { return "jest/1" }

func (Jest) Sniff(lines []string) bool {
	suite, tests := false, false
	for _, l := range lines {
		if jsSuiteRE.MatchString(l) {
			suite = true
		}
		if jsTestsRE.MatchString(l) {
			tests = true
		}
	}
	return suite && tests
}

func (Jest) CheckArgs(args []string) error {
	if a, ok := hasFlag(args, "--json", "--reporters", "--watch", "--watchAll", "--listTests", "--showConfig",
		"--outputFile", "--useStderr", "--notify", "--init", "--clearCache", "--version", "-v", "--help", "-h",
		"--silent=false"); ok {
		return fmt.Errorf("%w: %s", ErrOption, a)
	}
	return nil
}

// jestSection is a PASS/FAIL suite header and the indented lines under it.
type jestSection struct {
	header string // "" for lines before the first suite
	pass   bool
	lines  []string
}

func (j Jest) Parse(lines []string) (*Result, error) {
	res := &Result{Parser: j.Name(), Version: j.Version()}
	var b builder
	var sections []jestSection
	var tail []string // summary lines and everything after them
	cur := jestSection{}
	inSummary := false
	sawSuite, sawTests := false, false
	for _, l := range lines {
		if inSummary {
			tail = append(tail, l)
			continue
		}
		switch {
		case jsSuitesRE.MatchString(l):
			sections = append(sections, cur)
			inSummary = true
			tail = append(tail, l)
		case jsSuiteRE.MatchString(l):
			sawSuite = true
			sections = append(sections, cur)
			cur = jestSection{header: l, pass: strings.HasPrefix(l, "PASS ")}
		case l == "Summary of all failing tests":
			// Printed before failing suites are repeated; a section of its own.
			sections = append(sections, cur)
			cur = jestSection{header: l}
		default:
			cur.lines = append(cur.lines, l)
		}
	}
	if !inSummary {
		sections = append(sections, cur)
	}
	failIDs := 0
	for _, s := range sections {
		if s.header != "" {
			if s.pass && !IsImportant(s.header) {
				b.add(DropPassed, s.header, 0)
			} else {
				b.add(Keep, s.header, 0)
			}
		}
		clean := s.pass && s.header != "" && jestCleanPass(s.lines)
		region := "" // "bullet", "console"
		for _, l := range s.lines {
			blank := strings.TrimSpace(l) == ""
			switch {
			case clean && jsPassRE.MatchString(l) && !IsImportant(l):
				b.add(DropPassed, l, 1)
			case clean && !blank:
				b.add(DropPassed, l, 0) // describe() header of a fully passing suite
			case clean:
				b.add(DropPassed, l, 0)
			case jsConsoleHdr.MatchString(l):
				region = "console"
				b.add(Keep, l, 0)
			case jsBulletRE.MatchString(l):
				region = "bullet"
				res.MustKeep = append(res.MustKeep, l)
				failIDs++
				b.add(Keep, l, 0)
			case jsConsoleRE.MatchString(l):
				region = "console"
				b.add(Keep, l, 0)
			case region != "":
				b.add(Keep, l, 0)
			case jsPassRE.MatchString(l) && !IsImportant(l):
				b.add(DropPassed, l, 1)
			case jsFailRE.MatchString(l):
				res.MustKeep = append(res.MustKeep, l)
				failIDs++
				b.add(Keep, l, 0)
			case jsSkipRE.MatchString(l), blank:
				b.add(Keep, l, 0)
			case s.header != "" && indent(l) >= 2:
				// describe() group names inside a suite with failures.
				b.add(Keep, l, 0)
			default:
				b.add(Unknown, l, 0)
			}
		}
	}
	for _, l := range tail {
		switch {
		case jsSuitesRE.MatchString(l):
			res.MustKeep = append(res.MustKeep, l)
			b.add(Keep, l, 0)
		case jsTestsRE.MatchString(l):
			sawTests = true
			res.MustKeep = append(res.MustKeep, l)
			b.add(Keep, l, 0)
		case jsSummaryRE.MatchString(l), jsSnapBullet.MatchString(l), strings.TrimSpace(l) == "":
			b.add(Keep, l, 0)
		default:
			b.add(Unknown, l, 0)
		}
	}
	if !sawSuite {
		return nil, ErrNotRecognized
	}
	if !inSummary || !sawTests {
		return nil, ErrIncomplete
	}
	counts, err := j.Counts(lines)
	if err != nil {
		return nil, err
	}
	res.Counts = counts
	res.Blocks = b.blocks
	if err := checkUnknown(res.Blocks); err != nil {
		return nil, err
	}
	if counts["tests_failed"] > 0 || counts["suites_failed"] > 0 {
		res.Status = StatusFailed
		if failIDs == 0 {
			return nil, fmt.Errorf("%w: summary reports failures but no failure details were found", ErrInconsistent)
		}
	} else {
		res.Status = StatusPassed
	}
	if droppedPassed(res.Blocks) > counts["tests_passed"] {
		return nil, fmt.Errorf("%w: more passing lines than passed tests", ErrInconsistent)
	}
	return res, nil
}

// jestCleanPass reports whether a PASS suite body contains only passing
// tests, describe() headers and blank lines. Anything else (console output,
// skipped/todo tests, warnings) keeps the suite verbatim except ✓ lines.
func jestCleanPass(lines []string) bool {
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			continue
		case jsPassRE.MatchString(l):
			continue
		case jsBulletRE.MatchString(l), jsConsoleRE.MatchString(l), jsStackRE.MatchString(l),
			jsSkipRE.MatchString(l), jsFailRE.MatchString(l), IsImportant(l):
			return false
		case indent(l) >= 2:
			// A describe header must be followed by a more indented non-blank line.
			next := ""
			for _, n := range lines[i+1:] {
				if strings.TrimSpace(n) != "" {
					next = n
					break
				}
			}
			if next == "" || indent(next) <= indent(l) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Counts reads "Test Suites:" and "Tests:" lines.
func (Jest) Counts(lines []string) (map[string]int, error) {
	out := map[string]int{}
	found := 0
	for _, l := range lines {
		var prefix string
		var m []string
		if m = jsSuitesRE.FindStringSubmatch(l); m != nil {
			prefix = "suites_"
		} else if m = jsTestsRE.FindStringSubmatch(l); m != nil {
			prefix = "tests_"
		} else {
			continue
		}
		c, err := parseCounts(m[1], jestWords)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotRecognized, err)
		}
		if _, dup := out[prefix+"total"]; dup {
			return nil, fmt.Errorf("%w: duplicate summary", ErrInconsistent)
		}
		for k, n := range c {
			out[prefix+k] = n
		}
		found++
	}
	if found < 2 {
		return nil, ErrIncomplete
	}
	return out, nil
}
