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
  | `describe`、`diff`、`prepare`、`script` | 一切通さない |
  | `observe` と `lookup` | 宣言した読み取りだけ |
  | `guard` | マニフェストの `guard.calls` の読み取りだけ（guard 用の資格情報があればそれで送る） |
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

`sdk.Provider` の 5 つのメソッドを実装する。必要なら、次のインターフェースも実装する（ADR 0006）。

| インターフェース | メソッド | 使うとき |
|---|---|---|
| `sdk.Guarder` | `Guard` | 適用の前後の安全確認（下の「guard」） |
| `sdk.Preparer` | `Prepare` | 削除・再起動・引き継ぎなどの明示操作（下の「明示操作」） |
| `sdk.Scripter` | `Script` | 人が root などで実行する準備・片付けのスクリプト（下の「スクリプト」） |

完全な例は [`examples/kv`](../examples/kv) にある。小さなキー・バリューの API の項目を作り、更新するプロバイダーである。guard（`max_items`）と明示操作 `delete-item` も持つ。

### Manifest: 宣言する

- **種類（Kind）と操作（Operation）:** 操作ごとに、次のどちらかを書く。
  - **Supported:** 次のものを書く。
    - `Calls`: 送りうる API 呼び出し。メソッド、パスのテンプレート（`/items/{item}`）、送るパラメータをすべて、クエリ（`Query`）、フォーム（`Form`）、ヘッダー（`Header`）、JSON の本文のメンバー（`JSON`）に分けて書く。場所はカタログ（API が読む場所）と一致しなければ送れない。読み取りも含める。1 つの呼び出しの本文は、フォームか JSON のどちらかである。
    - `Needs`: 必要な権限。
  - **Unsupported:** 理由を `Why` に書く。
- **宣言しなかったもの:** 宣言しなかった呼び出しとパラメータは、送れない。パラメータを書かない呼び出しは、パラメータを 1 つも送れない。
- **`API`:** 話す API の名前（`kv-example/v1`）。host は、この名前で自分のカタログと資格情報を選ぶ。複数の API を使うなら、代わりに `APIs` を書く（下の「複数の API を使うクラウドのプロバイダー」）。
- **`ObserveCalls`:** 観測に使う読み取り。
- **`Explicit`・`Adopts`:** 明示操作の印（下の「明示操作」）。
- **`Guard`:** guard で使う読み取りの呼び出し（下の「guard」）。
- **`Scripts`:** 書くスクリプトの名前と説明（下の「スクリプト」）。
- **削除:** diff は、宣言から消したリソースを削除しない。Mamela は、宣言から消えたものを保持する。削除が要るなら、明示操作として宣言する。

### Observe: 読む

- **使える要求:** `ObserveCalls` の読み取りだけを使う。
- **宣言されたリソース:** 宣言されたリソースごとに、存在するか、Mamela が作ったものか（`Owned`。API の中に印を残す）、API の ID（`IDs`）、版（`Version`）、状態を返す。
- **宣言されていないリソース:** Mamela が作ったもので、もう宣言されていないものも、`Owned` として返す。Mamela はこれを見て保持する。
- **引き継げるもの:** 以前に別の形で Mamela が作ったもの（Proxmox では、旧組み込みプロバイダーの印を持つ VM）は、`Owned` ではなく `Adoptable` として返す。引き継ぎの明示操作だけが、これに触れられる。
- **ID の形:** `IDs` の名前は、パスのテンプレートの引数になれる名前にする。値は、パスの区画（英数字と `._~:@=+,!-`）を `/` でつないだものにする。先頭の `/` は許す（CloudWatch Logs のロググループ `/ecs/web`）。空の値、空の区画、`.`・`..` の区画は、host が拒む。

### Diff: 計算する

- **入力だけで計算する:** `Host` は渡されない。要求は送れない。同じ入力からは同じ変更を返す。適合テストは、2 回呼んで比べる。
- **依存（ADR 0007）:** 宣言のリソースは、`After` に先に要るリソースの ID を持てる（[graph-schema.md](graph-schema.md) の「依存」）。依存先のどれかが、まだないか Mamela のものでないリソースは待つ（MAM-PLAN-0113）。そのリソースは `Desired` に入らない。`Desired` のリソースの依存先の観測は、`Dependencies`（グラフ ID ごと）で渡る。どれも存在し、Mamela が所有を確かめたものである。作るまで決まらない ID（security group の ID、cluster の ARN）は、ここから変更に書く。
- **存在する対象の変更:** 変更の `IDs` は、観測した `IDs` の中のもの（同じ名前で同じ値）で、1 つ以上なければならない（`protocol.Targets`）。別の対象を指す変更は、plan の時点で拒む。
- **変更の中身:** 変更（`Change`）には、次のものを書く。人や policy は、この内容をそのまま承認する。
  - 操作
  - 対象 ID（`IDs`）
  - 変更の要求（`Requests`）: 送る変更の要求を 1 つずつ、呼び出しと、送るパラメータの名前と値をすべて書く。apply で送れるのはこれだけで、パラメータの省略・追加・値の違いは拒否される。それぞれ 1 回しか送れない
  - 前提条件（`Precondition`。観測した版）
  - 理由
  - `Late`（必要なとき。下の「遅い変更」）
- **触らないもの:** 宣言されていないリソースと、Mamela が作っていないリソースは変えない。Mamela が作っていない既存のものと名前が衝突したら、エラーにする。
- **明示操作は返さない:** `Explicit` の操作の変更を diff が返すと、host が拒む（MAM-PRV-0003）。

### Apply: 1 つの変更を実行する

- **許可の範囲:** 許可（`Permit`）の範囲だけで要求を送る。

  | 要求 | 守ること |
  |---|---|
  | 変更の要求 | 承認した要求（`Change.Requests`）を、そのまま送る。パスには許可の `IDs` を使う |
  | 前提条件 | カタログが前提条件のパラメータを持つ呼び出しでは、観測した版を必ず送る（diff で `Precondition` と承認した要求に入れておく）。ないと plan の時点で拒否される。存在しない対象の版も、API が表せるなら返す（例の API では `0` が「まだない」） |

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
| 分からない（再起動のように、この変更と結びつけられる跡が残らない） | `unknown` |

`unknown` と答えれば、Mamela は結果を分からないままにする。完了とは見なさない。

適合テストは、次の 3 つを確かめる。

- 適用の前に `done` と答えないこと
- 収束した後に `done` と答えること
- 要求が送られた（応答だけが失われた）後に `not-done` と答えないこと

## 作れないこと

- **ファイル・ソケット・HTTP を開くコード:** import の検査で拒否される（MAM-PRV-0001）。`net/http`、`os.ReadFile`、`os.Open` などを使うパッケージを、プロバイダーの import から外す。偽の API のように `net/http` を使うコードは、テストの側に置く（`examples/kv/kvapi` と `kvprovider` の分け方を参照）。
- **stdout への出力:** stdout には、プロトコルのメッセージしか書けない。診断は stderr に書く（`fmt.Println` は MAM-PRV-0003 になる）。
- **送れる本文:** 送れるのは、クエリ、フォーム、宣言したヘッダー、JSON のオブジェクト（宣言したメンバーだけ。ADR 0007）と、host が用意する成果物（下の「ファイルとノード」）だけである。

## ファイルとノード（ADR 0004）

VM のようなマシンを作るプロバイダーには、大きなファイル（イメージ）と、秘密を含むファイル（mamelet の seed）が要る。プロバイダーは、どちらの中身にも触れない。

- **成果物を宣言する:** 変更（Change）の `artifacts` に、必要なファイルを宣言する。
  - `download`: `url`（https）、`sha256`、`name`（保存する名前）
  - `seed`: `node`（リソースの名前）、`nodeId`、`network`

  plan はこれを表示し、承認の対象にする。
- **値として参照する:** 承認する要求の値には、`{artifact:<id>.name}` と `{artifact:<id>.sha256}` を書ける。seed の名前とハッシュは、トークンを含むので apply まで決まらない。host は、成果物を用意したときに値を埋めてから照合する。
- **apply での手順:**
  1. `sdk.Host.Artifact(ctx, id)` で用意を頼み、名前・sha256・大きさを受け取る。
  2. `HostRequest{..., Upload: id}` でアップロードを頼む。host は、宣言したフォームの順にフィールドを書き、カタログが `file` とするパラメータとしてファイルを足して、multipart で送る。pveproxy のように、フィールドの順を決めている API があるので、呼び出しの `Form` は API が読む順に書く。
- **作ったばかりの対象への変更:** 同じ変更の先の要求が作った対象への変更は、観測した版（前提条件）なしで送れる。plan の時点で存在しないので、上書きしてしまう他人の変更がない。host は、この apply で作成が効いた後だけ認める。
- **所有の主張:**
  - 観測では、すべての印（マーカー、タグなど）がそろったものを `owned`、作成が途中で止まったものを `unfinished` と申告する。
  - Mamela は、自分が作ろうとして記録した ID と一致するときだけ、それを自分のものとする。
  - 一致しないものは Foreign として、diff には `owned: false` で渡す。
- **実行しない差分:** 縮小のように決して行わない差分は、マニフェストで `unsupported` とした操作の変更として、`requests` なしで返す。plan が理由とともに拒否する（MAM-POL-0309）。「差分なし」にしてはいけない。
- **大きさの上限:** マシンの vCPU・メモリ・ディスクのように大きさを決めるパラメータは、カタログの `measures` に書く。host は、site の予算（`[budget]`）を plan と apply の両方で確かめる（MAM-SITE-0305）。
- **ノード:** kind に `node: true` を書くと、そのリソースは mamelet が載るマシンになる。
  - 観測では、存在するリソースについて `node`（`id`、`running`、`uptimeSeconds`）を返す。
  - 宣言では、`Mamela.package(Ec2.node(app), "nginx")`（`examples/homelab`）のように、パッケージやサービスや k3s をその上に書ける。プロバイダーの Pome は、マシンから `Mamela.Node` を返す関数（`Ec2.node`）を用意する。
  - Mamela は、観測した `id` で revision と登録トークンを出す。
  - マシンの `spec` に `("network", Mamela.network_spec(network))` を書くと、Mamela がその形と、固定アドレスの重複を確かめる（[graph-schema.md](graph-schema.md)）。
  - seed のネットワークは、site の規則（`network.dhcp`、`static_first`–`static_last`）で確かめる（MAM-SITE-0304）。

## guard: 適用の前後の安全確認（ADR 0006）

共有の本番環境のように、プロバイダーの境界の外に壊してはならないものがあるなら、guard を書く。Mamela は、そのプロバイダーの変更を apply する前後に、必ず guard を呼ぶ。

- **宣言:** マニフェストの `Guard.Calls` に、確認で使う読み取りの呼び出しを書く。`API` が要る。ファイルを送る呼び出しは書けない。
- **呼び出し（`guard`）:** `GuardParams` を受け取り、`GuardResult` を返す。

  | `Stage` | 渡されるもの | すること |
  |---|---|---|
  | `before` | `Changes`: この apply で送りうる変更（Denied は除く）。`mamela act` ではその明示操作の変更、`mamela guard` では空 | 送る前の検査。後で比べるものを `Baseline` に入れて返す |
  | `after` | `Baseline`: `before` が返したもの | `Baseline` と比べる（境界の外が変わっていないかなど） |

  どちらにも `Settings`（`mamela.providers.toml` の `[provider.<name>.guard]` を JSON にしたもの。秘密は入れない）が渡る。
- **結果:** `Findings` は、コード（プロバイダーが決める。例 `KV-0001`、`PVE-*`）、重さ（`ok`・`warn`・`block`）、文。どれかが欠ければ host が拒む。
  - `before` に `block` があれば、Mamela はそのプロバイダーの変更を何も送らない。
  - `after` の `block` は、apply の失敗として報告する。
- **資格情報:** `mamela.providers.toml` に `guard_token_id` と `guard_token_file` があれば、guard の要求はそれで送る（境界の外も見える読み取り専用のもの。Proxmox では `/` の `PVEAuditor`）。なければ通常の資格情報で送る。
- **単独の実行:** `mamela guard <pome-dir>` は `before` だけを実行する。
- **呼ばないとき:** plan を作ったときとプロバイダーの契約が違えば、apply は guard も呼ばない（reconciler が MAM-REC-0007 で止める）。

## 明示操作: 削除・再起動・引き継ぎ（ADR 0006）

データを失う操作や再起動は、plan に入れない。人が `mamela act` で名前を指し、`--confirm` で打ち直したときだけ行う。

- **宣言:** 操作に `Explicit: true` を書く。diff はこの操作を返さない。
- **呼び出し（`prepare`）:** `PrepareParams`（操作、宣言、観測）から、その操作の変更を 1 つ返す。diff と同じく、要求は送れない。返す変更は、その操作・その対象・その kind でなければならない。変更の `IDs` は、観測した対象の `IDs` の中のものだけである（`protocol.Targets`）。変更の中身（要求、パラメータ、前提条件）は diff と同じ規則で、承認する変更と同じように許可で縛られる。
- **対象:** Mamela は、作成を記録したもの（`Owned` で、記録と一致するもの）にしか明示操作をしない。Control Plane のホストにもしない。
- **引き継ぎ（`Adopts: true`）:** `Explicit` の操作にだけ書ける。対象は、観測で `Adoptable` と報告したものだけである。済めば、Mamela はその対象を自分が作ったものとして記録する。
- **Mamela の記録:** `mamela act` は、送る前に意図を記録し（`explicit_operations`）、結果も記録する。カタログが「データを失いうる」とする要求を含む操作が済めば、作成の記録を消す。
- **例:** `examples/kv` の `delete-item`。Proxmox では `delete-vm`、`reboot-vm`、`reset-vm`、`adopt-vm`。AWS（mamela-aws）では、各 kind の `delete` と、ECS の service の `run-task`（その service と同じやり方で、1 回だけのタスクを 1 つ走らせる。ADR 0007）。

## スクリプト: 人が実行する準備と片付け（ADR 0006）

Mamela に使わせるユーザー、トークン、権限を作るには、Mamela が持たない権限が要る。そのスクリプトはプロバイダーが書く。使う人は、Mamela と固定した版のプロバイダーだけで手に入る。

- **宣言:** マニフェストの `Scripts` に、名前（英小文字・数字・`-`）と説明（何をするか、どこでだれが実行するか）を書く。
- **呼び出し（`script`）:** `ScriptParams`（名前、`Settings`、`Credentials`）から本文を返す。
  - `Settings` は guard と同じ `[provider.<name>.guard]`。
  - `Credentials` は資格情報の ID と秘密のファイルの場所（`tokenId`、`tokenFile`、`guardTokenId`、`guardTokenFile`）。秘密そのものは渡さない。
  - 要求は送れない。本文は UTF-8 で 1 byte から 1 MiB まで。
- **使い方:** `mamela provider script <pome-dir> <provider>` で一覧、`... <provider> <script>` で本文を表示する。人が読んでから実行する。
- **例:** Proxmox の `bootstrap`（pool、ユーザー、トークン、ACL を作る。権限は guard が確かめるのと同じマニフェストの needs から導く）、`permissions`、`cleanup`。

## 遅い変更（`late`）

`Change.Late` を `true` にした変更は、その apply のほかのすべての変更の後に送られる。たとえばマシンの起動を、その実行のすべてのマシンの作成の後にする（作成中のディスクの取り込みと、別のマシンの初回起動が重ならないように）。順番を変えるだけで、何も許可しない。

## ビルドと確認

```sh
GOOS=wasip1 GOARCH=wasm go build -o my-provider.wasm ./cmd/my-provider
mamela provider inspect pome                 # Pome のプログラムを、Pome が同梱するカタログで分類する
mamela provider inspect --catalog my-api.json my-provider.wasm   # プログラムと、カタログのファイル
mamela provider catalog pome                 # 同梱するカタログの中身と sha256
```

- **`inspect` の引数:** プログラムか、プロバイダーの Pome のディレクトリを取る。Pome なら、同梱するカタログをすべて使う。
- **`--catalog`:** カタログのファイルを足す。API ごとに重ねられ、カタログの `api` が API を名指す。同じ API のカタログを 2 つ渡すと止まる。
- **カタログのない API:** 表示し、その呼び出しを分類できないものとして示す。

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
		Catalog:  providertest.Catalog(t, "pome/catalogs/myapi.json"), // the catalog the Pome ships
		NewAPI:   func() http.Handler { return myapi.NewFake() }, // a fresh fake of the API
		Cases:    []providertest.Case{{Name: "one", Desired: ...}},
	})
}
```

| `Suite` のフィールド | 意味 |
|---|---|
| `Catalogs` | 複数の API を使うプロバイダー（マニフェストの `APIs`）のカタログを、API の名前ごとに書く（`Catalog` の代わり）。偽の API には、どの API への要求かがヘッダー `X-Mamela-Api` で届く |
| `GuardSettings` | guard の設定（`[provider.<name>.guard]` の JSON）。guard があれば、各ケースの収束の前後に走らせ、block がないことを確かめる |
| `ScriptCredentials` | スクリプトを書くときの資格情報の ID とファイル。宣言したスクリプトがすべて書けることを確かめる |

| `Case` のフィールド | 意味 |
|---|---|
| `Explicit` | 収束した後に、順に行う明示操作（`Op` と `Resource`）。それぞれ done になり、lookup が done か unknown と答えること。引き継ぎなら、その後もう一度収束すること。ほかに、最初の変更が送られない場合と、送られて応答が失われる場合も試す（lookup が、送られていないのに done、効いたのに not-done と答えないこと） |
| `World` | 何かが動く前に、偽の API に置いておく物（引き継ぐもの、他人のもの）。`World` のあるケースは、guard つきの収束と明示操作だけを行う |

ケースごとに、空の世界から次のことを確かめる。

- **converges:** 観測、diff（2 回して同じか）、apply の流れを 1 周とし、依存を待つリソースがなくなるまで周を重ねる（Mamela と同じく、依存先がまだないリソースは diff に渡さず、依存先の観測を `Dependencies` で渡す）。すると、次のことが成り立つ。
  - すべての apply が done になる。
  - 許可を越える要求が 1 つもない。
  - 次の diff が空で、待つリソースもない。
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
- API が各パラメータを読む場所（`query`、`form`、`header`、`json`）と、同じメソッドとパスの呼び出しを区別する固定のパラメータ（`fixed`）
- 前提条件のパラメータ
- `measures`: 大きさを決めるパラメータと、その書式（`count`、`size`、`drive`、`table`、`cpu-units`）。site の予算で確かめる（MAM-SITE-0305）。`cpu-units` は ECS の CPU 単位で、1024 を 1 vCPU とし、切り上げて数える
- `values`（ADR 0007）: パラメータに許す値の一覧。host は、plan の時点と送る前に、ほかの値を拒む（上限の検査より先。MAM-PRV-0008）。JSON の文字列のメンバーは、引用符を外した値で比べる。mamela-aws の IAM のカタログは、これでロールの path・信頼ポリシー・付けられるポリシーを限る
- `bind`: 対象を名指すパラメータ（EC2 の `InstanceId.1`、ECS の `cluster`・`service`）。変更の要求では、許可の同じ名前の ID と同じ値でなければ送らない。そのため、その変更の `IDs` には、同じ名前の ID を入れる。承認した要求を、別の対象に向けられない

### カタログを書いて同梱する（ADR 0008）

カタログは、その API を知っているプロバイダーの作者が書く。Mamela 本体は、特定の基盤のカタログを持たない（[ADR 0008](adr/0008-catalogs-in-provider-pomes.md)）。

- **形式:** カタログは JSON のデータ（`"schema": "mamela-catalog/v1"`）である。Mamela は、未知のメンバーを拒み、`Check` で確かめて読む（`protocol.DecodeCatalog`）。
- **同梱:** プロバイダーの Pome に、プログラムとは別のファイルとして入れ、`mamela-provider.toml` の `[catalogs]` に名前とファイルを並べる。

  ```toml
  [catalogs]
  ec2 = "catalogs/ec2.json"
  ```

- **書き方:** Go で書いて JSON に書き出してもよい。mamela-aws と mamela-proxmox は、`catalogs` パッケージを `build.sh` で `pome/catalogs/*.json` に書き出す。同梱する JSON が書き出しと同じことは、試験で確かめる。
- **試験:** 同梱するファイルで行う（`providertest.Catalog(t, "pome/catalogs/ec2.json")`）。Mamela が分類に使うものと同じである。
- **使われ方:** カタログは、運用者が `mamela.providers.toml` で名指したときだけ使われる（`catalog = "pome:ec2"`）。運用者は、使う前に `mamela provider catalog <pome-dir>` で読める。
  - Mamela は、カタログをプログラムと一緒に plan に保存する。apply は、それで分類する。
  - カタログの中身は、承認の範囲に入る。カタログが変われば、承認し直す。
- **プログラムとの関係:** プログラムは、カタログを見ることも変えることもできない。host が、プログラムのすべての要求を、運用者が選んだカタログで分類する。プログラムが自分の呼び出しを自己申告で分類することはない。
- **カタログのない呼び出し:** その呼び出しを送る変更は、毎回、人の承認が要る。

## 配布と使い方

### 作者: Pome に入れる

プロバイダーは、Emela の Pome として配る。例は [`examples/kv/pome`](../examples/kv/pome) にある。

| ファイル | 内容 |
|---|---|
| `src/lib.emel` | 種類ごとの型と関数。`Mamela.provided(graph, "kv-example", "KVItem", name, spec)` を呼ぶ。使う人は `Kv.item(...)` と書くだけで、型の検査を受けられる |
| `provider.wasm` | プロバイダーのプログラム（WASM）。再現できる build にする（`go build -trimpath -buildvcs=false`。例では `examples/kv/build.sh`） |
| `mamela-provider.toml` | `provider`・`version`（マニフェストと同じ）、`program`（Pome の中のパス）、`sha256`（プログラムの）、`[catalogs]`（同梱するカタログの名前とファイル） |
| `catalogs/*.json` | API のカタログ（`mamela-catalog/v1`。上の「カタログを書いて同梱する」） |

Pome の版の中身は git のツリーそのもので、`Pome.lock` と sumdb の hash で固定される。そのため、プログラムも同じ鎖で固定される。Mamela はさらに、次のことを確かめる。

- `mamela-provider.toml` の sha256
- プログラムが WASM であること（Pome のネイティブは使わない）
- マニフェストの名前と版
- 同梱するカタログが、Pome の中のファイルで、`mamela-catalog/v1` として正しいこと

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
   catalog    = "kv-catalog.json"              # API のカタログ: Pome が同梱するもの（"pome:<名前>"）か、自分のファイル

   [provider.kv-example.guard]                 # guard の設定（秘密は書かない）
   max_items = 5
   ```

   guard 用に別の読み取り専用の資格情報があるなら、`guard_token_id` と `guard_token_file` も書く。

4. `mamela check <pome-dir>` で、各リソースに要る能力を確かめる。
5. `mamela plan <pome-dir>` を実行する。プロバイダーが観測し、diff を計算する。plan は、変更ごとに対象と、送る要求をすべてパラメータまで表示する。承認するのはこの内容である。
6. `mamela guard <pome-dir>` で、プロバイダーの guard の前の検査だけを確かめられる（読み取りだけ）。
7. `mamela approve <digest>` で承認し、`mamela apply <digest>` で実行する。

承認は、そのときの API の接続先（URL・資格情報・カタログ）に対するものである。設定でどれかを変えると、承認は効かなくなる（MAM-REC-0007）。資格情報が AWS の profile なら、鍵ではなく profile の名前で決まる（鍵を更新しても承認は効く）。複数の API を使うプロバイダーでは、すべての API の接続先をまとめたものである。

apply は、plan のときに SQLite に保存したプログラムを動かす。キャッシュや Pome が変わっていても、plan と同じバイト列である。apply は次の手順で進む。

1. guard（前）を走らせる。block があれば何も送らない。
2. 観測し直す。
3. プロバイダーの変更が承認したものと完全に同じであることを確かめる。違えば（たとえば対象の版が変わっていれば）、何もせずに止まる（MAM-REC-0002）。
4. 変更ごとに許可を発行する。許可は一度だけ使えて、SQLite に記録する。`late` の変更は最後に送る。
5. guard（後）を走らせる。

削除や再起動は `mamela act [--confirm <name>] <op> <resource> <pome-dir>` で行う（[lab.md](lab.md)）。

`examples/kv-demo` で試せる（`go run ./examples/kv/cmd/kv-api` が、ローカルの API になる）。

### Proxmox

Proxmox のように、トークンの形式が Bearer でなく、証明書が自己署名の API には、次のように書く（値は架空）。カタログは、mamela-proxmox の Pome が同梱するものを名前で選ぶ。

```toml
[provider.proxmox]
api_url          = "https://pve.example:8006/api2/json"
auth             = "proxmox"                     # Authorization: PVEAPIToken=<token_id>=<token>
token_id         = "mamela@pve!lab"
token_file       = "~/.config/mamela/pve-token"  # chmod 600 でなければ拒否する
guard_token_id   = "mamela-audit@pve!audit"      # guard だけが使う読み取り専用のトークン
guard_token_file = "~/.config/mamela/pve-audit-token"
tls_fingerprint  = "AA:BB:..."                   # サーバー証明書の SHA-256 を固定する
catalog          = "pome:proxmox"                # Pome が同梱するカタログ

[provider.proxmox.guard]
# guard の設定（境界、しきい値）。中身はプロバイダーが決める（mamela-proxmox の docs/lab.md）
```

Proxmox の VM を作り、その上で mamelet を動かすプロバイダーは、[`github.com/ieyoukan/mamela-proxmox`](https://github.com/ieyoukan/mamela-proxmox) にある（Pome は `github.com/ieyoukan/mamela-proxmox/pome`）。このリポジトリの仕組みの見本になっている。

- 成果物: 固定したクラウドイメージと seed
- ノードの kind
- 同じ変更で作る対象への変更
- guard: クォーラム、ノード、トークンの権限がマニフェストの付与とちょうど同じであること、メモリとストレージの余裕、境界（pool と VMID の範囲）、各変更が境界の中であること、境界の外のゲストが変わっていないこと
- 明示操作: `delete-vm`、`reboot-vm`、`reset-vm`、`adopt-vm`（旧組み込みプロバイダーの VM の引き継ぎ）
- スクリプト: `bootstrap`、`permissions`、`cleanup`
- `late`: VM の起動を、その実行のすべての作成の後にする

### クラウドの API（AWS）

EC2 のようなクエリ API と SigV4 の API にも、プロバイダーを作れる（[ADR 0005](adr/0005-cloud-apis.md)）。小さな例が [`examples/ec2`](../examples/ec2) にある。

- **呼び出しの区別:** 呼び出しが同じ `POST /` を使う API では、`fixed` で区別する（`"Action": "RunInstances"`）。`MaxCount` のように値を決めておきたいものも、`fixed` に入れる。JSON の API（ECS など）は、ヘッダー `X-Amz-Target` を `fixed` にする（下の「複数の API を使うクラウドのプロバイダー」）。
- **認証:**

  ```toml
  [provider.ec2-example]
  api_url    = "https://ec2.ap-northeast-1.amazonaws.com"
  auth       = "aws-sigv4"
  token_id   = "AKIA..."                          # アクセスキー ID
  token_file = "~/.config/mamela/aws-secret"      # シークレット（chmod 600）
  region     = "ap-northeast-1"
  service    = "ec2"
  catalog    = "pome:ec2"                         # Pome が同梱するカタログ（examples/ec2/pome/catalogs/ec2.json）
  ```

  `token_id` と `token_file` の代わりに、AWS の profile（一時的な資格情報でもよい）を書ける（下の「複数の API を使うクラウドのプロバイダー」の「設定と資格情報」）。

- **seed:** 成果物 `seed` に `format: "user-data"` を書き、`nodeId` は 0 にする（Mamela が番号を振る）。
  - 作る要求の値に `{artifact:seed.base64}` を書けば、host が中身を入れて送る。
  - 番号は `{artifact:seed.node}` でタグに残し、観測で `node.id` として返す。
- **所有:** 作成は、作る前に決まる ID（クライアントトークン）で記録される。観測では、その ID とインスタンス ID の両方を返す。

### 複数の API を使うクラウドのプロバイダー（ADR 0007）

AWS のように、サービスごとに API が分かれ、クエリの API（EC2、IAM）と JSON の API（ECS、CloudWatch Logs、SSM）が混ざるクラウドにも、1 つのプロバイダーを書ける。guard と設定は 1 つにまとまる。完全な例は [`github.com/ieyoukan/mamela-aws`](https://github.com/ieyoukan/mamela-aws) にある（VPC、IAM、CloudWatch Logs、ECS on EC2 と Fargate Spot）。

- **依存:** プロバイダーの Pome は、自分の型の値から Mamela の `Ref`（`import Mamela.{Ref}` して `Ref(kind: ..., name: ...)`）を作り、`Mamela.provided_after(graph, provider, kind, name, spec, after)` を呼ぶ。diff では、`Dependencies` にある依存先の ID（security group の ID、cluster の ARN）を変更に書く（上の「Diff」）。依存先として宣言されていないものの ID は推測しない（mamela-aws は、エラーにして `after` に足すよう示す）。1 回の apply では、待たないものだけが変わる。依存の深さだけ plan と apply を繰り返す。
- **マニフェストの `APIs`:** `API` の代わりに `APIs`（`["ec2", "ecs", "logs", "ssm", "iam"]` など）を書く。呼び出し（`Call.API`）、承認する要求（`Approved.API`）、host への要求（`HostRequest.API`）で、どの API かを名指す。API の名前は、設定の節の名前とカタログの `api` と同じにする（mamela-aws の `pome:ecs` のカタログは `ecs`）。host は、要求をその API のカタログで分類し、その API の接続先に送る。
- **JSON の API の呼び出し:** `X-Amz-Target` を固定のヘッダーにし、`JSON` に送ってよい本文のメンバーを並べる。

  ```go
  protocol.Call{API: "ecs", Method: "POST", Path: "/",
  	Header: []string{"X-Amz-Target"},
  	Fixed:  map[string]string{"X-Amz-Target": "AmazonEC2ContainerServiceV20141113.CreateCluster"},
  	JSON:   []string{"clusterName", "tags"}}
  ```

  - `fixed` は JSON のメンバーには書けない。
  - 承認する要求の `Params` には、`X-Amz-Target` と、各メンバーの正規形の JSON の文字列を書く（`protocol.JSONParams`。文字列のメンバーは引用符を含めた `"locust"`、数は `1`）。apply では、同じものを `HostRequest.Header` と `HostRequest.JSON` で送る。host はメンバーを正規形にして、承認した値とバイト単位で比べる。
  - `bind`・`values`・`measures`・許可の ID との照合は、メンバーの値に効く。文字列のメンバーは、引用符を外した値で比べる（`protocol.ParamText`）。
  - host は、本文を正規形のメンバーを名前の順に並べたオブジェクトにして、`Content-Type: application/x-amz-json-1.1` で送る。メンバーのない呼び出し（ECS の `ListClusters`）でも `{}` を送る。
- **カタログ:** API ごとに 1 つ同梱する（mamela-aws は `ec2`・`ecs`・`logs`・`ssm`・`iam`）。mamela-aws のカタログの例は次のとおり。
  - IAM のカタログは、`values` で次のように限る。
    - ロールとインスタンスプロファイルの path は `/mamela/` だけ。
    - 信頼ポリシーは 2 つ（ECS のタスク、EC2）だけ。
    - 付けられるポリシーは、AWS 管理の ECS のポリシー 2 つだけ。
  - ECS の `RunTask`・`UpdateService`・`DeleteService`・`StopTask` は、`cluster`（と service や task）を `bind` する。そのため、その変更の `IDs` には `cluster` を入れる。
  - カタログの範囲を越える呼び出しは、変更のたびに人の承認が要る。
- **設定と資格情報:** `[provider.<name>]` に認証を書き、`[provider.<name>.apis.<api>]` に API ごとの `api_url`・`service`・`catalog` を書く（値は架空）。

  ```toml
  [provider.aws]
  auth    = "aws-sigv4"
  profile = "my-profile"        # ~/.aws/credentials の profile。token_id と token_file の代わり
  region  = "ap-northeast-1"

  [provider.aws.apis.ec2]
  api_url = "https://ec2.ap-northeast-1.amazonaws.com"
  service = "ec2"
  catalog = "pome:ec2"

  [provider.aws.apis.ecs]       # logs と ssm も同じ形
  api_url = "https://ecs.ap-northeast-1.amazonaws.com"
  service = "ecs"
  catalog = "pome:ecs"

  [provider.aws.apis.iam]       # IAM はグローバル。us-east-1 で署名する
  api_url = "https://iam.amazonaws.com"
  service = "iam"
  region  = "us-east-1"
  catalog = "pome:iam"

  [provider.aws.guard]          # guard の設定はプロバイダーに 1 つ（API の節には書けない）
  ```

  - API の節は、`auth`・`profile`・`region`・`service`・`tls_fingerprint`・資格情報（guard の資格情報も）を、書かなければプロバイダーの節から受け継ぐ。`apis` を書くなら、プロバイダーの節には `api_url` と `catalog` を書かない。
  - `profile`: host は、要求ごとに、共有の資格情報のファイル（`AWS_SHARED_CREDENTIALS_FILE`、なければ `~/.aws/credentials`）からその profile の鍵とセッショントークンを読む。MFA や assume-role の一時的な鍵を人が更新すれば、次の要求から使う。セッショントークンは `X-Amz-Security-Token` で送る。鍵が読めなければ何も送らない。
  - 設定のない API への要求は送られない。
- **テスト:** `Suite.Catalog` の代わりに `Suite.Catalogs` を書く。偽の API は 1 つの `http.Handler` で、どの API への要求かをヘッダー `X-Mamela-Api` で見分ける。

  ```go
  providertest.Run(t, providertest.Suite{
  	Provider: providertest.BuildWASI(t, "./cmd/my-provider"),
  	Catalogs: map[string]protocol.Catalog{ // as the Pome ships them
  		"ec2": providertest.Catalog(t, "pome/catalogs/ec2.json"),
  		"ecs": providertest.Catalog(t, "pome/catalogs/ecs.json"),
  		"iam": providertest.Catalog(t, "pome/catalogs/iam.json")},
  	NewAPI:   func() http.Handler { return mycloud.NewFake() }, // dispatches on the X-Mamela-Api header
  	Cases:    []providertest.Case{{Name: "one", Desired: ...}},
  })
  ```

## ネイティブのプロバイダー

ネイティブの実行ファイルは、サンドボックスの外で動く。使う人のファイル、環境変数、ネットワーク（クラウドの VM では、メタデータとその資格情報）に届く。そのため、使う人が明示して信頼したときだけ動かす。

```sh
mamela provider trust --source example.com/myprovider --version 1.2.0 ./my-provider
```

記録は `mamela.trust.toml` に残る。レビューしてコミットする。build が変われば digest が変わり、信頼をやり直す必要がある。今は `mamela provider inspect` での確認にだけ使える。Pome で配るプロバイダーは WASM でなければならない。
