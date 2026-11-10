// Package cli implements the contextslim command line.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yotatameguchi-boop/contextslim/internal/claude"
	"github.com/yotatameguchi-boop/contextslim/internal/config"
	"github.com/yotatameguchi-boop/contextslim/internal/metrics"
	"github.com/yotatameguchi-boop/contextslim/internal/pipeline"
	"github.com/yotatameguchi-boop/contextslim/internal/store"
	"github.com/yotatameguchi-boop/contextslim/internal/textutil"
	"github.com/yotatameguchi-boop/contextslim/internal/version"
)

const usage = `ContextSlim — Claude Code のテスト／診断ログを情報を落とさずに圧縮するフック

使い方:
  contextslim install [--project DIR] [--shared | --user]   フックを登録（既定: DIR/.claude/settings.local.json）
  contextslim uninstall [--project DIR] [--shared | --user] 自身が登録したフックだけを解除
  contextslim doctor [--project DIR]                        バージョン・設定・保存先・互換性を診断
  contextslim mode [observe|compress]                       モードの表示／切り替え（初期値 observe）
  contextslim show <raw-id> [--from N] [--to M] [--field stdout|stderr]
                                                            保存した原文を表示（行番号は1始まり）
  contextslim stats [--session KEY] [--json]                ローカル統計を表示
  contextslim prune [--dry-run]                             期限・容量に基づき保存データを整理
  contextslim config                                        設定ファイルの場所と内容を表示
  contextslim hook                                          （Claude Code から呼ばれる）フック本体
  contextslim version

データ保存先は CONTEXTSLIM_HOME で変更できます。
`

// Main runs the CLI and returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "hook":
		return runHook(stdin, stdout, stderr)
	case "install":
		err = runInstall(rest, stdout, false)
	case "uninstall":
		err = runInstall(rest, stdout, true)
	case "doctor":
		return runDoctor(rest, stdout, stderr)
	case "mode":
		err = runMode(rest, stdout)
	case "show":
		err = runShow(rest, stdout)
	case "stats":
		err = runStats(rest, stdout)
	case "prune":
		err = runPrune(rest, stdout)
	case "config":
		err = runConfig(stdout)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "contextslim %s (%s)\n", version.Version, version.Compressor)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "不明なサブコマンド: %s\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "contextslim %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func dataDir() (string, error) { return store.DataDir() }

// executable returns the absolute path of the running binary.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Abs(exe)
}

// runHook is the PostToolUse entry point. It must never disturb the user's
// work: every failure exits 0 with no output, which makes Claude Code use the
// original tool output.
func runHook(stdin io.Reader, stdout, stderr io.Writer) (code int) {
	debug := os.Getenv("CONTEXTSLIM_DEBUG") != ""
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			if debug {
				fmt.Fprintf(stderr, "contextslim: internal error: %v\n", r)
			}
			code = 0
		}
	}()
	dir, err := dataDir()
	if err != nil {
		return 0
	}
	cfg, cfgErr := config.Load(dir)
	// Read at most max_input_bytes plus the JSON envelope; larger inputs are
	// passed through without being buffered completely.
	limit := int64(cfg.MaxInputBytes)*2 + 1<<20
	input, err := io.ReadAll(io.LimitReader(stdin, limit+1))
	if err != nil {
		return 0
	}
	st := store.New(dir)
	showCmd := "contextslim"
	if exe, err := executable(); err == nil {
		showCmd = claude.QuoteArg(exe)
	}
	env := pipeline.Env{Config: cfg, Store: st, ShowCmd: showCmd, Now: time.Now}

	var outcome *pipeline.Outcome
	switch {
	case cfgErr != nil:
		outcome = &pipeline.Outcome{SessionKey: sessionKeyFrom(input)}
		outcome.Event.Action, outcome.Event.Reason = metrics.ActionSkipped, pipeline.ReasonConfig
	case int64(len(input)) > limit:
		outcome = &pipeline.Outcome{SessionKey: sessionKeyFrom(input)}
		outcome.Event.Action, outcome.Event.Reason = metrics.ActionSkipped, pipeline.ReasonTooLarge
	default:
		budget := time.Duration(cfg.ProcessingBudgetMS) * time.Millisecond
		done := make(chan *pipeline.Outcome, 1)
		go func() { done <- pipeline.Process(env, input) }()
		select {
		case outcome = <-done:
		case <-time.After(budget - time.Since(start)):
			outcome = &pipeline.Outcome{SessionKey: sessionKeyFrom(input)}
			outcome.Event.Action, outcome.Event.Reason = metrics.ActionSkipped, pipeline.ReasonBudget
		}
	}
	outcome.Event.Mode = cfg.Mode
	outcome.Event.DurationUS = time.Since(start).Microseconds()
	if outcome.Output != nil {
		if _, err := stdout.Write(outcome.Output); err != nil {
			return 0
		}
	}
	// Statistics failures never affect the result.
	_ = metrics.Append(st.MetricsDir(), outcome.SessionKey, outcome.Event)
	if debug {
		fmt.Fprintf(stderr, "contextslim: %s\n", pipeline.Describe(outcome))
	}
	return 0
}

func sessionKeyFrom(input []byte) string {
	var v struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(input, &v)
	return store.SessionKey(v.SessionID)
}

func settingsPath(project string, shared, user bool) (string, error) {
	if shared && user {
		return "", errors.New("--shared と --user は同時に指定できません")
	}
	if user {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	}
	if project == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		project = wd
	}
	project, err := filepath.Abs(project)
	if err != nil {
		return "", err
	}
	name := "settings.local.json"
	if shared {
		name = "settings.json"
	}
	return filepath.Join(project, ".claude", name), nil
}

func runInstall(args []string, stdout io.Writer, remove bool) error {
	name := "install"
	if remove {
		name = "uninstall"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	project := fs.String("project", "", "対象プロジェクトのディレクトリ（既定: カレント）")
	shared := fs.Bool("shared", false, ".claude/settings.json（チーム共有）を対象にする")
	user := fs.Bool("user", false, "~/.claude/settings.json（全プロジェクト）を対象にする")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := settingsPath(*project, *shared, *user)
	if err != nil {
		return err
	}
	var ch claude.Change
	if remove {
		ch, err = claude.Uninstall(path, time.Now())
	} else {
		exe, e := executable()
		if e != nil {
			return e
		}
		ch, err = claude.Install(path, exe, time.Now())
	}
	if err != nil {
		return err
	}
	switch {
	case ch.Changed:
		fmt.Fprintf(stdout, "%s: 更新しました\n", ch.Path)
		if ch.Backup != "" {
			fmt.Fprintf(stdout, "  バックアップ: %s\n", ch.Backup)
		}
	default:
		fmt.Fprintf(stdout, "%s: 変更なし（%s）\n", ch.Path, ch.Message)
	}
	if !remove {
		dir, _ := dataDir()
		cfg, _ := config.Load(dir)
		fmt.Fprintf(stdout, "現在のモード: %s", cfg.Mode)
		if cfg.Mode == config.ModeObserve {
			fmt.Fprint(stdout, "（出力は変更せず統計のみ記録します。有効化: contextslim mode compress）")
		}
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Claude Code で /hooks を開くか、新しいセッションを開始すると反映されます。")
	}
	return nil
}

func runMode(args []string, stdout io.Writer) error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(stdout, cfg.Mode)
		return nil
	}
	if len(args) != 1 || (args[0] != config.ModeObserve && args[0] != config.ModeCompress) {
		return errors.New("使い方: contextslim mode [observe|compress]")
	}
	cfg.Mode = args[0]
	if err := config.Save(dir, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "モードを %s に設定しました（%s）\n", cfg.Mode, config.Path(dir))
	return nil
}

func runShow(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	from := fs.Int("from", 0, "開始行（1始まり）")
	to := fs.Int("to", 0, "終了行（この行を含む）")
	field := fs.String("field", "", "stdout または stderr（既定: 圧縮対象だったフィールド）")
	// Accept the id before or after flags.
	var id string
	var flagArgs []string
	for _, a := range args {
		if store.ValidID(a) && id == "" {
			id = a
			continue
		}
		flagArgs = append(flagArgs, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if id == "" && fs.NArg() > 0 {
		id = fs.Arg(0)
	}
	if id == "" {
		return errors.New("使い方: contextslim show <raw-id> [--from N] [--to M]")
	}
	dir, err := dataDir()
	if err != nil {
		return err
	}
	st := store.New(dir)
	rec, err := st.Load(id)
	switch {
	case errors.Is(err, store.ErrInvalidID):
		return fmt.Errorf("%q は原文IDの形式ではありません", id)
	case errors.Is(err, store.ErrDeleted):
		return fmt.Errorf("原文 %s は保存期限切れまたは容量整理で削除済みです: %v", id, err)
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("原文 %s は見つかりません（削除済み、または別のデータ保存先）", id)
	case err != nil:
		return err
	}
	resp, err := claude.ParseBashResponse(rec.ToolResponse)
	if err != nil {
		return err
	}
	f := *field
	if f == "" {
		f = rec.Field
	}
	var text string
	switch f {
	case "stdout":
		text = resp.Stdout
	case "stderr":
		text = resp.Stderr
	default:
		return fmt.Errorf("--field は stdout か stderr です")
	}
	if key := st.SessionKeyOf(id); key != "" {
		_ = metrics.Append(st.MetricsDir(), key, metrics.Event{Action: metrics.ActionShow, RawID: id})
	}
	if *from == 0 && *to == 0 {
		_, err := io.WriteString(stdout, text)
		if err == nil && text != "" && !strings.HasSuffix(text, "\n") {
			_, err = io.WriteString(stdout, "\n")
		}
		if err == nil && *field == "" && f == "stdout" && resp.Stderr != "" {
			_, err = fmt.Fprintf(stdout, "--- stderr ---\n%s\n", strings.TrimSuffix(resp.Stderr, "\n"))
		}
		return err
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	total := textutil.CountLines(text)
	start, end := *from, *to
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > total {
		end = total
	}
	if start > total || start > end {
		return fmt.Errorf("範囲外です（全 %d 行）", total)
	}
	for i := start; i <= end; i++ {
		if _, err := fmt.Fprintln(stdout, lines[i-1]); err != nil {
			return err
		}
	}
	return nil
}

func runStats(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	session := fs.String("session", "", "セッションキー（16桁の16進）で絞り込む")
	asJSON := fs.Bool("json", false, "JSONで出力")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := dataDir()
	if err != nil {
		return err
	}
	cfg, _ := config.Load(dir)
	st := store.New(dir)
	events, sessions, err := metrics.ReadAll(st.MetricsDir(), *session)
	if err != nil {
		return err
	}
	s := metrics.Summarize(events, sessions)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	w := stdout
	fmt.Fprintf(w, "ContextSlim 統計（%d セッション、現在のモード: %s）\n\n", s.Sessions, cfg.Mode)
	fmt.Fprintf(w, "フック実行          %d 回\n", s.HookRuns)
	fmt.Fprintf(w, "  圧縮して置換      %d\n", s.Compressed)
	fmt.Fprintf(w, "  観測のみ（候補）  %d\n", s.Observed)
	fmt.Fprintf(w, "  原文のまま通過    %d\n", s.Skipped)
	if len(s.SkipReasons) > 0 {
		var keys []string
		for k := range s.SkipReasons {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return s.SkipReasons[keys[i]] > s.SkipReasons[keys[j]] || s.SkipReasons[keys[i]] == s.SkipReasons[keys[j]] && keys[i] < keys[j]
		})
		for _, k := range keys {
			fmt.Fprintf(w, "    %-30s %d\n", k, s.SkipReasons[k])
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "バイト数（実測値）")
	fmt.Fprintf(w, "  置換した出力      %s\n", bytesLine(s.CompressedIn, s.CompressedOut))
	fmt.Fprintf(w, "  観測のみの候補    %s（Claude に渡した出力は変更していません）\n", bytesLine(s.ObservedIn, s.ObservedOut))
	if s.ObservedFailure > 0 {
		fmt.Fprintf(w, "    うち失敗終了    %d 件 %s（終了コード≠0 のため Claude Code が置換を受け付けない出力）\n", s.ObservedFailure, bytesLine(s.ObservedFailureIn, s.ObservedFailureOut))
	}
	fmt.Fprintln(w)
	if len(s.Estimators) == 0 {
		fmt.Fprintln(w, "推定トークン数      表示しません（Claude 向けに妥当性を確認した推定方式が未選定: token_estimator=none）")
	} else {
		fmt.Fprintf(w, "推定トークン数（推定値・方式: %s・Claude のトークナイザーではありません）\n", strings.Join(s.Estimators, ","))
		fmt.Fprintf(w, "  置換した出力      %d → %d\n", s.TokensInEst, s.TokensOutEst)
	}
	fmt.Fprintln(w, "  ※ ログ単位の削減量は、サブスクリプション利用枠の節約率や請求額を意味しません。")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "処理時間（実測）    p50 %s / p95 %s / 最大 %s\n", us(s.DurationP50US), us(s.DurationP95US), us(s.DurationMaxUS))
	fmt.Fprintf(w, "原文の再取得        %d 回\n", s.Shows)
	if len(s.PerParser) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "解析器別")
		var names []string
		for k := range s.PerParser {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, n := range names {
			p := s.PerParser[n]
			fmt.Fprintf(w, "  %-7s 置換 %d / 観測 %d / 通過 %d   %s", n, p.Compressed, p.Observed, p.Skipped, bytesLine(p.BytesIn, p.BytesOut))
			if v := s.ToolVersions[n]; len(v) > 0 {
				fmt.Fprintf(w, "   ツール版: %s", strings.Join(v, ","))
			}
			fmt.Fprintln(w)
		}
		if len(s.ParserVersions) > 0 {
			fmt.Fprintf(w, "  解析器の版: %s\n", strings.Join(s.ParserVersions, ", "))
		}
	}
	return nil
}

func bytesLine(in, out int64) string {
	if in == 0 {
		return "0 B"
	}
	return fmt.Sprintf("%s → %s（−%.1f%%）", human(in), human(out), 100*float64(in-out)/float64(in))
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func us(v int64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1fms", float64(v)/1000)
	}
	return fmt.Sprintf("%dµs", v)
}

func runPrune(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "削除せずに対象を数える")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := dataDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	st := store.New(dir)
	r, err := st.Prune(store.PruneOptions{
		Retention:        time.Duration(cfg.RetentionDays) * 24 * time.Hour,
		MetricsRetention: time.Duration(cfg.MetricsRetentionDays) * 24 * time.Hour,
		MaxBytes:         cfg.MaxStorageBytes,
		DryRun:           *dry,
	})
	label := "削除"
	if *dry {
		label = "削除予定"
	}
	fmt.Fprintf(stdout, "%s: 原文 %d 件、統計 %d 件、一時ファイル %d 件（%s）\n使用量: %s / 上限 %s\n",
		label, r.RawDeleted, r.MetricsDeleted, r.TempDeleted, human(r.BytesFreed), human(r.UsageAfter), human(cfg.MaxStorageBytes))
	if errors.Is(err, store.ErrNoSpace) {
		fmt.Fprintf(stdout, "作成から %s 以内のファイルは保護されるため、上限を下回れませんでした。\n", store.ProtectAge)
		return nil
	}
	return err
}

func runConfig(stdout io.Writer) error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "データ保存先: %s\n設定ファイル: %s\n", dir, config.Path(dir))
	b, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Fprintf(stdout, "%s\n", b)
	return nil
}
