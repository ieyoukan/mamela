# ADR 0007 — 公開する Pome の形を固め、リソースの依存とクラウドの API を足す

- 状態: Accepted（2026-11-11）
- 日付: 2026-11-11
- 対象: Mamela Pome 0.3.0、グラフ v2、プロバイダーの契約（diff の依存、AWS の JSON の API、一時的な資格情報）、K3s Pome

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

- **呼び出しの区別:** 呼び出しとカタログの項目に `target`（`X-Amz-Target` の値、例 `AmazonEC2ContainerServiceV20141113.RunTask`）を足す。`target` のある呼び出しの本文は JSON（`application/x-amz-json-1.1`）である。呼び出しは、メソッド・パス・target で決まる。
- **引数:** JSON の一段目のメンバーを引数とみなす。`form` に、送ってよいメンバーを並べる。
- **承認:**
  - 変更の要求は、承認した本文（正規形の JSON）とバイト単位で一致しなければ送らない（`Approved.Body`）。
  - `bind`（ADR 0006 の修正）と `fixed` は、一段目のメンバーの値に効く。
  - 上限の検査（measures）も、一段目のメンバーに効く。ECS の CPU（`"256"` は 0.25 vCPU）のために、書式 `cpu-units` を足す。
- **host がすること:** `X-Amz-Target` と `Content-Type` を付け、本文を含めて SigV4 で署名する。プロバイダーは鍵を見ない。

### 4. 一時的な資格情報

- `auth = "aws-sigv4"` で、`token_id` と `token_file` の代わりに `profile` を書ける。
- host は、AWS の共有の設定と資格情報のファイルからその profile を読み、要求ごとに（期限が来れば読み直して）署名する。セッショントークンは `X-Amz-Security-Token` で送る。
- 承認の範囲（接続先の識別）には、鍵ではなく、profile の名前・region・service・URL・カタログを使う。一時的な鍵が入れ替わっても、承認は無効にならない。資格情報が切れていれば、何も送らずに止まる。

### 5. 1 つのプロバイダーが複数の API を使う

クラウドのプロバイダーは、サービスごとに別の API を使う。AWS なら EC2・ECS・CloudWatch Logs・IAM で、接続先も、署名のサービス名も、カタログも違う。これを API ごとの別のプロバイダーに分けると、guard も設定も分かれてしまう。そこで、1 つのプロバイダーが複数の API を持てるようにする。

- **マニフェスト:** `apis`（名前の一覧）を書ける。そのときは、各呼び出しが `api` でどれかを名指す。API が 1 つなら今のまま（`api`）。
- **要求:** 承認する要求（`Approved`）と host への要求（`HostRequest`）も `api` を持つ。host は、その API のカタログで要求を分類し、その API の接続先に送る。
- **設定:** `[provider.<name>.apis.<api>]` に、API ごとの `api_url`・`service`・`catalog` を書く。認証（`auth`・`profile`・`region`・資格情報・guard の資格情報）は、書かなければプロバイダーの節から受け継ぐ。guard の設定（`[provider.<name>.guard]`）はプロバイダーに 1 つである。
- **承認の範囲:** すべての API の接続先の識別をまとめたものにする。

## 結果

- Mamela Pome 0.3.0 は、汎用の部分だけになり、k3s やクラウドの都合で版を上げずに済む。
- クラウドの API（AWS の JSON の API と、一時的な資格情報）のプロバイダーを、Mamela の外で書ける。依存のあるリソース（VPC → subnet → security group → ECS service）も、確定した要求だけを承認しながら作れる。
- **検証:** `github.com/ieyoukan/-mamela-aws` が、ECS の cluster・EC2 のコンテナーインスタンス・Fargate Spot の locust を、専用の VPC の中に作る。既存のリソースには触れない。
