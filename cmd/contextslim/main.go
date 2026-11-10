// Command contextslim compresses known test/lint/build outputs that Claude Code
// reads through its Bash tool, via a PostToolUse hook.
package main

import (
	"os"

	"github.com/yotatameguchi-boop/contextslim/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
