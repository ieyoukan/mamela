# プロバイダーの書き方

Mamela のプロバイダーは、1 つのプログラムである。Mamela（host）と stdin と stdout で JSON-RPC 2.0（`mamela-provider-rpc/v1`）を話す。実状態を観測し、差分を計算し、承認された変更を 1 つずつ実行する。設計の根拠は [ADR 0002](adr/0002-provider-contract.md) にある。

> 段階 2a の時点では、プロバイダーを書き、host の上で適合テストを通すところまでできる。`mamela plan` と `apply` から外部のプロバイダーを使うのは、段階 2b からである。

## 前提: プロバイダーは信頼されない

Mamela は、プロバイダーが正しく書かれていることを安全の根拠にしない。

- **隔離:** WASM のプロバイダーは、固定版の wasmtime で動く。ファイル、環境変数、ネットワークは与えられない。外と話せる道は、host との JSON-RPC だけである。
- **API への要求:** API への要求は、すべて `host/request` で host に頼む。host が資格情報を付けて送る。プロバイダーは、資格情報も API の URL も見ない。
- **host が確かめること:** host は、要求を 1 つずつ確かめてから送る。

  | いつ | 通す要求 |
  |---|---|
  | `describe` と `diff` | 一切通さない |
  | `observe` と `lookup` | 宣言した読み取りだけ |
  | `apply` | 承認した 1 操作の呼び出し・パラメータ・対象 ID・値・前提条件・期限に合うものだけ |

  合わない要求は送らずに拒否し（`CodeRefused`）、記録する。
- **効果:** 呼び出しの効果（読み取り、作成、変更、再起動、データを失いうる）は、host のカタログで決める。プロバイダーの申告は使わない。カタログが知らない呼び出しを含む操作は、つねに人の承認が要る。
- **結果:** apply の結果（done、failed、partly-done、unknown）は、host が自分の送った要求の記録から決める。プロバイダーが成功と答えても、許可を越える要求をしていれば失敗として扱う。

## 何を作るか

Go では `provider/sdk` を使う。

```go
package main

import (
	"github.com/ieyoukan/mamela/provider/sdk"
	"example.com/myprovider"
)

func main() { sdk.Serve(myprovider.Provider{}) }
```

`sdk.Provider` の 5 つのメソッドを実装する。完全な例は [`examples/kv`](../examples/kv) にある。小さなキー・バリューの API の項目を作り、更新するプロバイダーである。

### Manifest: 宣言する

- **種類（Kind）と操作（Operation）:** 操作ごとに、次のどちらかを書く。
  - **Supported:** 次のものを書く。
    - `Calls`: 送りうる API 呼び出し。メソッド、パスのテンプレート（`/items/{item}`）、送るパラメータをすべて書く。読み取りも含める。
    - `Needs`: 必要な権限。
  - **Unsupported:** 理由を `Why` に書く。
- **宣言しなかったもの:** 宣言しなかった呼び出しとパラメータは、送れない。パラメータを書かない呼び出しは、パラメータを 1 つも送れない。
- **`API`:** 話す API の名前（`kv-example/v1`）。host は、この名前で自分のカタログと資格情報を選ぶ。
- **`ObserveCalls`:** 観測に使う読み取り。
- **削除:** 宣言から消したリソースを削除する操作は作らない。Mamela は、宣言から消えたものを保持する。

### Observe: 読む

- **使える要求:** `ObserveCalls` の読み取りだけを使う。
- **宣言されたリソース:** 宣言されたリソースごとに、存在するか、Mamela が作ったものか（`Owned`。API の中に印を残す）、API の ID（`IDs`）、版（`Version`）、状態を返す。
- **宣言されていないリソース:** Mamela が作ったもので、もう宣言されていないものも、`Owned` として返す。Mamela はこれを見て保持する。

### Diff: 計算する

- **入力だけで計算する:** `Host` は渡されない。要求は送れない。同じ入力からは同じ変更を返す。適合テストは、2 回呼んで比べる。
- **変更の中身:** 変更（`Change`）には、次のものを書く。人や policy は、この内容をそのまま承認する。
  - 操作
  - 対象 ID（`IDs`）
  - 意図を担う値（`Values`。送る値を固定する）
  - 前提条件（`Precondition`。観測した版）
  - 理由
- **触らないもの:** 宣言されていないリソースと、Mamela が作っていないリソースは変えない。Mamela が作っていない既存のものと名前が衝突したら、エラーにする。

### Apply: 1 つの変更を実行する

- **許可の範囲:** 許可（`Permit`）の範囲だけで要求を送る。

  | 要求 | 守ること |
  |---|---|
  | 変更の要求 | パスと同名のパラメータに、許可の `IDs` だけを使う |
  | 固定された値 | 承認した値だけを送る |
  | 前提条件 | カタログが前提条件のパラメータを持つ呼び出しでは、観測した版を送る |

- **再開:** 先に読んで、できていることは飛ばす。前の apply が途中で止まっていても、続きから終える。
- **エラーの扱い:** `host.Request` のエラーは、握りつぶさずに返す。
  - `sdk.Refused`: 許可の外の要求。送られていない。
  - `sdk.NotSent`: 送られていない。
  - `sdk.Uncertain`: 効いたかもしれない。
- **自分で再試行しない:** 結果が分からない要求を、自分で再試行して成功にしない。host は unknown として、観測からやり直す。

### Lookup: 効いているか調べる

読み取りだけで、変更が効いているかを返す。

| 状態 | 返す値 |
|---|---|
| 全部効いている | `done` |
| 何も効いていない | `not-done` |
| 一部だけ効いている | `partly-done` |

適合テストは、次の 3 つを確かめる。

- 適用の前に `done` と答えないこと
- 収束した後に `done` と答えること
- 要求が送られた（応答だけが失われた）後に `not-done` と答えないこと

## 作れないこと

- **ファイル・ソケット・HTTP を開くコード:** import の検査で拒否される（MAM-PRV-0001）。`net/http`、`os.ReadFile`、`os.Open` などを使うパッケージを、プロバイダーの import から外す。偽の API のように `net/http` を使うコードは、テストの側に置く（`examples/kv/kvapi` と `kvprovider` の分け方を参照）。
- **stdout への出力:** stdout には、プロトコルのメッセージしか書けない。診断は stderr に書く（`fmt.Println` は MAM-PRV-0003 になる）。
- **送れる本文:** v1 で送れるのは、クエリとフォームだけである。

## ビルドと確認

```sh
GOOS=wasip1 GOARCH=wasm go build -o my-provider.wasm ./cmd/my-provider
mamela provider inspect --catalog my-api-catalog.json my-provider.wasm
```

`inspect` は、次のものを表示する。

- 形式
- digest
- import の検査の結果
- マニフェスト
- 操作ごとの効果（カタログによる分類）。分類できない呼び出しは、「人の承認が要る」と表示する

## 適合テスト

`provider/providertest` で、プロバイダーのテストを書く。

```go
func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Provider: providertest.BuildWASI(t, "./cmd/my-provider"),
		Catalog:  myapi.Catalog,                                  // the effect of each call
		NewAPI:   func() http.Handler { return myapi.NewFake() }, // a fresh fake of the API
		Cases:    []providertest.Case{{Name: "one", Desired: ...}},
	})
}
```

ケースごとに、空の世界から次のことを確かめる。

- **converges:** 観測、diff（2 回して同じか）、apply の流れを 1 周すると、次のことが成り立つ。
  - すべての apply が done になる。
  - 許可を越える要求が 1 つもない。
  - 次の diff が空になる。
  - lookup が、適用の前と収束の後に正しく答える。
- **keeps-undeclared:** 宣言を空にしても、作ったものが `Owned` として観測される。diff は何も変えない。
- **faults:** 変更の要求を 1 つずつ選び、次の 2 通りの障害を入れる。
  - 送られない
  - 送られて効いたが、応答が失われる

  それぞれについて、次のことを確かめる。
  - host の判定（応答が失われたら unknown）
  - lookup の答え
  - 観測からやり直すと収束すること

テストには、固定版の wasmtime が要る（`mamela toolchain install`、または `MAMELA_TOOLCHAIN_DIR`）。

## カタログ

カタログ（`protocol.Catalog`）は、API の呼び出しごとに、次のものを書いたものである。

- 効果（`read`、`create`、`change`、`restart`、`lose-data`）
- 前提条件のパラメータ

カタログは host のものである。Mamela が知っている API のカタログは、Mamela が持つ。新しい API のカタログは、プロバイダーの作者が提案し、Mamela にレビューされて入る。それまでは、変更のたびに人の承認が要る。

## ネイティブのプロバイダー

ネイティブの実行ファイルは、サンドボックスの外で動く。使う人のファイル、環境変数、ネットワーク（クラウドの VM では、メタデータとその資格情報）に届く。そのため、使う人が明示して信頼したときだけ動かす。

```sh
mamela provider trust --source example.com/myprovider --version 1.2.0 ./my-provider
```

記録は `mamela.trust.toml` に残る。レビューしてコミットする。build が変われば digest が変わり、信頼をやり直す必要がある。できるだけ WASM 版を配布する。
