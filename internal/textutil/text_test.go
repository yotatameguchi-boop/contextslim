package textutil

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	tx, err := Normalize("a\r\n\x1b[31mred\x1b[39m 日本語\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tx.Lines, []string{"a", "red 日本語"}) || !tx.CRLF || !tx.TrailingNewline || tx.SGRRemoved != 2 {
		t.Fatalf("%+v", tx)
	}
	for in, want := range map[string]error{
		"a\x00b":          ErrBinary,
		"\xff\xfe":        ErrBinary,
		"50%\r60%\r":      ErrCarriageReturn,
		"x\x1b[2Ky":       ErrEscape,
		"x\x1b[1A\x1b[0m": ErrEscape,
	} {
		if _, err := Normalize(in); !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", in, err, want)
		}
	}
	if n := CountLines("a\nb"); n != 2 {
		t.Error(n)
	}
	if n := CountLines("a\nb\n"); n != 2 {
		t.Error(n)
	}
}
