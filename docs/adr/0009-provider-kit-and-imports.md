# ADR 0009 — provider kit、Pome の生成、Terraform からの乗り換え

- 状態: Accepted（2026-11-12）
- 日付: 2026-11-12
- 対象: プロバイダーの作者の負担（ADR 0002、0006、0008）と、既存の IaC からの移行

## 文脈

プロバイダーを 1 つ作るのに、作者は次のものを書いていた。

- **プロトコルの手続き:** `observe`・`diff`・`apply`・`lookup`・`prepare` を、毎回同じ形で書く。
  - 宣言と観測の突き合わせ。
  - 宣言から外れた自分の物の報告。
  - 中断後の再開（効いている要求を送らない）。
  - 要求のパラメータの置き場所（クエリ、フォーム、ヘッダー、JSON）。
- **Pome の Emela:** 種類ごとの型と、`Mamela.provided` を呼ぶ関数を手で書く。Go の spec の構造体と、内容が二重になる。参照（`after`）を書き忘れやすい。
- **雛形がない:** fake の API、カタログ、適合テスト、`build.sh`、CI を、既存のプロバイダーから写して直す。

さらに、使う人の多くは、すでに Terraform か OpenTofu で基盤を管理している。Mamela に移るには、宣言を書き直したうえで、既存の物を Mamela のものにする必要がある。ところが、Mamela は自分が作った物しか変えない（ADR 0004）。既存の物は Foreign で、宣言すると拒まれる。

## 決定

### 1. provider/kit: 作者は種類ごとの知識だけを書く

`provider/kit` は、`sdk.Provider` を実装する。作者は、種類（`kit.Kind`）ごとに次のものだけを書く。

| 書くもの | 内容 |
|---|---|
| `Reads`・`Operations` | 送りうる呼び出し。読み取りは、kit が各操作に足す |
| `List` | 種類の物の一覧。名前、IDs、版、状態。他人の物は `Foreign`、引き継げる物は `Adoptable` |
| `Diff` | 宣言と観測の差を、変更と送る要求にする |
| `Done` | 要求がすでに効いているか。再開と `lookup` に使う |

必要なら、次のものも書く。

- `Absent`: まだない物の版（作成の前提条件）。
- `Missing`: 作るのに要る観測（空いている ID など）。
- `Scoped`: 名前のほかに、境界で区別する。
- `Explicit`: 明示操作の `Prepare`。
- `Importer`: Terraform からの取り込み（下の 4）。

kit が引き受けるのは次のことである。

- 宣言と観測の突き合わせ。
- 宣言から外れた自分の物の報告。
- Foreign の宣言の拒否（理由と直し方を付ける）。
- 取り込み対象の照合。
- 要求の組み立て。宣言した呼び出しの形に従って、パラメータを置く。
- 中断後の再開（`Done` が真の要求を送らない）。

安全の根拠は変わらない。host が、すべての要求をカタログと許可で確かめる（ADR 0002、0008）。kit は、プロバイダーの中の便利な道具にすぎない。

### 2. spec の型を manifest に載せ、Pome の Emela を生成する

- **型を載せる:** 種類の spec の型を、`protocol.Field` として manifest に載せる（`Kind.Spec`）。
  - 型は、文字列、整数、真偽、リスト、オブジェクト、参照のどれかである。
  - 省略できるかどうか、許す値、説明も載せる。
- **Go から書き出す:** kit は、Go の構造体からこれを書き出す（`kit.SpecOf`）。
  - フィールド名は `json` タグから取る。`omitempty` かポインタなら省略できる。
  - `mamela:"ref=Kind"` は参照になる。
  - `values:"a|b"` は列挙になる。
  - `doc:"..."` は説明になる。
- **生成する:** `mamela provider pome <pome-dir>` は、Pome のプログラムの manifest から `src/lib.emel` を生成する。
  - 種類ごとに、レコード型、宣言する関数、参照（`<kind>_ref`）、ノード（`<kind>_node`）を作る。
  - 列挙から enum を、オブジェクトから型を作る。
  - 参照は、参照先の型の値で受け取る。spec には名前を、`after` には `Ref` を入れる。これで、宣言していない物を参照できず、`after` を書き忘れることもない。
  - 出力は決定的で、`emela fmt` を通す。
  - `--check` は、古くなっていれば失敗する（CI 用）。

生成した Emela を検証の根拠にはしない。Mamela は、プロバイダーの `diff` と host の検査で決める。生成は、使う人の書き間違いを型で防ぐための道具である。

### 3. `mamela provider new`: そのまま通る雛形

`mamela provider new <go-module> <dir>` は、次のものを書く。

- kit で書いたプロバイダー。例の種類は「thing」で、次の操作を持つ。
  - JSON の API への、版を前提条件にした作成と更新。
  - 明示操作の削除と引き継ぎ。
  - Terraform からの取り込み。
- API の fake。
- カタログ。
- 適合テスト。fault の注入と取り込みを含む。
- Pome、`build.sh`、CI。

生成した直後に、`go mod tidy`・`./build.sh`・`go vet`・`go test` が通る（Mamela の試験で確かめる）。

`build.sh` は、`go.mod` の `tool` で固定した版の mamela（`go tool mamela provider pome`）で、Pome の Emela を生成する。作者は、例の種類を自分の API に置き換えていく。公開前の Mamela は、`--mamela <checkout>` でローカルを使う。

### 4. Terraform と OpenTofu から乗り換える

`mamela import terraform [--state <file>] <pome-dir>` で乗り換える。

1. **読む:** `terraform show -json`（`tofu show -json`）の出力を読む。
   - Mamela は Terraform を実行せず、state ファイルも直接は読まない。
   - 読むのは、管理するリソースだけである（data は読まない）。
   - 名前は、モジュールとリソース名と添字から決める（`module.net.aws_vpc.main[0]` は `net-main-0`）。
2. **宣言に変える:** 宣言が依存するプロバイダーのうち、その型を import する（`Kind.Imports`）ものが、新しい RPC `import` で、各リソースを自分の種類の宣言（spec と IDs）に変える。
   - `import` は、`diff` と同じく、要求を一切送れない。
   - 1 つのプロバイダーの物をまとめて渡す。サブネットの `vpc_id` のような参照を、取り込んだ宣言の名前に解決するためである（`kit.Resolver`）。
   - guard の設定（`[provider.<name>.guard]`、秘密は含まない）も渡す。Terraform が言わず、サイトの宣言に要るもの（AWS の境界）をここから取る。
   - 1 つの宣言が、ほかの物を含むことがある（ルートテーブルの関連付け、別リソースで書いたセキュリティグループのルール）。それらは `Also` に並べ、宣言と一緒に移す。
   - host は結果を確かめる。種類がその型を import するか、物が 1 度ずつか（含まれる物も）、名前・IDs・spec が正しいか。
3. **Emela に書く:** 宣言を、生成した Pome の型と関数を使う Emela（`src/imported.emel`、関数 `imported`）として書く。
   - 生成した型と関数は、Pome の `src/lib.emel`（全体が生成物のとき）か `src/kinds.emel`（`lib.emel` を手で書くとき。モジュール `<Pome>.Kinds`）にある。
   - 参照される物を先に書く。
   - これは使う人のものになり、自由に直してよい。
4. **記録する:** 各物を、引き継ぐ対象として SQLite に記録する（`adoptions`、migration 12）。
5. **何も変えない:** 観測では、記録した IDs の物を、その宣言の `Adoptable` として報告する（`ObserveParams.Adopting`）。
   - Mamela の印のない物は一覧に出ないことが多い（AWS はタグで探す）。kit の `Finder` は、IDs で 1 つ探す。
   - host は、報告された物の IDs が記録と一致することを確かめる。
   - plan は何も変えず、`MAM-PLAN-0114` で引き継ぎの手順を示す。
   - その物に `after` で依存する物は待つ。
6. **人が引き継ぐ:** 引き継ぎは、今までどおり明示操作だけで行う（`mamela act --confirm <name> <adopt-op>`、ADR 0006）。
   - 終われば、Mamela が作った物として記録し、引き継ぎの記録を消す。
   - そのあと、plan は宣言との差を示す。
7. **Terraform から外す:** 使う人が `terraform state rm` で、Terraform にその物を忘れさせる。Mamela は、そのコマンドを表示するだけである。

引き継ぎを計画に入れないことは変えない（AGENTS.md）。何十もの物を移すときも、1 つずつ名前を打って引き継ぐ。取り込みが変えるのは、Mamela が「引き継げる」と認める物の範囲だけである。

## 結果

- **作者の負担:** 作者が書くのは、kit の種類、カタログ、fake である。プロトコルと Pome の Emela は書かない。`examples/kv` は kit で書き直し、Pome は生成物になった。
- **manifest:** `Kind.Spec`・`Kind.Doc`・`Kind.Imports` が増える。どれも省略でき、古いプロバイダーはそのまま動く。spec を持たない種類は、Pome を生成できない（手で書く）。
- **protocol:** `import` と `ObserveParams.Adopting` が増える。どちらも、知らないプロバイダーには影響しない。`import` を持たないプロバイダーは、何も取り込まない。
- **乗り換え:** Terraform の管理物は、宣言を書き直さずに Mamela へ移せる。安全の線は変わらない。
  - 引き継ぎは人の明示操作である。
  - Foreign は変えない。
  - 要求は host が確かめる。
- **mamela-aws:** kit に移した。ネットワーク（`aws_vpc`、`aws_internet_gateway`、`aws_subnet`、`aws_route_table` とその関連付け・経路、`aws_security_group` とそのルール）を取り込み、`adopt`（`CreateTags` で `mamela:name` と `mamela:boundary` を付ける）で引き継ぐ。guard は、引き継ぎが guard の境界に入ることを確かめる。
- **これから:**
  - mamela-aws の ECS・IAM・CloudWatch Logs・EC2 インスタンスの取り込み。名前の決まり（`/mamela/` の下など）があり、そのままでは引き継げない。
  - mamela-proxmox を kit に移し、取り込みを足す。
  - OpenAPI などの API 定義から、カタログと呼び出しの宣言を起こす道具は、まだない。
