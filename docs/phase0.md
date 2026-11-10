# Phase 0：Claude Code 接続検証の結果

- 検証日：2026-11-10
- Claude Code：2.1.322（Linux x86_64）
- 方法：`claude -p` を実機で実行し、検証用フックの入出力とモデルが受け取った内容を確認した

## 結果一覧

| 確認項目（基本設計書 4.2） | 結果 |
| --- | --- |
| フック入力の実際の構造 | ✅ 下記「入力例」のとおり |
| Bash出力のスキーマ | ✅ `{"stdout","stderr","interrupted","isImage","noOutputExpected", ...}` |
| stdout/stderr の置換可否 | ✅ `hookSpecificOutput.updatedToolOutput` で置換できる（モデルが置換後の文字列のみを受け取ることを確認） |
| 状態フィールドの維持 | ✅ 置換オブジェクトは元の `tool_response` を基にし、`stdout` 以外のフィールドをバイト単位で保持する |
| 置換を返さない場合 | ✅ 原文がそのまま使われる |
| フックエラー時 | ✅ 不正なJSONを出力して exit 1 → 原文が使われる |
| フックのタイムアウト時 | ✅ timeout 超過 → 原文が使われる（遅れて返した置換は無視される） |
| 原文取得コマンドの利用 | ✅ Claude が圧縮結果の案内に従って `contextslim show <id> --from 3 --to 5` を実行し、原文の該当行を取得した |
| 複数フックの挙動 | ⚠️ Claude Code の説明文によれば、出力を書き換えるフックは**元の出力に対して並列に**実行され、**後勝ち**で反映される。`contextslim doctor` が Bash に一致する他の PostToolUse フックを警告する |
| **終了コード≠0 のコマンド** | ❌ **`PostToolUse` ではなく `PostToolUseFailure` が発火する。このイベントの出力は `additionalContext` のみで、置換（`updatedToolOutput`）は無視される** |

## 重要な制約：失敗したコマンドの出力は置換できない

```text
$ printf 'line1\nline2\n'; exit 3
→ hook_event_name: "PostToolUseFailure"
  error: "Exit code 3\nline1\nline2"
```

`PostToolUseFailure` で `updatedToolOutput` を返しても、モデルが受け取るのは原文のままだった（`additionalContext` は原文に**追加**されるだけで、削減にはならない）。

テストランナーは失敗時に、tsc と ruff は診断があると、いずれも終了コード≠0 を返す。
そのため、**設計どおりの接続方式（PostToolUse + updatedToolOutput）で実際に置換できるのは、終了コード0の出力に限られる**。

- 置換できる：全件成功したテスト（vitest / jest / pytest）、`ruff check --exit-zero`、診断が warning のみの場合など
- 置換できない：失敗を含むテスト結果、エラーのある tsc / ruff

なお、失敗時の出力は Claude Code 側でも長さに応じて切り詰められることがある（実機で `... [14462 characters truncated] ...` を確認）。

### 実装での扱い

- `PostToolUseFailure` にも観測専用のフックを登録する。出力は**一切変更しない**。
- 圧縮候補を作って `observed`（理由 `failure_event_not_replaceable`）として統計に記録し、`contextslim stats` の「うち失敗終了」に表示する。
- `contextslim doctor` はこの制約を常に表示する。

### 判断が必要な事項（設計書 14章 Phase 0「成立しない場合は接続方式を見直す」）

失敗時の出力も圧縮対象にするには、接続方式の変更が必要になる。候補は次のとおり。

1. **現状維持**：成功時のみ置換し、失敗時は観測のみ。設計に忠実だが、効果は成功時の出力に限られる。
2. **PreToolUse でのコマンドラッパー**：`updatedInput` で `npx vitest run` を `contextslim run -- npx vitest run` に書き換え、ラッパーがコマンドを実行・原文保存・圧縮して、同じ終了コードで終了する。失敗時も圧縮できる一方、次の点を新たに検証する必要がある：権限ルール（`Bash(npm test:*)` など）との関係、終了状態を維持できること、FR-03（判定のためにコマンドを実行しない）との整合、要件 6.4「既存権限・終了状態を変更しない」との整合。
3. **Claude Code 側の対応待ち**：`PostToolUseFailure` が出力の置換に対応した時点で、同じ解析器をそのまま使う。

観測モードの統計（「うち失敗終了」の候補バイト数）を、この判断の材料にできる。

## 入力例（PostToolUse, Bash）

```json
{
  "session_id": "6dcdb859-...",
  "transcript_path": "/root/.claude/projects/.../6dcdb859-....jsonl",
  "cwd": "/tmp/claude-0/p0",
  "permission_mode": "default",
  "hook_event_name": "PostToolUse",
  "tool_name": "Bash",
  "tool_input": {"command": "printf 'hello\\n'; echo err >&2", "description": "..."},
  "tool_response": {"stdout": "hello\nerr", "stderr": "", "interrupted": false, "isImage": false, "noOutputExpected": false},
  "tool_use_id": "toolu_01...",
  "duration_ms": 52
}
```

stderr は通常 stdout に統合されて届く（上の例では `err` が stdout に入っている）。

## 出力例（置換）

```json
{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":{"stdout":"...","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false}}}
```

## 実機での確認（ContextSlim 本体）

`contextslim install` → `contextslim mode compress` の後、Claude に `npx vitest run vbig/f1 --reporter=verbose`（110件すべて成功）を実行させた。

- Claude が受け取った出力：6.4 KiB → 580 B。成功テスト110行が除去され、`Tests  110 passed (110)` などの要約行は原文どおり
- Claude は案内に従って `contextslim show <id> --from 3 --to 5` を実行し、原文の3〜5行目を取得した
- `show` の出力は再圧縮されなかった（理由 `contextslim_command` で通過）
- 統計に「原文の再取得 1回」が記録された

この検証で、原文の整合性チェックが失敗する不具合を発見し、修正した（`json.Marshal` が `>` `&` をエスケープするため、保存したバイト列とハッシュが一致しなかった）。回帰テストを追加済み。
