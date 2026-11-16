# ADR 0015 — プロバイダーのプログラムとカタログは、運用者が固定する

- 状態: Accepted（2026-11-17）
- 日付: 2026-11-17
- 対象: ADR 0002（プロバイダーの契約）、0008（カタログ）、0009（kit と取り込み）、0012（mamelacd の信頼の範囲）。監査 A3・B10（[docs/reviews/2026-11-17-iac-audit.md](../reviews/2026-11-17-iac-audit.md)）

## 文脈

宣言のツリーは、依存する Pome を選ぶ（`Pome.toml` の `[dependencies]` と `[replace]`、`Pome.lock`）。プロバイダーの Pome には、プログラム（`provider.wasm`）とカタログが入っている（ADR 0008）。つまりツリーを書ける人は、運用者の資格情報で動くプログラムと、そのプログラムの要求を分類するカタログを選べた。

ADR 0012 の信頼の範囲は、gitops が信頼する作者の PR も plan する、と書いている。監査（A3）は、次の筋書きを kv の偽 API で再現した。

1. 作者の PR ブランチに書ける人が、PR のツリーにプロバイダーの Pome を同梱する。名前は運用者の設定（`mamela.providers.toml`）と同じにする。
2. その Pome のカタログは、`DELETE /items/{item}` を `read` と分類する。プログラムは、観測の最中にその削除を送る。
3. host は「観測の最中は `read` だけを通す」。カタログが `read` と言うので、削除は運用者のトークンで API に届いた。他人の item が消えた。`mamela plan` の表示は「何も変えていない」のままだった。
4. 承認者に plan が届く前に、実世界が変わっている。「plan は何も変えない」「カタログは破れない」（ADR 0012 の 6）という主張が崩れる。

壊れていたのは、「誰が選ぶか」である。ADR 0008 は「運用者がカタログを名指す」と書いたが、名指されたカタログの中身は、ツリーの Pome が決めていた（`catalog = "pome:<name>"` は、その Pome 自身のカタログを使う）。プログラムも同じで、設定に provider の出所を固定するものがなかった。

もう一つ（B10）。provider の API に繋がらないとき、エラー文に接続先の URL（host と path）がそのまま入った。plan のエラーは gitops が PR コメントに載せる。宣言のリポジトリが公開だと、ラボのアドレスが漏れる。

## 決定

### 1. 設定の各プロバイダーに、プログラムとカタログの sha256 を持たせる

`mamela.providers.toml` に次の項目を足す。値は 64 桁の 16 進（`sha256:` を前に付けてもよい）。

| 項目 | 場所 | 固定するもの |
|---|---|---|
| `program_sha256` | `[provider.<name>]` | プログラム（`provider.wasm`）の sha256 |
| `catalog_sha256` | `catalog = "pome:<name>"` と書いた節（`[provider.<name>]` か `[provider.<name>.apis.<api>]`） | その Pome が同梱するカタログのファイルの sha256（`mamela provider catalog` が出す値） |
| `strict_pins` | ファイルの先頭（任意。既定は偽） | 真なら、固定していないプロバイダーを拒む |

- **照合する場所:** `external.Start` の最初。プログラムを検査する前、ましてや実行する前に、ツリーの Pome のプログラムとカタログを、設定の固定と比べる。plan、apply（plan に保存したプログラムで動かす）、act、guard、claim の、API に繋ぐすべての道が通る。API に繋がない `mamela import` と `mamela provider script` は、設定の API を使わないので、照合しない（スクリプトは、人が読んでから実行する）。
- **合わなければ:** 起動しない。API に何も届かない。`MAM-PRV-0009`。メッセージは、見つけたプログラム（またはカタログ）の digest、固定の値、直し方（`mamela provider pin`）を言う。
- **宣言が選べるもの:** 宣言のツリーが選べるのは、どの Pome の版を import するか（Emela の型と関数を含む）までである。運用者の資格情報で動くプログラムと、その要求を分類するカタログは、運用者が固定した digest のものだけが動く。`[replace]` の相対パス・絶対パス、`Pome.lock` のどちらから来ても、バイト列が違えば拒否される。同じバイト列なら同じプログラムで、問題はない。
- **`catalog = "pome:<name>"`:** 固定したカタログだけを使う。カタログの中身は接続先の識別にも入る（ADR 0008）ので、固定を変えれば承認は効かなくなる（MAM-REC-0007）。
- **運用者自身のカタログのファイル:** 宣言のツリーからは変えられないので、固定は要らない。`catalog_sha256` は書けない（書くと設定の読み込みで断る）。
- **設定されていないプロバイダー:** API の設定がないので、何も送れない。固定は要らない。

### 2. 固定のない既存の設定は、警告つきで動かす

- 既存の `mamela.providers.toml` は、固定がなくても動く。`mamela plan` は stderr に `MAM-PRV-0010` の警告を出す。何が固定されていないか、なぜ危ないか、直し方（`mamela provider pin`、`strict_pins`）を言う。
- `strict_pins = true` と書くと、固定のないプロバイダー（`program_sha256` がない、または `pome:` のカタログに `catalog_sha256` がない）を、設定の読み込みで断る（`MAM-PRV-0010`）。固定のない間は、A3 の筋書きがそのまま残るので、gitops で使うなら、すべて固定してから `strict_pins` を書く。
- 既定を `strict_pins = true` にするのは、0.4 で検討する（既存の運用を壊すので、0.3.2 では選ばない）。

### 3. 固定する値を出す: `mamela provider pin`

`mamela provider pin [--providers <file>] [--check] <pome-dir | declaration-dir> [<provider>]`

- プロバイダーの Pome か宣言のツリーから、各プロバイダーのプログラムと同梱するカタログの sha256 を、設定のどの節に書くかとともに出す。設定にあれば、「pinned」「differs」「not pinned」も出す。`--check` は、すべて pinned でなければ終了コード 1 にする。
- **何も書き換えない。** 設定ファイルには、秘密のファイルの場所やコメントがある。機械が書き換えると、それが失われるので、出した行を、運用者が読んでから写す。
- 出した値は、運用者が読んで選ぶもの（`mamela provider catalog`、`mamela provider inspect`）である。プロバイダーの作者は、リリースの文に、プログラムとカタログの sha256 を書いておくとよい。

### 4. `read` と書かれた呼び出しは、読み取りの HTTP メソッドでなければならない

カタログの検証（`Catalog.Check`、host の起動時にも呼ばれる）で、`read` の呼び出しは次のどれかでなければ拒む（`MAM-PRV-0011`。どのカタログのどの項目かを言う）。

- `GET`（と `HEAD`）
- `POST` で、`fixed` のパラメータを持つもの。`Action=DescribeInstances`、`X-Amz-Target: ...ListClusters` のように、1 つのパスへ POST して呼び出しを `fixed` で区別する API（EC2、ECS など）の読み取り。
- `PUT`・`PATCH`・`DELETE` は、読み取りにならない。

これは、カタログ自身の矛盾を拒むものである。**破れない壁ではない。** `fixed` を持つ POST を `read` と偽る RPC 型の API のカタログは、この検査を通る。誰のカタログを動かすかを決めるのは、1 の固定である。

### 5. 接続先は、エラー文に出さない

- **upstream のエラー:** 接続の失敗は、種類（拒否された、名前が引けない、時間切れ、TLS の証明書、固定した指紋と違う、切られた、など）と `MAM-PRV-0012`、直し方だけを言う。URL、host、port、path、解決の失敗の名前は出さない。Go のエラーは、`HTTP.Detail` で運用者のログ（Control Plane の stderr）に出す。
- **`api_url` の検査:** `CheckBase` のエラーも、値を繰り返さない（設定の節の名前は、読み込む側が付ける）。
- **最後の防衛線:** `host.Error` の文面と、gate が provider に返す upstream のエラーは、URL らしき文字列と接続のアドレス（`ip:port`、`[ipv6]`）を `<address removed>` にする（`host.Redact`）。ポートのない IP アドレスは、宣言の値（承認した要求の値）かもしれず、人が見比べる必要があるので、消さない。
- **plan の他の経路:** gitops の `planFailed` と `internal/api` の `hidden` は、この ADR では変えない。上流で消すことで、PR コメントに届く前に守る。

## 検討したが採らなかったもの

- **設定に Pome のディレクトリを書き、ツリーの Pome を無視する:** プログラムの版が、宣言の Emela の型と食い違える（宣言の `Pome.lock` が選ぶ版と、運用者のディレクトリの版）。版上げが、運用者のホストの作業と、宣言の変更の 2 か所になる。digest の固定なら、版はツリーが選び、運用者は digest だけ照合できる。
- **`Pome.lock` の hash を固定する:** この hash は Pome 全体（`src/lib.emel` を含む）のもので、運用者が読むもの（プログラムとカタログ）より広く、`[replace]` のツリーには hash がない。固定したいのは、資格情報で動くものだけである。
- **Pome の source の path も固定する:** digest が同じなら、プログラムは同じである。path は、名前でしかなく、守るものが増えない。
- **`read` を `GET`（と `HEAD`）に限る:** EC2・ECS・IAM・SSM などの AWS のカタログは、読み取りも `POST` で、`Action` や `X-Amz-Target` で区別する（`mamela-aws`、`examples/ec2`）。限ると、運用中のカタログが読み込めなくなる。`fixed` を持つ POST を許し、`PUT`・`PATCH`・`DELETE` を拒む形にした。
- **既定で `strict_pins` にする:** 既存の運用者の設定がすべて、更新まで動かなくなる。警告と `strict_pins` の選択にした。
- **`mamela provider pin --write` で設定を書き換える:** コメントや、運用者の書き方が失われる（TOML の書き戻しは、これらを保てない）。出した行を写すのを、読んで選ぶ手順の一部にした。
- **複数の digest を許す（移行のため）:** 版上げのとき、古い digest と新しい digest を一度に許す形。設定が複雑になる。版上げは、運用者が新しいものを読んで 1 行書き換える一回の作業とした。

## 結果

- **A3:** ツリーを書ける人は、プロバイダーのプログラムもカタログも選べない。固定した運用では、ツリーに別のプロバイダーを同梱しても、起動しない。
- **版上げ:** プロバイダーの Pome を新しい版にするには、宣言の変更（PR）に加えて、運用者が新しい digest を読んで `mamela.providers.toml` に書く。それまで、PR の plan は `MAM-PRV-0009` で失敗する（コメントに理由が出る）。設定は plan のたびに読み直すので、`mamela api` の再起動は要らない。書き換えたあとに plan し直せば通る（mamelacd がいつ plan し直すかは ADR 0012、[docs/gitops.md](../gitops.md)）。
- **残るもの:** 固定のない間は、A3 の筋書きが残る（警告が出る）。固定したプログラムとカタログの作者は、信頼する人である。RPC 型の `POST` の `read` は、人が読んで確かめるしかない。Emela の評価（宣言の型と関数）はツリーが選ぶが、評価は host の関数を呼べない（AGENTS.md）。
- **例の Pome:** `examples/kv/pome`・`examples/ec2/pome` のプログラムは、`provider/protocol` を含むので、`build.sh` で作り直す。

## 移行（既存の `mamela.providers.toml`）

1. Control Plane のホストで、いま動いている宣言のコミットを展開した場所を指して実行する（`--providers` は、そのホストの設定）。

   ```sh
   mamela provider pin --providers /etc/mamela/providers.toml <declaration-dir>
   ```

2. 出た行のプログラムとカタログを、`mamela provider catalog <declaration-dir>` と `mamela provider inspect` で読んでから、設定の各節に書く。`pome:` のカタログを使う節には `catalog_sha256`、プロバイダーの節には `program_sha256`。API が複数あるプロバイダー（mamela-aws）は、`program_sha256` をプロバイダーの節に、`catalog_sha256` を各 API の節に書く。
3. `mamela provider pin --check --providers ... <declaration-dir>` が 0 で終わることを確かめる。
4. すべてのプロバイダーを固定したら、ファイルの先頭に `strict_pins = true` を書く。以後、固定のないプロバイダーを足すと、設定の読み込みで止まる。
5. プロバイダーの版を上げるときは、PR の plan が `MAM-PRV-0009` で止まる。新しい版を読み、1 の手順の値で書き換える。
