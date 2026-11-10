package cli

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yotatameguchi-boop/contextslim/internal/claude"
	"github.com/yotatameguchi-boop/contextslim/internal/config"
	"github.com/yotatameguchi-boop/contextslim/internal/metrics"
	"github.com/yotatameguchi-boop/contextslim/internal/pipeline"
	"github.com/yotatameguchi-boop/contextslim/internal/store"
)

var (
	sessionKeyRE = regexp.MustCompile(`^[0-9a-f]{16}$`)
	assignRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

// runWrapped implements `contextslim run --session KEY --cmd B64 -- argv...`,
// the command a PreToolUse hook substitutes for a supported Bash command.
//
// Guarantees:
//   - the program runs with the same argv, environment, working directory and
//     stdin it would have had (leading VAR=value / env are applied here,
//     because the shell no longer treats them as assignments);
//   - stdout and stderr go to one pipe, as Claude Code merges them anyway;
//   - the exit code is the program's own (128+N when killed by signal N), so
//     Claude Code still sees success/failure exactly as before;
//   - the printed text is the compressed output, or the original output
//     byte for byte whenever compression is not possible;
//   - ContextSlim's own problems never change the program's result.
func runWrapped(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var session, cmdB64 string
	i := 0
	for ; i < len(args); i++ {
		switch {
		case args[i] == "--":
			i++
			goto parsed
		case args[i] == "--session" && i+1 < len(args):
			session = args[i+1]
			i++
		case args[i] == "--cmd" && i+1 < len(args):
			cmdB64 = args[i+1]
			i++
		default:
			fmt.Fprintf(stderr, "contextslim run: unexpected argument %q\n", args[i])
			return 2
		}
	}
parsed:
	argv := args[i:]
	if len(argv) > 0 && argv[0] == "env" {
		argv = argv[1:]
	}
	var extraEnv []string
	for len(argv) > 0 && assignRE.MatchString(argv[0]) {
		extraEnv = append(extraEnv, argv[0])
		argv = argv[1:]
	}
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "contextslim run: no command")
		return 2
	}
	original := ""
	if b, err := base64.RawURLEncoding.DecodeString(cmdB64); err == nil {
		original = string(b)
	}
	start := time.Now()

	// Load configuration; if anything is wrong, just run the program with its
	// output going straight through.
	dir, derr := dataDir()
	cfg, cerr := config.Load(dir)
	if derr != nil || cerr != nil || !sessionKeyRE.MatchString(session) || original == "" {
		return execDirect(argv, extraEnv, stdin, stdout, stderr)
	}

	path, err := exec.LookPath(argv[0])
	if err != nil {
		// Mirror the shell's behaviour for a missing program.
		fmt.Fprintf(stdout, "%s: command not found\n", argv[0])
		return 127
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return execDirect(argv, extraEnv, stdin, stdout, stderr)
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = stdin
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		fmt.Fprintf(stdout, "%s: %v\n", argv[0], err)
		return 126
	}
	pw.Close()

	// Forward termination signals to the program; its output so far is then
	// printed unchanged.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	var gotSignal atomic.Bool
	go func() {
		for s := range sigs {
			gotSignal.Store(true)
			_ = cmd.Process.Signal(s)
		}
	}()

	// Capture up to max_input_bytes; beyond that, stream everything through.
	var buf bytes.Buffer
	limit := int64(cfg.MaxInputBytes)
	n, rerr := io.Copy(&buf, io.LimitReader(pr, limit+1))
	streamed := false
	if n > limit && rerr == nil {
		streamed = true
		stdout.Write(buf.Bytes())
		io.Copy(stdout, pr)
	}
	pr.Close()
	werr := cmd.Wait()
	code := exitCode(cmd, werr)
	// Statistics measure ContextSlim's own overhead, not the program's run.
	start = time.Now()

	st := store.New(dir)
	ev := metrics.Event{Mode: cfg.Mode, Via: "wrapper", BytesIn: buf.Len()}
	ev.ExitCode = &code
	record := func() {
		ev.DurationUS = time.Since(start).Microseconds()
		_ = metrics.Append(st.MetricsDir(), session, ev)
	}
	switch {
	case streamed:
		ev.Action, ev.Reason = metrics.ActionSkipped, pipeline.ReasonTooLarge
		record()
		return code
	case gotSignal.Load():
		stdout.Write(buf.Bytes())
		ev.Action, ev.Reason = metrics.ActionSkipped, pipeline.ReasonInterrupted
		record()
		return code
	}

	exe := "contextslim"
	if e, err := executable(); err == nil {
		exe = claude.QuoteArg(e)
	}
	env := pipeline.Env{Config: cfg, Store: st, ShowCmd: exe, Now: time.Now}
	done := make(chan *pipeline.Outcome, 1)
	go func() { done <- pipeline.CompressOutput(env, session, original, buf.String()) }()
	var o *pipeline.Outcome
	select {
	case o = <-done:
	case <-time.After(time.Duration(cfg.ProcessingBudgetMS) * time.Millisecond):
		o = &pipeline.Outcome{}
		o.Event = metrics.Event{Action: metrics.ActionSkipped, Reason: pipeline.ReasonBudget}
	}
	if o.Event.Action == metrics.ActionCompressed && o.Text != "" {
		io.WriteString(stdout, o.Text)
	} else {
		stdout.Write(buf.Bytes())
	}
	o.Event.Mode, o.Event.Via, o.Event.ExitCode = cfg.Mode, "wrapper", &code
	if o.Event.BytesIn == 0 {
		o.Event.BytesIn = buf.Len()
	}
	ev = o.Event
	record()
	return code
}

// execDirect runs the program with inherited output (no capture).
func execDirect(argv, extraEnv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(stdout, "%s: command not found\n", argv[0])
		return 127
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stdout, "%s: %v\n", argv[0], err)
		return 126
	}
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	return exitCode(cmd, cmd.Wait())
}

// exitCode returns the program's exit status the way a POSIX shell reports
// it: the exit code, or 128+signal when it was killed by a signal.
func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState == nil {
		return 1
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	var ee *exec.ExitError
	if err == nil || errors.As(err, &ee) {
		return cmd.ProcessState.ExitCode()
	}
	return 1
}
