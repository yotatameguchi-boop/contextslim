package parser

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Vitest handles the default and verbose reporters of Vitest (non-TTY output).
//
// Verified: Vitest 3.x (3.2.7). Other major versions are passed through.
type Vitest struct{}

var vitestVerifiedMajors = map[int]bool{3: true}

var (
	vtRunRE      = regexp.MustCompile(`^\s*RUN\s+v(\d+)\.(\d+)\.(\d+)(?:\S*)(?:\s|$)`)
	vtFilePassRE = regexp.MustCompile(`^\s*✓ (\S.*?) \((\d+) tests?\)(?: \d+(?:\.\d+)?m?s)?$`)
	vtTestPassRE = regexp.MustCompile(`^\s*✓ \S.*$`)
	vtFileFailRE = regexp.MustCompile(`^\s*❯ \S.* \(\d+ tests? \|.*\)(?: \d+(?:\.\d+)?m?s)?$`)
	vtTestFailRE = regexp.MustCompile(`^\s*[×✗] \S`)
	vtReasonRE   = regexp.MustCompile(`^\s*→ `)
	vtSkipRE     = regexp.MustCompile(`^\s*[↓↩] \S`)
	vtUserOutRE  = regexp.MustCompile(`^(stdout|stderr) \| \S`)
	vtSectionRE  = regexp.MustCompile(`^⎯+ .+ ⎯+$`)
	vtFailHdrRE  = regexp.MustCompile(`^\s*FAIL\s+\S`)
	vtFilesRE    = regexp.MustCompile(`^\s*Test Files\s+(.+?)\s+\((\d+)\)$`)
	vtTestsRE    = regexp.MustCompile(`^\s*Tests\s+(.+?)\s+\((\d+)\)$`)
	vtSummaryRE  = regexp.MustCompile(`^\s*(Start at|Duration|Errors|Type Errors)\s{2,}\S`)
)

var vitestWords = map[string]string{
	"passed": "passed", "failed": "failed", "skipped": "skipped", "todo": "todo",
	"expected fail": "expected_fail",
}

func (Vitest) Name() string    { return "vitest" }
func (Vitest) Version() string { return "vitest/1" }

func (Vitest) Sniff(lines []string) bool {
	run, tests := false, false
	for _, l := range lines {
		if vtRunRE.MatchString(l) {
			run = true
		}
		if vtTestsRE.MatchString(l) {
			tests = true
		}
	}
	return run && tests
}

func (Vitest) CheckArgs(args []string) error {
	if a, ok := hasFlag(args, "--watch", "-w", "--ui", "--api", "--json", "--merge-reports", "--standalone", "--list", "--browser"); ok {
		return fmt.Errorf("%w: %s", ErrOption, a)
	}
	for i, a := range args {
		var v string
		switch {
		case a == "--reporter" && i+1 < len(args):
			v = args[i+1]
		case strings.HasPrefix(a, "--reporter="):
			v = strings.TrimPrefix(a, "--reporter=")
		default:
			continue
		}
		if v != "default" && v != "verbose" {
			return fmt.Errorf("%w: --reporter %s", ErrOption, v)
		}
	}
	return nil
}

func (v Vitest) Parse(lines []string) (*Result, error) {
	res := &Result{Parser: v.Name(), Version: v.Version()}
	var b builder
	inFailures := false // inside "Failed Tests"/"Unhandled Errors" sections
	inUserOut := false  // inside a "stdout | test" block
	sawRun, sawSummary := false, false
	failIDs := 0
	for _, l := range lines {
		switch {
		case vtFilesRE.MatchString(l) || vtTestsRE.MatchString(l):
			inFailures, inUserOut = false, false
			sawSummary = true
			b.add(Keep, l, 0)
			res.MustKeep = append(res.MustKeep, l)
		case inFailures:
			if vtFailHdrRE.MatchString(l) {
				res.MustKeep = append(res.MustKeep, l)
				failIDs++
			}
			b.add(Keep, l, 0)
		case vtSectionRE.MatchString(l):
			inFailures, inUserOut = true, false
			b.add(Keep, l, 0)
		case inUserOut:
			if strings.TrimSpace(l) == "" {
				inUserOut = false
			}
			b.add(Keep, l, 0)
		case strings.TrimSpace(l) == "":
			b.add(Keep, l, 0)
		case vtRunRE.MatchString(l) && !sawRun:
			sawRun = true
			m := vtRunRE.FindStringSubmatch(l)
			major, _ := strconv.Atoi(m[1])
			res.ToolVersion = m[1] + "." + m[2] + "." + m[3]
			if !vitestVerifiedMajors[major] {
				return nil, fmt.Errorf("%w: vitest %s", ErrUnverified, res.ToolVersion)
			}
			b.add(Keep, l, 0)
		case vtUserOutRE.MatchString(l):
			inUserOut = true
			b.add(Keep, l, 0)
		case vtFilePassRE.MatchString(l) && !IsImportant(l):
			n, _ := strconv.Atoi(vtFilePassRE.FindStringSubmatch(l)[2])
			b.add(DropPassed, l, n)
		case vtTestPassRE.MatchString(l) && !IsImportant(l):
			b.add(DropPassed, l, 1)
		case vtTestFailRE.MatchString(l):
			res.MustKeep = append(res.MustKeep, l)
			failIDs++
			b.add(Keep, l, 0)
		case vtFileFailRE.MatchString(l), vtReasonRE.MatchString(l), vtSkipRE.MatchString(l),
			vtSummaryRE.MatchString(l), vtTestPassRE.MatchString(l):
			b.add(Keep, l, 0)
		default:
			b.add(Unknown, l, 0)
		}
	}
	if !sawRun {
		return nil, ErrNotRecognized
	}
	if !sawSummary {
		return nil, ErrIncomplete
	}
	counts, err := v.Counts(lines)
	if err != nil {
		return nil, err
	}
	res.Counts = counts
	res.Blocks = b.blocks
	if err := checkUnknown(res.Blocks); err != nil {
		return nil, err
	}
	if counts["tests_failed"] > 0 || counts["files_failed"] > 0 {
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

// Counts reads the "Test Files" and "Tests" summary lines.
func (Vitest) Counts(lines []string) (map[string]int, error) {
	out := map[string]int{}
	found := 0
	for _, l := range lines {
		var prefix string
		var m []string
		if m = vtFilesRE.FindStringSubmatch(l); m != nil {
			prefix = "files_"
		} else if m = vtTestsRE.FindStringSubmatch(l); m != nil {
			prefix = "tests_"
		} else {
			continue
		}
		c, err := parseCounts(m[1], vitestWords)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotRecognized, err)
		}
		if _, dup := out[prefix+"total"]; dup {
			return nil, fmt.Errorf("%w: duplicate summary", ErrInconsistent)
		}
		for k, n := range c {
			out[prefix+k] = n
		}
		total, _ := strconv.Atoi(m[2])
		out[prefix+"total"] = total
		found++
	}
	if _, ok := out["tests_total"]; !ok || found == 0 {
		return nil, ErrIncomplete
	}
	return out, nil
}
