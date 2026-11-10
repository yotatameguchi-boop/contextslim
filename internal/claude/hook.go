// Package claude implements the Claude Code hook protocol and settings
// management.
//
// Verified against Claude Code 2.1.322 (see docs/phase0.md):
//   - PostToolUse input carries tool_response as the Bash tool's output object:
//     {"stdout","stderr","interrupted","isImage","noOutputExpected",...}.
//     Claude Code merges stderr into stdout for normal commands.
//   - Returning hookSpecificOutput.updatedToolOutput with the same object shape
//     replaces what the model reads.
//   - Returning nothing (exit 0, empty stdout) leaves the original untouched.
//   - A command that exits non-zero fires PostToolUseFailure instead, with the
//     output in an "error" string ("Exit code N\n..."). That event only
//     accepts additionalContext; replacement is ignored. ContextSlim therefore
//     only observes it.
package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// HookInput is the subset of the PostToolUse input ContextSlim reads. The raw
// tool_response is kept byte-for-byte so unknown fields survive.
type HookInput struct {
	SessionID     string          `json:"session_id"`
	CWD           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolResponse  json.RawMessage `json:"tool_response"`
	// PostToolUseFailure only.
	Error       string `json:"error"`
	IsInterrupt bool   `json:"is_interrupt"`
}

// ParseHookInput decodes hook stdin.
func ParseHookInput(b []byte) (*HookInput, error) {
	var in HookInput
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// Command returns tool_input.command, or "".
func (in *HookInput) Command() string {
	var ti struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &ti) != nil {
		return ""
	}
	return ti.Command
}

// BashResponse is the Bash tool output, validated strictly.
type BashResponse struct {
	Stdout      string
	Stderr      string
	Interrupted bool
	IsImage     bool
	fields      map[string]json.RawMessage
}

// ErrSchema means tool_response does not have the expected Bash shape.
var ErrSchema = errors.New("unexpected Bash tool_response schema")

// ParseBashResponse validates that tool_response is an object with string
// stdout/stderr and boolean interrupted.
func ParseBashResponse(raw json.RawMessage) (*BashResponse, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, ErrSchema
	}
	r := &BashResponse{fields: fields}
	if err := strictString(fields, "stdout", &r.Stdout); err != nil {
		return nil, err
	}
	if err := strictString(fields, "stderr", &r.Stderr); err != nil {
		return nil, err
	}
	b, ok := fields["interrupted"]
	if !ok || json.Unmarshal(b, &r.Interrupted) != nil {
		return nil, ErrSchema
	}
	if b, ok := fields["isImage"]; ok {
		if json.Unmarshal(b, &r.IsImage) != nil {
			return nil, ErrSchema
		}
	}
	return r, nil
}

func strictString(fields map[string]json.RawMessage, k string, dst *string) error {
	b, ok := fields[k]
	if !ok || len(b) == 0 || b[0] != '"' {
		return ErrSchema
	}
	if json.Unmarshal(b, dst) != nil {
		return ErrSchema
	}
	return nil
}

// Incomplete reports outputs that are still running, were moved to the
// background or were persisted to a file because they were too large (the
// stdout field is then only a preview).
func (r *BashResponse) Incomplete() bool {
	for _, k := range []string{"backgroundTaskId", "backgroundedByUser", "backgroundedByTurnAbort", "persistedOutputPath", "rawOutputPath"} {
		if v, ok := r.fields[k]; ok && !isFalsy(v) {
			return true
		}
	}
	return false
}

func isFalsy(v json.RawMessage) bool {
	s := string(bytes.TrimSpace(v))
	return s == "null" || s == "false" || s == `""`
}

// ReplaceField returns tool_response with one string field replaced; every
// other field (known or not) is preserved byte-for-byte.
func ReplaceField(raw json.RawMessage, field, value string) (json.RawMessage, error) {
	obj, err := ParseObject(raw)
	if err != nil {
		return nil, err
	}
	if _, ok := obj.Get(field); !ok {
		return nil, fmt.Errorf("field %q missing", field)
	}
	v, err := marshalNoEscape(value)
	if err != nil {
		return nil, err
	}
	obj.Set(field, v)
	return obj.MarshalJSON()
}

// HookOutput builds the PostToolUse hook stdout.
func HookOutput(updated json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":`)
	buf.Write(updated)
	buf.WriteString("}}\n")
	if !json.Valid(buf.Bytes()) {
		return nil, errors.New("invalid hook output")
	}
	return buf.Bytes(), nil
}

func marshalNoEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
