package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/yotatameguchi-boop/contextslim/internal/claude"
	"github.com/yotatameguchi-boop/contextslim/internal/config"
	"github.com/yotatameguchi-boop/contextslim/internal/store"
	"github.com/yotatameguchi-boop/contextslim/internal/version"
)

// VerifiedClaudeVersions are Claude Code versions on which the Phase 0
// connection checks (output replacement, pass-through, re-fetch) passed.
var VerifiedClaudeVersions = map[string]bool{"2.1.322": true}

var semverRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

type doctor struct {
	w           io.Writer
	warns, errs int
}

func (d *doctor) ok(f string, a ...any)   { fmt.Fprintf(d.w, "  [OK]   "+f+"\n", a...) }
func (d *doctor) warn(f string, a ...any) { d.warns++; fmt.Fprintf(d.w, "  [WARN] "+f+"\n", a...) }
func (d *doctor) fail(f string, a ...any) { d.errs++; fmt.Fprintf(d.w, "  [NG]   "+f+"\n", a...) }
func (d *doctor) info(f string, a ...any) { fmt.Fprintf(d.w, "         "+f+"\n", a...) }

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("doctor", flag.ContinueOnError)
	project := fset.String("project", "", "対象プロジェクト（既定: カレント）")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	d := &doctor{w: stdout}
	fmt.Fprintf(stdout, "contextslim %s (%s)\n\n", version.Version, version.Compressor)

	fmt.Fprintln(stdout, "Claude Code")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		d.warn("claude コマンドを実行できません（PATH 未設定の可能性）: %v", err)
	} else {
		v := semverRE.FindString(string(out))
		switch {
		case v == "":
			d.warn("バージョンを読み取れません: %q", string(out))
		case VerifiedClaudeVersions[v]:
			d.ok("Claude Code %s（接続検証済み: updatedToolOutput による出力置換）", v)
		case olderThan(v, "2.1.322"):
			d.warn("Claude Code %s は未検証です。PostToolUse の updatedToolOutput に対応していない可能性があります（検証済み: 2.1.322）", v)
		default:
			d.warn("Claude Code %s は互換性未確認です（検証済み: 2.1.322）。observe モードで動作を確認してから compress を有効にしてください", v)
		}
	}

	fmt.Fprintln(stdout, "\nフック設定")
	paths := settingsFiles(*project)
	installed := 0
	for _, p := range paths {
		refs, err := claude.ScanPostToolUse(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			d.fail("%s を読めません: %v", p, err)
			continue
		}
		for _, r := range refs {
			if r.Owned {
				installed++
				d.ok("%s: 登録済み（matcher=%q）", p, r.Matcher)
				if exe := ownedExe(r.Command); exe != "" {
					if _, err := os.Stat(exe); err != nil {
						d.fail("登録されている実行ファイルが見つかりません: %s（contextslim install を再実行してください）", exe)
					}
				}
			} else {
				d.warn("%s: 他の PostToolUse フックが Bash に一致します（matcher=%q, command=%q）。"+
					"出力を書き換えるフック同士は元の出力に対して並列に動き、後勝ちで競合します", p, r.Matcher, r.Command)
			}
		}
	}
	failureHook := false
	for _, p := range paths {
		if refs, err := claude.ScanEvent(p, "PostToolUseFailure"); err == nil {
			for _, r := range refs {
				failureHook = failureHook || r.Owned
			}
		}
	}
	if installed > 0 && !failureHook {
		d.warn("PostToolUseFailure の観測フックがありません（失敗終了時の削減候補が統計に出ません）。contextslim install を再実行してください")
	}
	d.info("制約: 終了コードが0以外のコマンド（失敗したテスト、エラーのある tsc / ruff）は Claude Code が PostToolUseFailure を発火し、")
	d.info("      出力の置換を受け付けません（2.1.322 で確認）。ContextSlim はその場合も原文を通し、削減候補を統計に記録するだけです。")
	if installed == 0 {
		d.warn("ContextSlim のフックが登録されていません（contextslim install）")
	} else if installed > 1 {
		d.warn("ContextSlim のフックが %d 箇所に登録されています。同じ出力が複数回処理されます", installed)
	}

	fmt.Fprintln(stdout, "\n設定と保存先")
	dir, err := dataDir()
	if err != nil {
		d.fail("データ保存先を決定できません: %v", err)
	} else {
		cfg, err := config.Load(dir)
		if err != nil {
			d.fail("設定ファイルが不正です（フックは原文を通します）: %v", err)
		} else {
			d.ok("モード: %s%s", cfg.Mode, map[bool]string{true: "（出力は変更しません）", false: ""}[cfg.Mode == config.ModeObserve])
			if cfg.ProcessingBudgetMS >= claude.HookTimeoutSeconds*1000 {
				d.warn("processing_budget_ms がフックのタイムアウト（%ds）以上です", claude.HookTimeoutSeconds)
			}
		}
		if err := checkWritable(dir); err != nil {
			d.fail("保存先 %s に書き込めません（圧縮は行われません）: %v", dir, err)
		} else {
			d.ok("保存先: %s", dir)
		}
		if u, err := store.New(dir).Usage(); err == nil {
			d.info("使用量: %s / 上限 %s", human(u), human(cfg0(dir).MaxStorageBytes))
		}
	}

	fmt.Fprintln(stdout, "\n対応形式（検証済みのバージョン）")
	d.info("vitest  3.x（default / verbose レポーター）")
	d.info("jest    29.x（default レポーター、--verbose 可）")
	d.info("tsc     5.x（plain / --pretty）")
	d.info("pytest  8.x, 9.x（既定 / -q / -v）")
	d.info("ruff    0.17（ruff check: full / concise）")

	fmt.Fprintf(stdout, "\n結果: 警告 %d 件、エラー %d 件\n", d.warns, d.errs)
	if d.errs > 0 {
		return 1
	}
	return 0
}

func cfg0(dir string) config.Config { c, _ := config.Load(dir); return c }

func settingsFiles(project string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".claude", "settings.json"))
	}
	if project == "" {
		project, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(project); err == nil {
		out = append(out, filepath.Join(abs, ".claude", "settings.json"), filepath.Join(abs, ".claude", "settings.local.json"))
	}
	return out
}

var exeRE = regexp.MustCompile(`^\s*(?:"([^"]+)"|'([^']+)'|(\S+))\s+hook`)

func ownedExe(cmd string) string {
	m := exeRE.FindStringSubmatch(cmd)
	if m == nil {
		return ""
	}
	for _, s := range m[1:] {
		if s != "" {
			if filepath.IsAbs(s) {
				return s
			}
			return ""
		}
	}
	return ""
}

func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func olderThan(v, ref string) bool {
	a, b := semverRE.FindStringSubmatch(v), semverRE.FindStringSubmatch(ref)
	if a == nil || b == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(a[i])
		y, _ := strconv.Atoi(b[i])
		if x != y {
			return x < y
		}
	}
	return false
}
