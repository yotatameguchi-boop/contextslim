// Package detect decides, from the command string alone, which parser may
// handle a Bash tool output. The command is only tokenized, never executed or
// expanded. Anything ambiguous (pipes, chains, redirects, substitutions,
// variables) is rejected so the original output is passed through.
package detect

import (
	"errors"
	"path"
	"strings"
)

// Reasons for rejecting a command.
var (
	ErrEmpty       = errors.New("empty command")
	ErrCompound    = errors.New("compound command or shell syntax")
	ErrUnsupported = errors.New("command is not a supported tool")
	ErrSubcommand  = errors.New("unsupported subcommand")
)

// Match is the result of command analysis.
type Match struct {
	// Candidates are parser names that may handle the output. More than one
	// means the output must decide (e.g. `npm test` → vitest or jest), and
	// exactly one parser must accept it.
	Candidates []string
	// Args are the arguments passed to the tool (after the tool name and
	// subcommand). For package scripts they are the args after the script name.
	Args []string
	// ViaScript reports that the tool runs through a package.json script, so
	// its real options are unknown.
	ViaScript bool
}

// Analyze tokenizes command and identifies the tool.
func Analyze(command string) (Match, error) {
	toks, err := Tokenize(command)
	if err != nil {
		return Match{}, err
	}
	// Accept a single trailing "2>&1" (stderr merged into stdout) only.
	if n := len(toks); n > 0 && toks[n-1] == "2>&1" {
		toks = toks[:n-1]
	}
	for _, t := range toks {
		if t == "2>&1" {
			return Match{}, ErrCompound
		}
	}
	// Leading environment assignments: FOO=1 CI=true cmd ...
	i := 0
	if i < len(toks) && toks[i] == "env" {
		i++
	}
	for i < len(toks) && isAssignment(toks[i]) {
		i++
	}
	toks = toks[i:]
	if len(toks) == 0 {
		return Match{}, ErrEmpty
	}
	return resolve(toks, 0)
}

func resolve(toks []string, depth int) (Match, error) {
	if len(toks) == 0 || depth > 4 {
		return Match{}, ErrUnsupported
	}
	name := execName(toks[0])
	rest := toks[1:]
	switch name {
	case "npx", "bunx", "pnpx":
		rest = skipFlags(rest)
		return resolve(rest, depth+1)
	case "npm", "pnpm", "yarn", "bun":
		if len(rest) == 0 {
			return Match{}, ErrUnsupported
		}
		sub := rest[0]
		switch {
		case sub == "exec" || (name == "pnpm" && sub == "dlx") || (name == "bun" && sub == "x"):
			r := skipFlags(rest[1:])
			if len(r) > 0 && r[0] == "--" {
				r = r[1:]
			}
			return resolve(r, depth+1)
		case sub == "test" || sub == "t":
			return Match{Candidates: []string{"vitest", "jest"}, Args: scriptArgs(rest[1:]), ViaScript: true}, nil
		case sub == "run" || sub == "run-script":
			if len(rest) > 1 && rest[1] == "test" {
				return Match{Candidates: []string{"vitest", "jest"}, Args: scriptArgs(rest[2:]), ViaScript: true}, nil
			}
			return Match{}, ErrUnsupported
		case name != "npm" && isTool(execName(sub)):
			// pnpm vitest / yarn jest / bun ... run the local binary.
			return resolve(rest, depth+1)
		}
		return Match{}, ErrUnsupported
	case "uv", "poetry", "pipenv", "pdm", "hatch", "rye":
		if len(rest) > 0 && rest[0] == "run" {
			r := skipFlags(rest[1:])
			if len(r) > 0 && r[0] == "--" {
				r = r[1:]
			}
			return resolve(r, depth+1)
		}
		return Match{}, ErrUnsupported
	case "python", "python3", "py":
		for j := 0; j < len(rest); j++ {
			switch {
			case rest[j] == "-m" && j+1 < len(rest):
				return resolve(append([]string{rest[j+1]}, rest[j+2:]...), depth+1)
			case rest[j] == "-B" || rest[j] == "-u" || rest[j] == "-E" || rest[j] == "-s" || rest[j] == "-I":
				continue
			case strings.HasPrefix(rest[j], "-3"): // py -3.12
				continue
			default:
				return Match{}, ErrUnsupported
			}
		}
		return Match{}, ErrUnsupported
	case "vitest":
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			switch rest[0] {
			case "run":
				rest = rest[1:]
			case "watch", "dev", "bench", "list", "init", "related", "typecheck":
				return Match{}, ErrSubcommand
			}
		}
		return Match{Candidates: []string{"vitest"}, Args: rest}, nil
	case "jest":
		return Match{Candidates: []string{"jest"}, Args: rest}, nil
	case "tsc":
		return Match{Candidates: []string{"tsc"}, Args: rest}, nil
	case "pytest", "py.test":
		return Match{Candidates: []string{"pytest"}, Args: rest}, nil
	case "ruff":
		if len(rest) > 0 && rest[0] == "check" {
			return Match{Candidates: []string{"ruff"}, Args: rest[1:]}, nil
		}
		return Match{}, ErrSubcommand
	}
	return Match{}, ErrUnsupported
}

func isTool(n string) bool {
	switch n {
	case "vitest", "jest", "tsc", "pytest", "py.test", "ruff":
		return true
	}
	return false
}

// scriptArgs returns arguments forwarded to a package script ("npm test -- -x").
func scriptArgs(a []string) []string {
	if len(a) > 0 && a[0] == "--" {
		return a[1:]
	}
	return a
}

// skipFlags drops runner options such as `npx --yes` / `uv run --frozen`.
// Options taking a separate value are not supported; an unknown token that
// is not a flag ends the scan and is treated as the program.
func skipFlags(a []string) []string {
	for len(a) > 0 && strings.HasPrefix(a[0], "-") && a[0] != "--" {
		a = a[1:]
	}
	return a
}

// execName returns the lower-cased base name of an executable path with
// Windows extensions removed: ./node_modules/.bin/vitest.cmd → vitest.
func execName(tok string) string {
	tok = strings.ReplaceAll(tok, `\`, "/")
	n := strings.ToLower(path.Base(tok))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		n = strings.TrimSuffix(n, ext)
	}
	return n
}

func isAssignment(t string) bool {
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return false
	}
	for k, r := range t[:eq] {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || k > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// Tokenize splits a command into words, honouring single quotes, double
// quotes and backslash escapes. Any unquoted shell operator, substitution,
// variable expansion or newline returns ErrCompound. The literal token 2>&1
// is returned as-is for the caller to inspect.
func Tokenize(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrEmpty
	}
	var toks []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			toks = append(toks, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
		case c == '\'':
			inWord = true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j >= len(rs) {
				return nil, ErrCompound
			}
			cur.WriteString(string(rs[i+1 : j]))
			i = j
		case c == '"':
			inWord = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				switch rs[j] {
				case '$', '`':
					return nil, ErrCompound
				case '\\':
					if j+1 < len(rs) {
						j++
					}
				}
				cur.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return nil, ErrCompound
			}
			i = j
		case c == '\\':
			if i+1 >= len(rs) || rs[i+1] == '\n' {
				return nil, ErrCompound
			}
			inWord = true
			i++
			cur.WriteRune(rs[i])
		case c == '2' && !inWord && strings.HasPrefix(string(rs[i:]), "2>&1") && (i+4 == len(rs) || rs[i+4] == ' ' || rs[i+4] == '\t'):
			toks = append(toks, "2>&1")
			i += 3
		case strings.ContainsRune(";&|<>`$(){}\n\r", c):
			return nil, ErrCompound
		default:
			inWord = true
			cur.WriteRune(c)
		}
	}
	flush()
	return toks, nil
}
