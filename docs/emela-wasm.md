# Emela / WASM 評価経路 — ステップ 0 の記録

> 2026-11-08、emela 0.27.0 / Wasmtime 49.0.5 / Go 1.27 / macOS arm64 で確認。
> 再現: `scripts/install-toolchain.sh && scripts/check.sh`

## 結論

Emela → WASM → Go の経路は、**副作用なしで、決定的に**動く。同じソースから 2 回評価して、Graph の digest も `main.wasm` の digest も一致した。

## 使い方

~~~bash
scripts/install-toolchain.sh          # .tools/bin に emela と wasmtime（sha256 固定）
go run ./cmd/mamela eval --twice examples/homelab
go run ./cmd/mamela eval --json examples/homelab | jq .
~~~

## 分かったこと

### Emela 0.27

| 事柄 | 内容 | 対応 |
|---|---|---|
| ホストとの関数呼び出し | ユーザーの `@export` / `@external` は WASM でコンパイルエラー | stdout に JSON 1 文書を書く方式（[graph-schema.md](graph-schema.md)） |
| JSON | std にない。`x/json` v0.3.0 は 0.26 向けで、0.27 ではコンパイルできない（`fn(x) { }` 形式の無名関数が E0148） | `pome/mamela/src/json.emel` に小さな書き出しを持つ。形式は `x/json` と同じ |
| 無名関数 | `|x| body` で書く | — |
| `escape` | 予約語（エラー処理の構文）。関数名に使えない | — |
| `++` | 文字列連結の演算子はない | `String.concat` / `List.join` |
| 依存の名前解決 | 依存先の型を使うには、自分の `[dependencies]` に直接書く必要がある | — |
| `emela fmt` | 1 回目と 2 回目で結果が変わることがある（揃え位置が往復する） | 2 回かけてから `--check`。upstream に報告候補 |

### WASM コンポーネントの import

- 実際の import は `wasi:cli/{exit,stderr,stdout,types}@0.3.0`。spec は `types` に触れていないが、中身は `error-code` 列挙だけで関数はない。
- `Std.Env` / `Std.Random` / `Std.Clock` を使うと、それぞれ `wasi:cli/environment` / `wasi:random/random` / `wasi:clocks/system-clock` が増える。
- **Wasmtime はこれらを普通に提供するので、import 検査がなければ非決定的な評価がそのまま動く。** import 検査は決定性の担保として必須。

### Wasmtime 49 での制限と、止まり方

| 状況 | Wasmtime の挙動 | Mamela の判定 |
|---|---|---|
| 時間切れ（`-W timeout`） | 終了コード 134、stderr に `wasm trap: interrupt` | `MAM-EVAL-0303` |
| メモリ上限（`-W max-memory-size`） | 終了コード 134、stderr に `emela: out of memory` | `MAM-EVAL-0304` |
| stdout の出しすぎ | 制限なしだと 3 秒で 1.29 GB 出た | Go 側で上限に達したらプロセスを止める（`MAM-EVAL-0305`） |
| Emela の error | 終了コード 1、`error: ...` | `MAM-EVAL-0301` |
| Emela の defect | 終了コード 101、`defect: ...` | `MAM-EVAL-0302` |

### オフラインビルド

- `EMELA_POMEPROXY=off` で取得を止め、`EMELA_POME_CACHE` に事前取得した依存だけを使える。キャッシュにない依存は `nowhere to get it from (EMELA_POMEPROXY)` でビルドが失敗する。
- `[replace]` でパス指定した依存（`pome/mamela`）は lock に hash が載らない。公開して lock で固定するまでは、リポジトリ内のソースそのものが正。

### Go 1.27

- `encoding/json/v2` が標準で使える。重複キー、未知のメンバー（`RejectUnknownMembers`）、小数・指数表記の整数、不正な UTF-8、末尾のデータをすべて拒否できる。

## 残る課題

- emela コンパイラはネイティブに動き、サンドボックスの外にある。悪意あるソースに対しては、コンテナや専用ユーザーでのビルドが必要。
- fuel（命令数）による制限は使っていない。時間制限で足りているが、マシンの速さに依存しない制限が要るなら検討する。
- Linux arm64 向けの emela バイナリは公開されていない（macOS arm64 と Linux x86_64 のみ）。
