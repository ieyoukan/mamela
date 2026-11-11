# ADR 0007 — 公開する Pome の形を固め、リソースの依存とクラウドの API を足す

- 状態: Accepted（2026-11-11）
- 日付: 2026-11-11
- 対象: Mamela Pome 0.3.0、グラフ v2、プロバイダーの契約（diff の依存、AWS の JSON の API、一時的な資格情報、複数の API、カタログの値の許可リスト）、K3s Pome

## 文脈

ADR 0006 で、Mamela Pome は破壊的に変わる（0.3.0）。Pome の公開は取り消せず、版を上げるたびに、宣言を書く人に移行を求める。そこで、公開の前に次の 3 つを決め、何度も版を上げずに済む形にする。

1. **Mamela Pome に何を残すか。** 0.2 の名前の別名（`*_on`）と、k3s の関数がある。
2. **リソースの依存。** クラウドでは、あるリソースが別のリソースの ID を要る。ECS の service は、cluster・task definition・security group の ID が要り、それらは作るまで決まらない。
3. **AWS で実際に使う API。** ECS・CloudWatch Logs・SSM は、本文が JSON の API である（`X-Amz-Target` で呼び出しを区別する）。また、検証で使う資格情報は、MFA と assume-role の一時的なもの（セッショントークン付き）である。ADR 0005 は、どちらも範囲外としていた。

## 決定

### 1. Mamela Pome 0.3.0 は、OS の汎用のものとグラフだけにする

| 名前 | 中身 |
|---|---|
| `Graph`、`empty`、`to_json`、`print` | グラフ |
| `Node` | mamelet が動くマシン（プロバイダーの Pome が返す） |
| `Network`（`Dhcp`、`Static`）、`network_spec` | マシンのネットワーク。プロバイダーの Pome が spec に書く |
| `package`、`service` | マシンの上の APT パッケージと systemd サービス（`Node` を取る） |
| `Ref`、`provided`、`provided_after` | プロバイダーのリソースと、その依存（§2） |
| `on_node` | マシンの上に置く組み込みの kind のリソース。Mamela 自身のロールの Pome（K3s）だけが使う |

- **0.2 の別名は消す。** `package_on`・`service_on`・`k3s_server_on`・`k3s_agent_on` はなくす。同じものに 2 つの名前を残さない。
- **k3s は別の Pome にする。** `github.com/ieyoukan/mamela/pome/k3s`（モジュール `K3s`）に `K3s.server` と `K3s.agent` を置く。k3s の変更（HA の server、起動の引数）で Mamela Pome の版は上がらない。
- **将来: mamelet のロール。** Go の側（組み込み kind の `NodeK3s` と、mamelet の k3s の対応）は、mamelet にロールを足す仕組みができるまで本体に残す。そのときも `K3s.server` と `K3s.agent` の形は変えず、K3s Pome の中身だけを変える。

### 2. リソースの依存（`after`）: 依存先ができるまで待つ

- **宣言:** グラフ v2 のリソースは、`after`（`"Kind name"` の一覧）を持てる。Pome では `provided_after(graph, provider, kind, name, spec, after: List[Ref])` と書く。`Ref` は、プロバイダーの Pome が自分のリソースから作る（型で参照するので、宣言にない名前を書き間違えない）。
- **Mamela の検査:** `after` の参照は、同じグラフのプロバイダーのリソースでなければならない。宣言にない参照と循環は、グラフの問題として拒む（MAM-GRAPH-0103、MAM-GRAPH-0108）。
- **待ち:** 依存先のどれかが、まだないか、Mamela のものと確かめられないとき、そのリソースは待つ（MAM-PLAN-0113）。プロバイダーは、そのリソースの diff を求められない。依存先ができた次の plan で、変更が計画される。
- **依存先の ID を渡す:** diff には、各リソースの依存先の観測（Mamela が所有を確かめた IDs と状態）を渡す（`DiffParams.Dependencies`）。プロバイダーは、作るまで決まらない ID（security group の ID、cluster の ARN）を、ここから変更に書く。
- **承認は今のまま。** 承認するのは、いつも値の決まった要求である。1 回の apply の中で後から値を埋めることはしない。そのぶん、依存の深さだけ plan と apply を繰り返す。

### 3. AWS の JSON の API

- **呼び出しの区別:** 呼び出しとカタログの項目に、パラメータの場所を 2 つ足す。`header`（HTTP のヘッダーで送るもの）と `json`（本文の JSON のオブジェクトの一段目のメンバー）である。AWS の JSON の API の呼び出しは `header: ["X-Amz-Target"]` と書き、その値（例 `AmazonEC2ContainerServiceV20141113.RunTask`）を `fixed` で決める。呼び出しは、クエリの API の `Action` と同じく、API・メソッド・パス・固定のパラメータで決まる。
- **引数:** `json` に、送ってよいメンバーを並べる。各メンバーを 1 つのパラメータとみなし、その値は正規形の JSON（RFC 8785）の文字列である（`protocol.JSONParams`）。
  - 1 つの呼び出しの本文は、フォームか JSON のどちらかである。
  - `fixed` は JSON のメンバーには書けない（呼び出しはヘッダーで区別する）。
- **承認:**
  - 承認する要求（`Approved.Params`）は、`X-Amz-Target` と、各メンバーの正規形の JSON の文字列を持つ。host は、送る要求のメンバーを正規形にし、承認した値とバイト単位で一致しなければ送らない。
  - `bind`（ADR 0006 の修正）、許可の ID との照合、上限の検査（measures）、`values`（§6）は、メンバーの値に効く。文字列のメンバーは、引用符を外した値で比べる（`protocol.ParamText`）。
  - ECS の CPU（`"256"` は 0.25 vCPU）のために、measures の書式 `cpu-units` を足す。1024 を 1 vCPU とし、予算では切り上げて数える（`"256"` は 1 vCPU）。
- **host がすること:** 本文を、正規形のメンバーを名前の順に並べたオブジェクトにし、`Content-Type: application/x-amz-json-1.1` を付ける。`X-Amz-Target` は、プロバイダーが要求のヘッダー（`HostRequest.Header`）で送り、host が固定の値と照合する。本文を含めて SigV4 で署名する。プロバイダーは鍵を見ない。

### 4. 一時的な資格情報

- `auth = "aws-sigv4"` で、`token_id` と `token_file` の代わりに `profile` を書ける。
- host は、要求ごとに、AWS の共有の資格情報のファイル（`AWS_SHARED_CREDENTIALS_FILE`、なければ `~/.aws/credentials`）からその profile の鍵とセッショントークンを読んで署名する。人が鍵を更新すれば、次の要求から使う。セッショントークンは `X-Amz-Security-Token` で送る。共有の設定のファイル（`~/.aws/config`）は読まない。
- 承認の範囲（接続先の識別）には、鍵ではなく、profile の名前・region・service・URL・カタログを使う。一時的な鍵が入れ替わっても、承認は無効にならない。
- profile の鍵が読めなければ（ファイルや鍵がない）、何も送らない。鍵の期限は Mamela が確かめない。期限の切れた鍵で送った要求は、AWS が拒む。

### 5. 1 つのプロバイダーが複数の API を使う

クラウドのプロバイダーは、サービスごとに別の API を使う。AWS なら EC2・ECS・CloudWatch Logs・IAM で、接続先も、署名のサービス名も、カタログも違う。これを API ごとの別のプロバイダーに分けると、guard も設定も分かれてしまう。そこで、1 つのプロバイダーが複数の API を持てるようにする。

- **マニフェスト:** `apis`（名前の一覧）を書ける。そのときは、各呼び出しが `api` でどれかを名指す。API が 1 つなら今のまま（`api`）。`api` と `apis` は一緒に書けない。
- **要求:** 承認する要求（`Approved`）と host への要求（`HostRequest`）も `api` を持つ。host は、その API のカタログで要求を分類し、その API の接続先に送る。
- **設定:** `[provider.<name>.apis.<api>]` に、API ごとの `api_url`・`service`・`catalog` を書く。認証（`auth`・`profile`・`region`・`service`・`tls_fingerprint`・資格情報・guard の資格情報）は、書かなければプロバイダーの節から受け継ぐ。`apis` があれば、プロバイダーの節には `api_url` と `catalog` を書かない。guard の設定（`[provider.<name>.guard]`）はプロバイダーに 1 つで、API の節には書けない。
  - API の名前は、マニフェストの `apis`、設定の節、カタログの `api` で同じにする（`builtin:ecs` は `ecs`）。
  - 設定のない API への要求は送らない（送っていない、として扱う）。
  - IAM のようにグローバルな API は、その節で `region = "us-east-1"` を書いて署名の region を変える。
- **承認の範囲:** すべての API の接続先の識別をまとめたものにする。

### 6. 実装で決めたこと

AWS のプロバイダー（`github.com/ieyoukan/mamela-aws`）を実際に動かす中で、次のことを決めた。

- **カタログの値の許可リスト（`values`）:** カタログの項目は、パラメータごとに許す値を並べられる。host は、plan の時点と送る前に、ほかの値を拒む（上限の検査より先）。IAM は権限そのものを作る API なので、`builtin:iam` はこれで次のように限る。プロバイダーが何を送っても、ほかのロールは作れない。
  - ロールとインスタンスプロファイルの path は `/mamela/` だけ。
  - 信頼ポリシーは 2 つ（ECS のタスクが引き受ける、EC2 が引き受ける）だけ。
  - 付けられるポリシーは、AWS 管理の ECS のポリシー 2 つ（`AmazonECSTaskExecutionRolePolicy`、`AmazonEC2ContainerServiceforEC2Role`）だけ。
- **先頭が `/` の ID:** 観測の ID の値は、パスの区画を `/` でつないだものである。クラウドの名前には `/` で始まるもの（CloudWatch Logs のロググループ `/ecs/web`）があるので、先頭の `/` を許す。空の値、空や `.`・`..` の区画、そのほかの文字は、今までどおり拒む。
- **メンバーのない JSON の呼び出し:** メンバーのない呼び出し（ECS の `ListClusters`）でも、host は空のオブジェクト `{}` を本文として送る。プロバイダーの空のオブジェクトは、プロトコルの符号化で消えるので、host が補う。
- **ECS の 1 回だけのタスク:** 負荷試験のような 1 回だけのタスクは、あるべき状態ではないので宣言しない。AWS のプロバイダーの、ECS の service の明示操作 `run-task` にする。人が `mamela act --confirm <name> run-task <service>` で名前を打ち直したときだけ、その service と同じやり方（task definition、起動の種類、ネットワーク）でタスクを 1 つ走らせる。
  - これはプロバイダーの操作であり、Mamela 本体の操作ではない（ADR 0006: 明示操作はプロバイダーの契約）。Mamela 本体は ECS を知らない。
  - `builtin:ecs` は、`RunTask` の `cluster` を許可の ID に束縛する。タスクは、対象の service のクラスターでしか走らない。`UpdateService`・`DeleteService`・`StopTask` も `cluster`（と service や task）を束縛し、他人のクラスターの同じ名前のものに届かない。
- **`values` の違反のコード:** site の予算の超過（MAM-SITE-0305）とは分け、MAM-PRV-0008 にする。
- **作成が途中で止まった依存先:** Mamela のものでも、作成が途中で止まっている（`Unfinished`）依存先は待つ。ID がそろっていないことがあるからである。
- **IAM のタグ（プロバイダーの決定）:** タグ付きの `CreateRole` には `iam:TagRole` が要る。実機の運用者のロールにはなかった。AWS のプロバイダーは、ロールにタグを付けない。持ち主は、パス `/mamela/`、説明の目印、Mamela の作成記録で確かめる。Mamela の契約は変わらない（持ち主は、どのみち Mamela の記録で確かめる）。

## 結果

- Mamela Pome 0.3.0 は、汎用の部分だけになり、k3s やクラウドの都合で版を上げずに済む。
- クラウドの API（AWS の JSON の API と、一時的な資格情報）のプロバイダーを、Mamela の外で書ける。依存のあるリソース（VPC → subnet → security group → ECS service）も、確定した要求だけを承認しながら作れる。
- **検証:** `github.com/ieyoukan/mamela-aws` で、運用者の AWS ロール（MFA の assume-role）を使って実機で確かめた（2026-11-11）。
  - **作ったもの:** 専用の VPC の中に、EC2（nginx）、ECS のクラスター、Fargate Spot の locust。
  - **試験と片付け:** `run-task` で負荷試験を 1 回行い、明示操作の `delete` ですべてを消した。
  - **既存のリソース:** 境界の外の EC2 インスタンスとクラスターは、前後の guard で変わっていないことを確かめた。
  - **ECS on EC2:** このロールにはインスタンスプロファイルの権限がない。そのため、コンテナーインスタンス（クラスターに参加する EC2）は、偽の AWS の適合試験でだけ確かめた。
