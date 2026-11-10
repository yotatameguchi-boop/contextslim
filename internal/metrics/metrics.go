// Package metrics records per-session events and aggregates them for `stats`.
//
// Byte counts are measured values. Token counts are estimates and are only
// produced when a token estimator is configured; they always carry the
// estimator name so they are never mistaken for Claude's real token usage.
package metrics

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yotatameguchi-boop/contextslim/internal/config"
)

// Actions.
const (
	ActionCompressed = "compressed" // output was replaced
	ActionObserved   = "observed"   // observe mode: candidate built, output unchanged
	ActionSkipped    = "skipped"    // passed through unchanged
	ActionShow       = "show"       // original output re-fetched
)

// Event is one JSONL line.
type Event struct {
	Time          string `json:"ts"`
	Action        string `json:"action"`
	Mode          string `json:"mode,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Parser        string `json:"parser,omitempty"`
	ParserVersion string `json:"parser_version,omitempty"`
	ToolVersion   string `json:"tool_version,omitempty"`
	BytesIn       int    `json:"bytes_in,omitempty"`
	BytesOut      int    `json:"bytes_out,omitempty"`
	Estimator     string `json:"token_estimator,omitempty"`
	TokensInEst   int    `json:"tokens_in_est,omitempty"`
	TokensOutEst  int    `json:"tokens_out_est,omitempty"`
	DurationUS    int64  `json:"duration_us,omitempty"`
	RawID         string `json:"raw_id,omitempty"`
}

// EstimateTokens returns an estimated token count, or 0 when the estimator is
// "none".
func EstimateTokens(estimator string, bytes int) int {
	switch estimator {
	case config.EstimatorBytesDiv4:
		return (bytes + 3) / 4
	default:
		return 0
	}
}

var keyPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Append writes one event to metrics/<sessionKey>.jsonl. Each event is written
// with a single O_APPEND write so concurrent hook processes do not interleave.
func Append(dir, sessionKey string, e Event) error {
	if !keyPattern.MatchString(sessionKey) {
		return errors.New("invalid session key")
	}
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, sessionKey+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// ReadAll loads events from every session file (or one session if key != "").
func ReadAll(dir, key string) ([]Event, int, error) {
	pattern := "*.jsonl"
	if key != "" {
		if !keyPattern.MatchString(key) {
			return nil, 0, errors.New("invalid session key")
		}
		pattern = key + ".jsonl"
	}
	files, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return nil, 0, err
	}
	var events []Event
	sessions := 0
	for _, p := range files {
		if !keyPattern.MatchString(strings.TrimSuffix(filepath.Base(p), ".jsonl")) {
			continue
		}
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		sessions++
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				events = append(events, e)
			}
		}
		f.Close()
	}
	return events, sessions, nil
}

// Summary aggregates events.
type Summary struct {
	Sessions   int `json:"sessions"`
	HookRuns   int `json:"hook_runs"`
	Compressed int `json:"compressed"`
	Observed   int `json:"observed"`
	// ObservedFailure counts observed candidates from non-zero-exit commands,
	// which Claude Code does not allow to be replaced.
	ObservedFailure    int                       `json:"observed_failure_path"`
	ObservedFailureIn  int64                     `json:"observed_failure_bytes_in"`
	ObservedFailureOut int64                     `json:"observed_failure_bytes_out"`
	Skipped            int                       `json:"skipped"`
	SkipReasons        map[string]int            `json:"skip_reasons"`
	CompressedIn       int64                     `json:"compressed_bytes_in"`
	CompressedOut      int64                     `json:"compressed_bytes_out"`
	ObservedIn         int64                     `json:"observed_bytes_in"`
	ObservedOut        int64                     `json:"observed_bytes_out"`
	Estimators         []string                  `json:"token_estimators"`
	TokensInEst        int64                     `json:"tokens_in_est"`
	TokensOutEst       int64                     `json:"tokens_out_est"`
	Shows              int                       `json:"raw_refetches"`
	DurationP50US      int64                     `json:"duration_p50_us"`
	DurationP95US      int64                     `json:"duration_p95_us"`
	DurationMaxUS      int64                     `json:"duration_max_us"`
	PerParser          map[string]*ParserSummary `json:"per_parser"`
	ToolVersions       map[string][]string       `json:"tool_versions"`
	ParserVersions     []string                  `json:"parser_versions"`
}

// ParserSummary is per-parser aggregation.
type ParserSummary struct {
	Compressed int `json:"compressed"`
	Observed   int `json:"observed"`
	// ObservedFailure counts observed candidates from non-zero-exit commands,
	// which Claude Code does not allow to be replaced.
	ObservedFailure    int   `json:"observed_failure_path"`
	ObservedFailureIn  int64 `json:"observed_failure_bytes_in"`
	ObservedFailureOut int64 `json:"observed_failure_bytes_out"`
	Skipped            int   `json:"skipped"`
	BytesIn            int64 `json:"bytes_in"`
	BytesOut           int64 `json:"bytes_out"`
}

// Summarize aggregates events.
func Summarize(events []Event, sessions int) Summary {
	s := Summary{Sessions: sessions, SkipReasons: map[string]int{}, PerParser: map[string]*ParserSummary{}, ToolVersions: map[string][]string{}}
	var durs []int64
	estimators := map[string]bool{}
	pversions := map[string]bool{}
	tversions := map[string]map[string]bool{}
	for _, e := range events {
		if e.Action == ActionShow {
			s.Shows++
			continue
		}
		s.HookRuns++
		durs = append(durs, e.DurationUS)
		var ps *ParserSummary
		if e.Parser != "" {
			ps = s.PerParser[e.Parser]
			if ps == nil {
				ps = &ParserSummary{}
				s.PerParser[e.Parser] = ps
			}
			if e.ParserVersion != "" {
				pversions[e.ParserVersion] = true
			}
			if e.ToolVersion != "" {
				if tversions[e.Parser] == nil {
					tversions[e.Parser] = map[string]bool{}
				}
				tversions[e.Parser][e.ToolVersion] = true
			}
		}
		switch e.Action {
		case ActionCompressed:
			s.Compressed++
			s.CompressedIn += int64(e.BytesIn)
			s.CompressedOut += int64(e.BytesOut)
			if e.Estimator != "" && e.Estimator != config.EstimatorNone {
				estimators[e.Estimator] = true
				s.TokensInEst += int64(e.TokensInEst)
				s.TokensOutEst += int64(e.TokensOutEst)
			}
			if ps != nil {
				ps.Compressed++
				ps.BytesIn += int64(e.BytesIn)
				ps.BytesOut += int64(e.BytesOut)
			}
		case ActionObserved:
			s.Observed++
			if e.Reason == "failure_event_not_replaceable" {
				s.ObservedFailure++
				s.ObservedFailureIn += int64(e.BytesIn)
				s.ObservedFailureOut += int64(e.BytesOut)
			}
			s.ObservedIn += int64(e.BytesIn)
			s.ObservedOut += int64(e.BytesOut)
			if ps != nil {
				ps.Observed++
				ps.BytesIn += int64(e.BytesIn)
				ps.BytesOut += int64(e.BytesOut)
			}
		default:
			s.Skipped++
			s.SkipReasons[e.Reason]++
			if ps != nil {
				ps.Skipped++
			}
		}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	if n := len(durs); n > 0 {
		s.DurationP50US = durs[(n-1)*50/100]
		s.DurationP95US = durs[(n-1)*95/100]
		s.DurationMaxUS = durs[n-1]
	}
	for k := range estimators {
		s.Estimators = append(s.Estimators, k)
	}
	sort.Strings(s.Estimators)
	for k := range pversions {
		s.ParserVersions = append(s.ParserVersions, k)
	}
	sort.Strings(s.ParserVersions)
	for p, m := range tversions {
		for v := range m {
			s.ToolVersions[p] = append(s.ToolVersions[p], v)
		}
		sort.Strings(s.ToolVersions[p])
	}
	return s
}
