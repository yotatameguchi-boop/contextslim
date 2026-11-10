# ContextSlim

Claude Code の Bash ツールが返すテスト結果・型チェック・Lint の出力を、**失敗情報を落とさずに**短くするローカル CLI です。
Claude Code のフック（PreToolUse・PostToolUse）として動きます。外部 API は呼ばず、アカウント登録や常駐プロセスも不要です。

> **失敗したテストの出力も圧縮します（ラッパー方式、既定）**
> Claude Code 2.1.322 では、終了コード≠0 のコマンドの出力を PostToolUse で置換できません（[docs/phase0.md](docs/phase0.md)）。
> そこで ContextSlim は、対応コマンドを PreToolUse フックで `contextslim run -- <元のコマンド>` に差し替え、
> ラッパーが実行・原文保存・圧縮を行って**元と同じ終了コード**で終了します。失敗を含むテスト結果やエラーのある tsc / ruff も圧縮されます。
> ラップするのは、元のコマンドを Claude Code が確認なしで実行する場合だけです（下の「ラッパー方式と権限」）。

入力トークンの削減率は、サブスクリプション利用枠の改善率とは一致しません。利用可能時間の延長や上限到達の回避は保証しません。

## 対応形式

| ツール | 検証済みの版 | 出力形式 | 圧縮内容 |
| --- | --- | --- | --- |
| Vitest | 3.x（3.2.7） | default / verbose レポーター | 成功テスト行・成功ファイル行を件数に集約 |
| Jest | 29.x（29.7.0） | default レポーター（`--verbose` 可） | `PASS` 行・成功テスト行・全件成功スイートの describe 見出しを集約 |
| TypeScript (tsc) | 5.x（5.9.3） | plain / `--pretty` | ANSI カラーの除去、完全に同一の診断の集約 |
| pytest | 8.x, 9.x | 既定 / `-q` / `-v`（xdist の `[gwN]` 行を含む） | `.` だけの進捗行・`PASSED` 行を集約 |
| Ruff | 0.17 | `ruff check`（full / concise） | 完全に同一の診断の集約 |

圧縮のルール：

- 失敗ブロック（期待値・実際の値・差分・スタックトレース・コード断片）、診断、スキップ、警告、テスト中の出力、ツール自身の要約行は**原文のまま**残します。
- 診断を集約するのは、ファイル・位置・コード・メッセージ・文脈がすべて一致する場合だけです。場所が違えば統合しません。
- 文字数や行数で末尾を切り捨てることはしません。
- 圧縮結果の先頭に、何を除去したかと原文の取得方法を書いた 2 行を付けます。

```text
[contextslim] pytest output compressed. Removed: 151 passing-test lines. Failures, errors, diagnostics and the tool's own summary are verbatim below.
[contextslim] Full original (209 lines): /usr/local/bin/contextslim show cs_5c44b70d5e767a6e140b   (range: --from N --to M)
```

次の場合は**原文をそのまま通します**：未対応のコマンドや形式、パイプ（`|`）・`&&`・`;`・リダイレクト・`$VAR`・`$(...)` を含むコマンド、出力形式を変えるオプション（`--reporter=json`、`--watch`、`--output-format json` など）、未検証のツール版、要約行がない（中断・実行途中の）出力、解析器が認識できずエラーや失敗に言及している行、情報保持の検証に失敗した場合、削減量が足りない場合、原文の保存に失敗した場合、処理時間の予算を超えた場合、内部エラー。

## インストール

Go をインストールしなくても動く単一バイナリです。

```bash
scripts/build-release.sh          # dist/ に OS・CPU 別のバイナリを生成
```

（開発時は `go build -o contextslim ./cmd/contextslim` でも構いません。）

PATH の通った場所にバイナリを置き、対象プロジェクトで次を実行します。

```bash
contextslim install
```

`.claude/settings.local.json` に `PreToolUse`・`PostToolUse`・`PostToolUseFailure` のフックを登録します。チームで共有する `.claude/settings.json` に登録するときは `--shared`、全プロジェクトに登録するときは `--user` を付けます。

- 既存の設定はキーの順序も含めて保持し、ContextSlim のフックだけを追加・更新します（インデントは 2 スペースに揃えます）。
- 変更前に `<設定ファイル>.contextslim-backup-<日時>` を作成します。
- 繰り返し実行しても重複しません。
- 解除は `contextslim uninstall` です。ContextSlim が登録したフックだけを削除します。

初期状態は **observe モード**です。出力は変更せず、圧縮候補と統計だけを記録します。統計を確認してから有効化してください。

```bash
contextslim stats
contextslim mode compress
```

## ラッパー方式と権限

`method` が `wrap`（既定）で compress モードのとき、次の流れで動きます。

```text
Claude が Bash で「npx vitest run」を実行しようとする
  → PreToolUse フック: 条件を満たせば
       "<contextslim> run --session <key> --cmd <base64> -- npx vitest run" に差し替え、許可を返す
  → contextslim run: npx vitest run を同じ引数・環境変数・作業ディレクトリ・標準入力で実行
       stdout/stderr をまとめて受け取り、原文を保存して圧縮（できなければ原文をそのまま）表示
       vitest の終了コードで終了（シグナルで終了した場合は 128+N）
  → Claude には「Exit code 1」と圧縮結果が届く
```

**権限について（重要）**：実機で確認したところ、Claude Code は差し替え**後**のコマンドに対して権限を判定します。元のコマンドへの許可ルールは効かず、**拒否（deny）・確認（ask）ルールもすり抜けます**。
そのため ContextSlim は、次の**すべて**を満たすときだけラップし、元のコマンドに対して Claude Code が出すはずの判定（許可）を返します。

1. どの設定ファイル（managed / ユーザー / プロジェクト / ローカル）の deny・ask ルールにも、元のコマンドが一致し得ない（判定は広めにとり、読めないルールは「一致する」とみなす）
2. 元のコマンドが allow ルールに確実に一致する、またはセッションが bypassPermissions モード（plan モードではラップしない）
3. 起動元の `claude` に、ContextSlim から見えないルールを足すオプション（`--disallowedTools`、`--settings`、`--setting-sources`、`--permission-prompt-tool`）がない。`--allowedTools` の許可ルールは読み取って使う（Linux のみ）
4. 起動元の `claude` プロセスを確認できる（Linux・macOS。**Windows ではラップしない**）

どれか一つでも満たさないときはラップせず、コマンドは ContextSlim がない場合とまったく同じように扱われます（終了コード0なら従来どおり PostToolUse で圧縮）。ラップしなかった理由は `contextslim stats` に出ます。

**ラッパー方式の注意点**

- コマンドの出力は終了後にまとめて表示されます（実行中の逐次表示はありません）。
- Claude Code のタイムアウトなどで SIGTERM / SIGINT を受けた場合は、コマンドに転送し、それまでの出力を原文のまま表示します。SIGKILL で強制終了された場合はその出力が失われます。
- `run_in_background` で実行されるコマンドはラップしません。
- 出力が `max_input_bytes` を超えた場合は、その時点から原文をそのまま流します。
- ラップしたくない場合は `config.json` で `"method": "post_tool_use"` にします（終了コード0の出力だけを圧縮）。

## コマンド

| コマンド | 内容 |
| --- | --- |
| `contextslim install [--project DIR] [--shared\|--user]` | フックを登録 |
| `contextslim uninstall [...]` | 自身のフックだけを解除 |
| `contextslim doctor` | Claude Code の版、フック設定、他フックとの競合、設定、保存先を診断 |
| `contextslim mode [observe\|compress]` | モードの表示・切り替え |
| `contextslim show <raw-id> [--from N] [--to M] [--field stdout\|stderr]` | 原文の全体または指定行を表示（この出力は再圧縮しません） |
| `contextslim stats [--session KEY] [--json]` | 統計を表示 |
| `contextslim prune [--dry-run]` | 保存期限と容量上限に基づいて整理 |
| `contextslim config` | 設定ファイルの場所と内容を表示 |
| `contextslim hook` | フック本体（Claude Code から呼ばれる） |
| `contextslim run --session KEY --cmd B64 -- CMD...` | ラッパー本体（PreToolUse フックが差し替えるコマンド） |

## 設定

`contextslim config` で場所を確認できる `config.json` を編集します。書かなかった項目には初期値を使い、未知の項目や不正な値はエラーになります（その間、フックは原文を通します）。

| 項目 | 初期値 | 内容 |
| --- | --- | --- |
| `mode` | `observe` | `observe` / `compress` |
| `method` | `wrap` | `wrap`：ラッパー方式（失敗時も圧縮）／ `post_tool_use`：終了コード0の出力のみ |
| `enabled_parsers` | 全5種 | 有効な解析器 |
| `min_input_bytes` | 4096 | これより小さい出力は圧縮しない |
| `min_savings_bytes` | 1024 | 最小削減バイト数（案内2行を含めて計算） |
| `min_savings_ratio` | 0.10 | 最小削減率 |
| `max_input_bytes` | 10485760 | これより大きい出力は圧縮しない |
| `processing_budget_ms` | 200 | 処理時間の予算（フックの timeout 10秒より十分短い） |
| `retention_days` | 7 | 原文の保存日数 |
| `metrics_retention_days` | 30 | 統計の保存日数 |
| `max_storage_bytes` | 524288000 | 原文と統計の容量上限 |
| `token_estimator` | `none` | `none`：バイト数のみ表示。`bytes_div_4`：未検証の概算（Claude のトークン数ではないと明示して表示） |

## 保存データ

保存先は OS 標準のユーザーデータ領域です（環境変数 `CONTEXTSLIM_HOME` で変更できます）。

- Linux：`$XDG_DATA_HOME/contextslim`（未設定なら `~/.local/share/contextslim`）
- macOS：`~/Library/Application Support/ContextSlim`
- Windows：`%LOCALAPPDATA%\ContextSlim`

```text
config.json
raw/<session-key>/<raw-id>.json   原文、状態、日時、コマンド、解析器、圧縮器の版、SHA-256、保存期限
raw/<session-key>/<raw-id>.gone   削除済みの記録（show で「削除済み」と明示するため）
metrics/<session-key>.jsonl       セッションごとのイベント
```

- 原文は**出力を置き換える前に**保存します。一時ファイルに書いてから rename で確定し、保存できなければ置換しません。
- ディレクトリは 0700、ファイルは 0600 で作成します（Windows を除く）。外部への送信やアップロードは一切しません。
- `raw-id` は 80 ビットのランダム値で、`O_EXCL` で衝突を防ぎます。セッション ID はハッシュ化してからディレクトリ名に使い、入力をパスとして直接使うことはありません。
- 読み込み時に SHA-256 を照合します。一致しない原文や、削除済みの原文の代わりに別の原文を返すことはありません。
- 整理では ContextSlim 自身のファイル（名前の形式で判定）だけを削除し、シンボリックリンクはたどりません。作成から10分以内の原文は容量整理の対象外です。容量を確保できない場合は圧縮を見送ります。

## 統計

`contextslim stats` は次を表示します。

- フック実行回数、置換・観測・通過の件数と、通過した理由の内訳
- 圧縮前後のバイト数（**実測値**）。置換したもの、観測のみのもの、そのうち失敗終了で置換できなかったものを分けて表示
- 推定トークン数（`token_estimator` を選んだ場合だけ。方式名を付け、推定値と明記）
- 処理時間の p50 / p95 / 最大
- 原文の再取得回数
- 解析器ごとの件数、ツールの版、解析器の版

ログ単位の削減量を、利用枠の節約率や請求額として表示することはありません。

## 性能

基準環境：Linux x86_64、Xeon 2.3GHz × 4 コア。`contextslim hook` をプロセスとして 60 回起動し、プロセス起動・解析・検証・原文保存・出力までを計測しました。

| 入力 | p50 | p95 |
| --- | --- | --- |
| 1 MiB の `pytest -v` ログ | 73.7 ms | 93.6 ms |
| 14 KiB の `pytest -v` ログ | 4.4 ms | 5.6 ms |
| 対象外のコマンド | 2.3 ms | 2.6 ms |

暫定目標（1 MiB 以下で p95 100 ms 以内）を満たしていますが、1 MiB では余裕が小さいため、他の環境でも計測してから見直してください。

## 削減効果（検証用ログ）

`testdata/logs` の実ログ（案内 2 行を含む）。

| ログ | 圧縮前 | 圧縮後 | 削減 |
| --- | --- | --- | --- |
| pytest -v（156件、失敗3） | 14,688 B | 2,867 B | 80% |
| Jest --verbose（404件、失敗2） | 14,640 B | 3,259 B | 78% |
| Vitest verbose（101件、失敗3） | 7,636 B | 2,857 B | 63% |
| pytest 既定（1,006件、失敗3） | 5,272 B | 2,334 B | 56% |
| Vitest 既定（405件、失敗3） | 4,165 B | 2,833 B | 32% |
| tsc --pretty（7件） | 2,213 B | 1,812 B | 18% |
| 実機（Claude Code）：Vitest verbose 全件成功 110件 | 6.4 KiB | 580 B | 91% |

ruff と tsc の plain 形式は、診断がすべて異なる限りほとんど削減できません（最小削減量の条件により原文を通します）。これは、異なる診断を統合しないという方針どおりの結果です。

失敗を含むログは、ラッパー方式（既定）で圧縮されます。`method: post_tool_use` では観測のみになります。

実機（Claude Code 2.1.322）では、失敗を含む Vitest 405件（`npx vitest run`、終了コード1）がラッパー経由で 4.1 KiB → 2.9 KiB になり、失敗3件の内容・位置・要約がそのまま Claude に届きました。

## 開発

```bash
go test ./...                                   # 単体・結合・ゴールデンテスト
go test -race ./...
go test ./internal/compress -update             # ゴールデンの再生成（差分をレビューする）
go test ./internal/pipeline -run '^$' -fuzz FuzzProcess -fuzztime 60s
go test ./internal/pipeline -run '^$' -bench .  # 1 MiB ログのベンチマーク
```

| パッケージ | 役割 |
| --- | --- |
| `cmd/contextslim` | エントリーポイント |
| `internal/cli` | サブコマンド、doctor |
| `internal/claude` | フックの入出力、設定ファイルの登録・解除（キー順を保持） |
| `internal/permissions` | ラップしてよいかの判定（設定ファイルの権限ルール、起動元 claude のオプション） |
| `internal/pipeline` | フック処理の流れ（設計書 6章） |
| `internal/detect` | コマンドの字句解析と対象判定（実行・展開はしない） |
| `internal/parser` | vitest / jest / tsc / pytest / ruff の解析器 |
| `internal/compress` | 集約・整形 |
| `internal/validate` | 情報保持の検証（必須行、要約の件数、失敗に言及する行が除去されていないこと） |
| `internal/store` | 原文の保存・取得・期限管理 |
| `internal/metrics` | イベントの記録と集計 |
| `internal/config` | 設定 |
| `internal/textutil` | ANSI・CRLF・バイナリの判定と正規化 |

## 未確定事項（要件定義書 9章）の現状

| 項目 | 現状 |
| --- | --- |
| 正式な製品名 | 未定（仮称 ContextSlim） |
| Claude Code の最低対応版 | 2.1.322 で検証済み。それ以外は doctor が「未確認」と表示 |
| 各ログ形式の対応版 | 上の「対応形式」のとおり |
| 推定トークン数の算出方式 | 未選定（既定 `none`） |
| 性能目標・最小削減量 | 上の計測値。最小削減量は初期値のまま |
| 保存期間・容量上限 | 初期値（7日、500 MiB） |
| 失敗時の出力の扱い | ラッパー方式で対応（Linux・macOS）。Windows は終了コード0の出力のみ |

評価方法 7.2（同じ課題で圧縮なし／ありを複数回比較する評価）は、まだ実施していません。
