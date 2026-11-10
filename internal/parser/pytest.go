package parser

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Pytest handles pytest's terminal reporter in default, -q and -v modes
// (including pytest-xdist progress lines).
//
// Verified: pytest 8.x and 9.x. Other major versions are passed through when
// the version is visible in the header.
type Pytest struct{}

var pytestVerifiedMajors = map[int]bool{8: true, 9: true}

var (
	pyStartRE    = regexp.MustCompile(`^=+ test session starts =+$`)
	pyPlatformRE = regexp.MustCompile(`^platform \S+ -- Python \S+, pytest-(\d+)\.(\d+)\.(\d+)`)
	pyHeaderRE   = regexp.MustCompile(`^(rootdir|configfile|cachedir|plugins|testpaths|cov|benchmark|hypothesis profile|django|asyncio|anyio|Using --randomly-seed|created|initialized|ready|bringing up nodes)\b`)
	pyCollectRE  = regexp.MustCompile(`^(collecting \.\.\. )?collected \d+ items?( / \d+ deselected)?( / \d+ selected)?\s*$|^collecting \.\.\.\s*$`)
	// Progress: "tests/test_a.py ....F.  [ 42%]" or (-q) "....F.  [ 42%]".
	pyProgressRE = regexp.MustCompile(`^(?:(\S+\.py) )?([.FEsxXRp]+)\s*\[\s*\d+(?:%|/\s*\d+)\]$`)
	// Verbose: "tests/test_a.py::test_x PASSED   [ 42%]".
	pyVerboseRE = regexp.MustCompile(`^(\S+::\S.*?) (PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS|RERUN)\b.*$`)
	// xdist verbose: "[gw0] [ 42%] PASSED tests/test_a.py::test_x".
	pyXdistRE   = regexp.MustCompile(`^\[gw\d+\] \[\s*\d+%\] (PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS|RERUN) \S+`)
	pySectionRE = regexp.MustCompile(`^=+ (.+?) =+$`)
	pyTestHdrRE = regexp.MustCompile(`^_{3,} (.+?) _{3,}$`)
	pyShortRE   = regexp.MustCompile(`^(FAILED|ERROR) \S`)
	pyInterrRE  = regexp.MustCompile(`^!{3,} .* !{3,}$`)
	pyFinalRE   = regexp.MustCompile(`^=*\s*((?:\d+ [a-z]+(?:, )?)+|no tests ran)(?: in [\d.]+s(?: \([\d:.]+\))?)\s*=*$`)
)

var pytestWords = map[string]string{
	"passed": "passed", "failed": "failed", "error": "errors", "errors": "errors",
	"skipped": "skipped", "deselected": "deselected", "xfailed": "xfailed", "xpassed": "xpassed",
	"warning": "warnings", "warnings": "warnings", "rerun": "rerun", "subtests passed": "subtests_passed",
}

func (Pytest) Name() string    { return "pytest" }
func (Pytest) Version() string { return "pytest/1" }

func (Pytest) Sniff(lines []string) bool {
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-5; i-- {
		if pyFinalRE.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

func (Pytest) CheckArgs(args []string) error {
	if a, ok := hasFlag(args, "--collect-only", "--co", "--version", "-V", "--help", "-h", "--fixtures",
		"--fixtures-per-test", "--markers", "--setup-plan", "--setup-only", "--setup-show", "--debug",
		"--pastebin", "--resultlog", "-n", "--numprocesses", "--looponfail", "-f"); ok {
		// -n is accepted when it comes with xdist's verbose/q output, but its
		// non-verbose progress interleaves workers; pass it through for now.
		return fmt.Errorf("%w: %s", ErrOption, a)
	}
	for i, a := range args {
		if a == "-p" && i+1 < len(args) && strings.HasPrefix(args[i+1], "no:") {
			return fmt.Errorf("%w: -p %s", ErrOption, args[i+1])
		}
		if strings.HasPrefix(a, "-p") && strings.Contains(a, "no:") {
			return fmt.Errorf("%w: %s", ErrOption, a)
		}
	}
	return nil
}

func (p Pytest) Parse(lines []string) (*Result, error) {
	res := &Result{Parser: p.Name(), Version: p.Version()}
	var b builder
	section := "" // current "==== name ====" section other than session start
	sawFinal := false
	failIDs := 0
	for i, l := range lines {
		final := i >= len(lines)-5 && pyFinalRE.MatchString(l)
		isRule := strings.HasPrefix(l, "=")
		switch {
		case final:
			sawFinal = true
			section = ""
			res.MustKeep = append(res.MustKeep, l)
			b.add(Keep, l, 0)
		case isRule && pyStartRE.MatchString(l):
			section = ""
			b.add(Keep, l, 0)
		case isRule && pySectionRE.MatchString(l):
			section = pySectionRE.FindStringSubmatch(l)[1]
			b.add(Keep, l, 0)
		case strings.HasPrefix(l, "!!!") && pyInterrRE.MatchString(l):
			res.Status = StatusInterrupted
			res.MustKeep = append(res.MustKeep, l)
			b.add(Keep, l, 0)
		case section != "":
			// FAILURES / ERRORS / warnings summary / short test summary info /
			// captured output / plugin sections: always verbatim.
			if pyTestHdrRE.MatchString(l) || pyShortRE.MatchString(l) {
				res.MustKeep = append(res.MustKeep, l)
				failIDs++
			}
			b.add(Keep, l, 0)
		case strings.HasPrefix(l, "[gw"):
			m := pyXdistRE.FindStringSubmatch(l)
			switch {
			case m == nil:
				b.add(Unknown, l, 0)
			case m[1] == "PASSED" && !IsImportant(l[strings.Index(l, "PASSED")+6:]):
				b.add(DropPassed, l, 1)
			default:
				b.add(Keep, l, 0)
			}
		case strings.Contains(l, "::"):
			m := pyVerboseRE.FindStringSubmatch(l)
			switch {
			case m == nil:
				b.add(Unknown, l, 0)
			case m[2] == "PASSED" && !IsImportant(m[1]):
				b.add(DropPassed, l, 1)
			default:
				b.add(Keep, l, 0)
			}
		case strings.HasPrefix(l, "platform ") && pyPlatformRE.MatchString(l):
			m := pyPlatformRE.FindStringSubmatch(l)
			res.ToolVersion = m[1] + "." + m[2] + "." + m[3]
			major, _ := strconv.Atoi(m[1])
			if !pytestVerifiedMajors[major] {
				return nil, fmt.Errorf("%w: pytest %s", ErrUnverified, res.ToolVersion)
			}
			b.add(Keep, l, 0)
		case strings.HasSuffix(l, "]") && pyProgressRE.MatchString(l):
			marks := pyProgressRE.FindStringSubmatch(l)[2]
			if strings.Trim(marks, ".") == "" {
				b.add(DropProgress, l, len(marks))
			} else {
				b.add(Keep, l, 0)
			}
		case pyHeaderRE.MatchString(l), pyCollectRE.MatchString(l), strings.TrimSpace(l) == "":
			b.add(Keep, l, 0)
		default:
			b.add(Unknown, l, 0)
		}
	}
	if !sawFinal {
		return nil, ErrIncomplete
	}
	counts, err := p.Counts(lines)
	if err != nil {
		return nil, err
	}
	res.Counts = counts
	res.Blocks = b.blocks
	if err := checkUnknown(res.Blocks); err != nil {
		return nil, err
	}
	switch {
	case res.Status == StatusInterrupted:
	case counts["failed"] > 0 || counts["errors"] > 0:
		res.Status = StatusFailed
	default:
		res.Status = StatusPassed
	}
	if (counts["failed"] > 0 || counts["errors"] > 0) && failIDs == 0 {
		return nil, fmt.Errorf("%w: summary reports failures but no failure details were found", ErrInconsistent)
	}
	if droppedPassed(res.Blocks) > counts["passed"] {
		return nil, fmt.Errorf("%w: more passing marks than passed tests", ErrInconsistent)
	}
	return res, nil
}

// Counts reads the final "N failed, M passed in Xs" line.
func (Pytest) Counts(lines []string) (map[string]int, error) {
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-5; i-- {
		m := pyFinalRE.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		if m[1] == "no tests ran" {
			return map[string]int{}, nil
		}
		c, err := parseCounts(m[1], pytestWords)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotRecognized, err)
		}
		return c, nil
	}
	return nil, ErrIncomplete
}
