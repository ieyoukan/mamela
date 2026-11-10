# プロバイダーの書き方

Mamela のプロバイダーは、1 つのプログラムである。Mamela（host）と stdin と stdout で JSON-RPC 2.0（`mamela-provider-rpc/v1`）を話す。実状態を観測し、差分を計算し、承認された変更を 1 つずつ実行する。設計の根拠は [ADR 0002](adr/0002-provider-contract.md) にある。

使う人は、プロバイダーの Pome を宣言で import するだけで使える（「配布と使い方」）。

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
    - `Calls`: 送りうる API 呼び出し。メソッド、パスのテンプレート（`/items/{item}`）、送るパラメータをすべて、クエリ（`Query`）とフォーム（`Form`）に分けて書く。場所はカタログ（API が読む場所）と一致しなければ送れない。読み取りも含める。
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
  - 変更の要求（`Requests`）: 送る変更の要求を 1 つずつ、呼び出しと、送るパラメータの名前と値をすべて書く。apply で送れるのはこれだけで、パラメータの省略・追加・値の違いは拒否される。それぞれ 1 回しか送れない
  - 前提条件（`Precondition`。観測した版）
  - 理由
- **触らないもの:** 宣言されていないリソースと、Mamela が作っていないリソースは変えない。Mamela が作っていない既存のものと名前が衝突したら、エラーにする。

### Apply: 1 つの変更を実行する

- **許可の範囲:** 許可（`Permit`）の範囲だけで要求を送る。

  | 要求 | 守ること |
  |---|---|
  | 変更の要求 | 承認した要求（`Change.Requests`）を、そのまま送る。パスには許可の `IDs` を使う |
  | 前提条件 | カタログが前提条件のパラメータを持つ呼び出しでは、観測した版を送る（diff で承認した要求に入れておく） |

- **再開:** 先に読んで、できていることは飛ばす。前の apply が途中で止まっていても、続きから終える。
- **エラーの扱い:** `host.Request` のエラーは、握りつぶさずに返す。
  - `sdk.Refused`: 許可の外の要求。送られていない。
  - `sdk.NotSent`: 送られていない。
  - `sdk.Uncertain`: 効いたかもしれない。
- **自分で再試行しない:** 結果が分からない要求を、自分で再試行して成功にしない。host は、結果が分からなくなった後や要求を拒否した後は、同じ apply の変更をすべて拒否し、unknown として観測からやり直す。
- **成功と答えてよいとき:** 送った変更がすべて効いたときだけ成功と答える。そうでないのに成功と答えると、host は信じない。適合テストでは不合格になる。

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

## 配布と使い方

### 作者: Pome に入れる

プロバイダーは、Emela の Pome として配る。例は [`examples/kv/pome`](../examples/kv/pome) にある。

| ファイル | 内容 |
|---|---|
| `src/lib.emel` | 種類ごとの型と関数。`Mamela.provided(graph, "kv-example", "KVItem", name, spec)` を呼ぶ。使う人は `Kv.item(...)` と書くだけで、型の検査を受けられる |
| `provider.wasm` | プロバイダーのプログラム（WASM）。再現できる build にする（`go build -trimpath -buildvcs=false`。例では `examples/kv/build.sh`） |
| `mamela-provider.toml` | `provider`・`version`（マニフェストと同じ）、`program`（Pome の中のパス）、`sha256`（プログラムの） |

Pome の版の中身は git のツリーそのもので、`Pome.lock` と sumdb の hash で固定される。そのため、プログラムも同じ鎖で固定される。Mamela はさらに、次のことを確かめる。

- `mamela-provider.toml` の sha256
- プログラムが WASM であること（Pome のネイティブは使わない）
- マニフェストの名前と版

### 使う人: import して plan・apply する

1. 宣言の `Pome.toml` の `[dependencies]` に、プロバイダーの Pome を足す（`emela pome add <path>@<version>`）。どの版を使うかは、ここで決まる。
2. 宣言で import して書く。

   ```emela
   import Mamela
   import Kv.{Item}

   fn infrastructure() -> Mamela.Graph {
     Mamela.empty() |> Kv.item(Item(name: "web", value: "v1", labels: ["prod"]))
   }
   ```

3. API の場所と資格情報を `mamela.providers.toml` に書く。プロバイダーには渡らない。

   ```toml
   [provider.kv-example]
   api_url    = "https://kv.example.com/api"   # http は 127.0.0.1・::1・localhost だけ
   token_file = "~/.config/kv/token"           # Authorization: Bearer <token>
   catalog    = "kv-catalog.json"              # API のカタログ（必須。ないと読み取りも変更も許さない）
   ```

4. `mamela check <pome-dir>` で、各リソースに要る能力を確かめる。
5. `mamela plan <pome-dir>` を実行する。プロバイダーが観測し、diff を計算する。plan は、変更ごとに対象と、送る要求をすべてパラメータまで表示する。承認するのはこの内容である。
6. `mamela approve <digest>` で承認し、`mamela apply <digest>` で実行する。

apply は、plan のときに SQLite に保存したプログラムを動かす。キャッシュや Pome が変わっていても、plan と同じバイト列である。apply は次の手順で進む。

1. 観測し直す。
2. プロバイダーの変更が承認したものと完全に同じであることを確かめる。違えば（たとえば対象の版が変わっていれば）、何もせずに止まる（MAM-REC-0002）。
3. 変更ごとに許可を発行する。許可は一度だけ使えて、SQLite に記録する。

`examples/kv-demo` で試せる（`go run ./examples/kv/cmd/kv-api` が、ローカルの API になる）。

## ネイティブのプロバイダー

ネイティブの実行ファイルは、サンドボックスの外で動く。使う人のファイル、環境変数、ネットワーク（クラウドの VM では、メタデータとその資格情報）に届く。そのため、使う人が明示して信頼したときだけ動かす。

```sh
mamela provider trust --source example.com/myprovider --version 1.2.0 ./my-provider
```

記録は `mamela.trust.toml` に残る。レビューしてコミットする。build が変われば digest が変わり、信頼をやり直す必要がある。今は `mamela provider inspect` での確認にだけ使える。Pome で配るプロバイダーは WASM でなければならない。
