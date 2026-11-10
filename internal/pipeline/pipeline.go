// Package pipeline implements the PostToolUse processing flow (design 6).
package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/yotatameguchi-boop/contextslim/internal/claude"
	"github.com/yotatameguchi-boop/contextslim/internal/compress"
	"github.com/yotatameguchi-boop/contextslim/internal/config"
	"github.com/yotatameguchi-boop/contextslim/internal/detect"
	"github.com/yotatameguchi-boop/contextslim/internal/metrics"
	"github.com/yotatameguchi-boop/contextslim/internal/parser"
	"github.com/yotatameguchi-boop/contextslim/internal/store"
	"github.com/yotatameguchi-boop/contextslim/internal/textutil"
	"github.com/yotatameguchi-boop/contextslim/internal/validate"
	"github.com/yotatameguchi-boop/contextslim/internal/version"
)

// Skip reasons recorded in metrics.
const (
	ReasonBadJSON        = "invalid_json"
	ReasonNotPostToolUse = "not_post_tool_use"
	ReasonNotBash        = "not_bash"
	ReasonSchema         = "schema_mismatch"
	ReasonSelf           = "contextslim_command"
	ReasonCommand        = "unsupported_command"
	ReasonOption         = "unsupported_option"
	ReasonDisabled       = "parser_disabled"
	ReasonInterrupted    = "interrupted"
	ReasonBackground     = "background_or_incomplete"
	ReasonImage          = "image"
	ReasonTooSmall       = "below_min_input_bytes"
	ReasonTooLarge       = "above_max_input_bytes"
	ReasonText           = "unsupported_text"
	ReasonFormat         = "format_not_recognized"
	ReasonAmbiguous      = "ambiguous_format"
	ReasonParse          = "parse_rejected"
	ReasonUnverified     = "unverified_tool_version"
	ReasonUnknownBlock   = "unknown_important_block"
	ReasonValidation     = "validation_failed"
	ReasonNotSmaller     = "insufficient_savings"
	ReasonStorage        = "storage_failed"
	ReasonStorageFull    = "storage_limit"
	ReasonBudget         = "processing_budget_exceeded"
	ReasonInternal       = "internal_error"
	ReasonConfig         = "config_error"
	// ReasonFailureEvent marks a candidate built from a PostToolUseFailure
	// event (non-zero exit). Claude Code 2.1.322 cannot replace output on that
	// path, so these are always observation-only.
	ReasonFailureEvent = "failure_event_not_replaceable"
)

var exitCodePrefix = regexp.MustCompile(`^Exit code -?\d+\n`)

// Env holds dependencies.
type Env struct {
	Config  config.Config
	Store   *store.Store
	ShowCmd string // how Claude should invoke ContextSlim, e.g. "contextslim"
	Now     func() time.Time
}

// Outcome is the result of processing one hook invocation.
type Outcome struct {
	// Output is the JSON to write to stdout, or nil to pass the original
	// through unchanged.
	Output     []byte
	Event      metrics.Event
	SessionKey string
}

func skip(o *Outcome, reason string) *Outcome {
	o.Event.Action = metrics.ActionSkipped
	o.Event.Reason = reason
	o.Output = nil
	return o
}

// Process handles one hook input. It never returns an error: every failure
// becomes a pass-through with a recorded reason.
func Process(env Env, input []byte) (out *Outcome) {
	o := &Outcome{SessionKey: store.SessionKey("")}
	o.Event.Mode = env.Config.Mode
	defer func() {
		if r := recover(); r != nil {
			out = skip(o, ReasonInternal)
		}
	}()
	in, err := claude.ParseHookInput(input)
	if err != nil {
		return skip(o, ReasonBadJSON)
	}
	o.SessionKey = store.SessionKey(in.SessionID)
	if in.HookEventName == "PostToolUseFailure" {
		return processFailure(env, o, in)
	}
	if in.HookEventName != "PostToolUse" {
		return skip(o, ReasonNotPostToolUse)
	}
	if in.ToolName != "Bash" {
		return skip(o, ReasonNotBash)
	}
	resp, err := claude.ParseBashResponse(in.ToolResponse)
	if err != nil {
		return skip(o, ReasonSchema)
	}
	command := in.Command()
	if isSelfCommand(command) {
		return skip(o, ReasonSelf)
	}
	match, err := detect.Analyze(command)
	if err != nil {
		return skip(o, ReasonCommand)
	}
	switch {
	case resp.Interrupted:
		return skip(o, ReasonInterrupted)
	case resp.IsImage:
		return skip(o, ReasonImage)
	case resp.Incomplete():
		return skip(o, ReasonBackground)
	}

	// Pick the field to compress: stdout (Claude Code merges stderr into it)
	// or, if stdout is empty, stderr. Both non-empty: try stdout first.
	fields := []string{"stdout", "stderr"}
	var lastReason = ReasonFormat
	for _, field := range fields {
		text := resp.Stdout
		if field == "stderr" {
			text = resp.Stderr
		}
		if text == "" {
			continue
		}
		r := tryField(env, o, in, resp, match, field, text, false)
		if r == "" {
			return o
		}
		lastReason = r
		if r != ReasonFormat && r != ReasonTooSmall {
			break
		}
	}
	return skip(o, lastReason)
}

// tryField attempts compression of one field. It returns "" on success (o is
// filled) or a skip reason.
func tryField(env Env, o *Outcome, in *claude.HookInput, resp *claude.BashResponse, match detect.Match, field, text string, observeOnly bool) string {
	cfg := env.Config
	o.Event.BytesIn = len(text)
	if len(text) < cfg.MinInputBytes {
		return ReasonTooSmall
	}
	if len(text) > cfg.MaxInputBytes {
		return ReasonTooLarge
	}
	norm, err := textutil.Normalize(text)
	if err != nil {
		return ReasonText
	}
	reg := parser.Registry()
	var chosen parser.Parser
	disabled := false
	for _, name := range match.Candidates {
		p := reg[name]
		if !cfg.ParserEnabled(name) {
			disabled = true
			continue
		}
		if !p.Sniff(norm.Lines) {
			continue
		}
		if chosen != nil {
			return ReasonAmbiguous
		}
		chosen = p
	}
	if chosen == nil {
		if disabled {
			return ReasonDisabled
		}
		return ReasonFormat
	}
	o.Event.Parser = chosen.Name()
	o.Event.ParserVersion = chosen.Version()
	if err := chosen.CheckArgs(match.Args); err != nil {
		return ReasonOption
	}
	res, err := chosen.Parse(norm.Lines)
	if res != nil {
		o.Event.ToolVersion = res.ToolVersion
	}
	if err != nil {
		switch {
		case errors.Is(err, parser.ErrUnverified):
			return ReasonUnverified
		case errors.Is(err, parser.ErrUnknownImportant):
			return ReasonUnknownBlock
		case errors.Is(err, parser.ErrNotRecognized):
			return ReasonFormat
		case errors.Is(err, parser.ErrOption):
			return ReasonOption
		default:
			return ReasonParse
		}
	}
	o.Event.ToolVersion = res.ToolVersion
	body, st := compress.Body(res)
	st.SGRRemoved = norm.SGRRemoved
	st.OriginalLines = len(norm.Lines)
	if err := validate.Check(chosen, res, body); err != nil {
		return ReasonValidation
	}
	if norm.TrailingNewline {
		body += "\n"
	}
	// Size check with a placeholder id of the final length.
	placeholder := "cs_" + strings.Repeat("0", 20)
	candidate := compress.Header(chosen.Name(), placeholder, env.ShowCmd, st) + "\n" + body
	o.Event.BytesOut = len(candidate)
	o.Event.Estimator = cfg.TokenEstimator
	o.Event.TokensInEst = metrics.EstimateTokens(cfg.TokenEstimator, len(text))
	o.Event.TokensOutEst = metrics.EstimateTokens(cfg.TokenEstimator, len(candidate))
	saved := len(text) - len(candidate)
	if saved < cfg.MinSavingsBytes || float64(saved) < cfg.MinSavingsRatio*float64(len(text)) {
		return ReasonNotSmaller
	}
	if observeOnly || cfg.Mode != config.ModeCompress {
		o.Event.Action = metrics.ActionObserved
		o.Output = nil
		return ""
	}

	// Save the original before replacing anything.
	rawResp := in.ToolResponse
	if err := ensureCapacity(env, int64(len(rawResp))+4096); err != nil {
		if errors.Is(err, store.ErrNoSpace) {
			return ReasonStorageFull
		}
		return ReasonStorage
	}
	id, err := env.Store.SaveRaw(store.Record{
		SessionKey:   o.SessionKey,
		ToolName:     in.ToolName,
		Command:      in.Command(),
		Parser:       chosen.Name(),
		Compressor:   version.Compressor,
		Field:        field,
		ToolResponse: rawResp,
	}, time.Duration(cfg.RetentionDays)*24*time.Hour)
	if err != nil {
		return ReasonStorage
	}
	final := compress.Header(chosen.Name(), id, env.ShowCmd, st) + "\n" + body
	updated, err := claude.ReplaceField(in.ToolResponse, field, final)
	if err != nil {
		return ReasonSchema
	}
	outJSON, err := claude.HookOutput(updated)
	if err != nil {
		return ReasonInternal
	}
	o.Output = outJSON
	o.Event.Action = metrics.ActionCompressed
	o.Event.RawID = id
	o.Event.BytesOut = len(final)
	o.Event.TokensOutEst = metrics.EstimateTokens(cfg.TokenEstimator, len(final))
	return ""
}

func ensureCapacity(env Env, need int64) error {
	cfg := env.Config
	opts := store.PruneOptions{
		Retention:        time.Duration(cfg.RetentionDays) * 24 * time.Hour,
		MetricsRetention: time.Duration(cfg.MetricsRetentionDays) * 24 * time.Hour,
		MaxBytes:         cfg.MaxStorageBytes,
		Reserve:          need,
	}
	if env.Store.AutoPruneDue(time.Hour) {
		_, err := env.Store.Prune(opts)
		env.Store.TouchPruneStamp()
		return err
	}
	usage, err := env.Store.Usage()
	if err != nil {
		return err
	}
	if usage+need <= cfg.MaxStorageBytes {
		return nil
	}
	_, err = env.Store.Prune(opts)
	return err
}

// processFailure handles PostToolUseFailure (the Bash command exited
// non-zero, e.g. failing tests). Its output cannot be replaced, so the
// candidate is built and recorded for statistics only; Output stays nil.
func processFailure(env Env, o *Outcome, in *claude.HookInput) *Outcome {
	if in.ToolName != "Bash" {
		return skip(o, ReasonNotBash)
	}
	if in.IsInterrupt {
		return skip(o, ReasonInterrupted)
	}
	command := in.Command()
	if isSelfCommand(command) {
		return skip(o, ReasonSelf)
	}
	match, err := detect.Analyze(command)
	if err != nil {
		return skip(o, ReasonCommand)
	}
	text := exitCodePrefix.ReplaceAllString(in.Error, "")
	if text == "" {
		return skip(o, ReasonSchema)
	}
	if r := tryField(env, o, in, nil, match, "error", text, true); r != "" {
		return skip(o, r)
	}
	o.Event.Reason = ReasonFailureEvent
	o.Output = nil
	return o
}

// isSelfCommand reports whether the Bash command runs ContextSlim itself
// (e.g. `contextslim show ...`); its output is never recompressed.
func isSelfCommand(cmd string) bool {
	return strings.Contains(strings.ToLower(cmd), "contextslim")
}

// Describe returns a one-line human description of an outcome (for debug).
func Describe(o *Outcome) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "action=%s", o.Event.Action)
	if o.Event.Reason != "" {
		fmt.Fprintf(&b, " reason=%s", o.Event.Reason)
	}
	if o.Event.Parser != "" {
		fmt.Fprintf(&b, " parser=%s", o.Event.Parser)
	}
	if o.Event.BytesIn > 0 {
		fmt.Fprintf(&b, " bytes=%d->%d", o.Event.BytesIn, o.Event.BytesOut)
	}
	return b.String()
}
