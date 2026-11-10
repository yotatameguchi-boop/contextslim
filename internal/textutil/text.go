// Package textutil normalizes tool output before parsing.
package textutil

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Errors returned by Normalize. Each one means "do not compress".
var (
	ErrBinary         = errors.New("binary or non-UTF-8 output")
	ErrCarriageReturn = errors.New("bare carriage return (in-place progress rendering)")
	ErrEscape         = errors.New("terminal escape sequence other than SGR colour")
)

// sgr matches only Select Graphic Rendition sequences (colours, bold, inverse).
// These are the only decorations ContextSlim removes; any other escape sequence
// (cursor movement, line clearing) makes the output unsupported.
var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

// Text is normalized output split into lines.
type Text struct {
	Lines []string
	// TrailingNewline reports whether the original ended with a newline.
	TrailingNewline bool
	// CRLF reports whether CRLF line endings were converted.
	CRLF bool
	// SGRRemoved counts removed colour sequences.
	SGRRemoved int
	// SGRBytes is the number of bytes those sequences occupied.
	SGRBytes int
}

// Normalize validates s and returns its lines with SGR colour codes removed.
func Normalize(s string) (Text, error) {
	var t Text
	if strings.IndexByte(s, 0) >= 0 || !utf8.ValidString(s) {
		return t, ErrBinary
	}
	if strings.Contains(s, "\r\n") {
		t.CRLF = true
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	if strings.IndexByte(s, '\r') >= 0 {
		return t, ErrCarriageReturn
	}
	if strings.IndexByte(s, 0x1b) >= 0 {
		before := len(s)
		t.SGRRemoved = len(sgr.FindAllStringIndex(s, -1))
		s = sgr.ReplaceAllString(s, "")
		t.SGRBytes = before - len(s)
		if strings.IndexByte(s, 0x1b) >= 0 {
			return t, ErrEscape
		}
	}
	if strings.HasSuffix(s, "\n") {
		t.TrailingNewline = true
		s = s[:len(s)-1]
	}
	if s == "" {
		t.Lines = nil
		return t, nil
	}
	t.Lines = strings.Split(s, "\n")
	return t, nil
}

// CountLines returns the number of lines in s the way `show` numbers them.
func CountLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
