# 検証用ログ

`logs/` は各ツールを実際に実行して取得した出力。手書きのログはない。
`golden/` は `go test ./internal/compress -update` が生成する期待出力で、差分はレビューしてからコミットする。

一時ディレクトリのパスは `/home/dev/project` に置き換えている。

| ファイル | ツール・版 | コマンド |
| --- | --- | --- |
| vitest-fail / vitest-pass / vitest-big-fail | Vitest 3.2.7 | `npx vitest run <dir>`（default レポーター） |
| vitest-verbose-fail | Vitest 3.2.7 | `npx vitest run vt --reporter=verbose` |
| jest-fail / jest-big-fail | Jest 29.7.0 | `npx jest --rootDir <dir>` |
| jest-verbose-fail / jest-big-verbose-fail | Jest 29.7.0 | `npx jest --rootDir <dir> --verbose` |
| tsc-plain | TypeScript 5.9.3 | `npx tsc -p .` |
| tsc-pretty | TypeScript 5.9.3 | `npx tsc -p . --pretty`（ANSIカラーを含む） |
| pytest-fail / pytest-pass / pytest-big-fail | pytest 9.2.0 | `pytest <dir>` |
| pytest-q-fail | pytest 9.2.0 | `pytest tests -q` |
| pytest-v-fail | pytest 9.2.0 | `pytest tests -v` |
| pytest8-fail / pytest8-v-fail | pytest 8.4.2 | `pytest tests` / `pytest tests -v` |
| ruff-full / ruff-ok | ruff 0.17.4 | `ruff check <path>`（full 形式） |
| ruff-concise | ruff 0.17.4 | `ruff check pkg --output-format concise` |

失敗ケースには、オブジェクトの差分、期待値と実際の値、例外、スキップ、xfail、警告、テスト中の console 出力を含めている。
