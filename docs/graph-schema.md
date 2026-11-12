# `mamela-graph/v1` — Resource Graph の受け渡し形式

Emela の宣言を WASM で評価したとき、stdout に書かれる JSON 1 文書。Go 側の正は `internal/graph`。

## 例

`examples/homelab`（例の EC2 プロバイダーのインスタンス 1 台と nginx）を評価した結果。

~~~json
{
  "resources": [
    {"kind": "NodePackage", "name": "app-server/nginx",
     "spec": {"node": "app-server", "package": "nginx", "state": "Present"}},
    {"kind": "NodeService", "name": "app-server/nginx",
     "spec": {"enabled": true, "node": "app-server", "running": true, "service": "nginx"}},
    {"kind": "Ec2Instance", "name": "app-server", "provider": "ec2-example",
     "spec": {"diskGiB": 10, "imageId": "ami-0123456789abcdef0", "instanceType": "t3.small",
              "network": {"mode": "dhcp"}, "rootDevice": "/dev/sda1",
              "securityGroupId": "sg-0123456789abcdef0", "subnetId": "subnet-0123456789abcdef0"}}
  ],
  "schema": "mamela-graph/v2"
}
~~~

実際の出力は空白なし・キーはソート順の 1 行。プロバイダーのリソースがあるので `mamela-graph/v2` になる（下の「プロバイダーのリソース」）。

## 規則

- 最上位は `schema`（値は `"mamela-graph/v1"` か `"mamela-graph/v2"`）と `resources`（配列）の 2 つ。
- 各リソースは `kind`・`name`・`spec` の 3 つ。プロバイダーのリソースは、それに `provider` が加わり、`after`（下の「依存」）も持てる。
- 書かれていないメンバー、重複したメンバー、`null`、小数・指数表記の数値、不正な UTF-8、文書の後に続くデータは、すべて拒否する（末尾の改行 1 つだけは許す）。
- 文書は 4 MiB まで、リソースは 10,000 件まで。

## kind

組み込みの kind は `NodePackage`、`NodeService`、`NodeK3s` の 3 つで、どれもマシンの上のものである。

**マシンは、プロバイダーの kind である**（ADR 0004、0006）。プロバイダーがマニフェストで `node: true` とした kind（Proxmox の VM、EC2 のインスタンスなど）のリソースが、mamelet の載るマシンになる。組み込みの `VirtualMachine` はなくなった。書くと MAM-GRAPH-0005 で拒否し、プロバイダーの Pome で宣言するよう示す。

### マシン（プロバイダーの node kind）

- **名前:** マシンの名前は、そのままホスト名になる。小文字・数字・`-` の 1–63 文字で、英数字で始まり英数字で終わる。プロバイダーを動かしたときに確かめる。
- **`spec.network`:** プロバイダーのリソースの `spec` に `network` があれば、Mamela がその中身を確かめる。プロバイダーの Pome は、Mamela の Pome の `Mamela.network_spec(network)` で書く（`network` は `Mamela.Network` の `Dhcp` か `Static(address: ..., gateway: ..., dns: [...])`。例の EC2 の Pome は `Mamela.network_spec(Mamela.Dhcp)` と書く）。

  | フィールド | 型 | 規則 |
  |---|---|---|
  | `spec.network.mode` | string | `dhcp` か `static` |
  | `spec.network.address` | string | `static` のときだけ。IPv4 とプレフィックス長（`192.0.2.10/24`、/8–/30）。ネットワークアドレスとブロードキャストアドレスは不可 |
  | `spec.network.gateway` | string | `static` のときだけ。`address` と同じサブネットの IPv4 で、マシン自身ではない |
  | `spec.network.dns` | string の配列 | `static` のときだけ。IPv4 を 1–3 個 |

  - 形が違えば MAM-GRAPH-0106。
  - **同じ固定アドレスを 2 台に宣言できない**（MAM-GRAPH-0107）。
  - **site の範囲:** 固定アドレスは site（`mamela.site.toml`）の `network.static_first`–`static_last` の中だけ、DHCP は `network.dhcp = true` のときだけ許す。作るマシンの seed のネットワークが合わなければ、plan の時点で止め（MAM-SITE-0304）、apply でも seed を作る前にもう一度確かめる。
  - ネットワークを作った後で変えるかどうかは、プロバイダーが決める（変えないなら、マニフェストで unsupported とした操作として示し、plan が MAM-POL-0309 で拒否する）。
- **大きさ:** vCPU・メモリ・ディスクの上限は、Graph ではなく site の予算（`[budget]`）が決める。プロバイダーの変更が超えれば MAM-SITE-0305。

### NodePackage

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/<package>` |
| `spec.node` | string | 同じ Graph で宣言したマシン（プロバイダーのリソース）の名前 |
| `spec.package` | string | Debian のパッケージ名 |
| `spec.state` | string | `"Present"` のみ。削除は宣言できない |

### NodeService

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/<service>` |
| `spec.node` | string | 同じ Graph で宣言したマシン（プロバイダーのリソース）の名前 |
| `spec.service` | string | systemd のユニット名（`.service` なし） |
| `spec.enabled` | bool | |
| `spec.running` | bool | |

### NodeK3s

k3s（軽量な Kubernetes）の server または agent。宣言は、K3s の Pome（`github.com/ieyoukan/mamela/pome/k3s`、モジュール `K3s`）の `K3s.server(graph, node, version, sha256)` と `K3s.agent(graph, node, server, version, sha256)` で書く（ADR 0007）。mamelet が、承認のあと一度だけ、GitHub のリリースからバイナリを取得し、sha256 が一致したときだけ `/usr/local/bin/k3s` に置いて、Mamela が決めた systemd unit を書く。起動は `NodeService`（`k3s`）で行う。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/k3s` |
| `spec.node` | string | 同じ Graph で宣言したマシン（プロバイダーのリソース）の名前 |
| `spec.role` | string | `"server"` か `"agent"` |
| `spec.server` | string | agent のとき、同じ Graph で k3s server を宣言したマシンの名前（server のときは書かない） |
| `spec.version` | string | k3s のリリース（`v1.37.2+k3s1`） |
| `spec.sha256` | string | そのノードのアーキテクチャのバイナリの sha256（16 進の小文字 64 桁。リリースの `sha256sum-<arch>.txt`） |

起動の引数は Mamela が決める。宣言からは変えられない。

| 役割 | 起動の引数 |
|---|---|
| server | `k3s server --token-file=/etc/rancher/k3s/cluster-token --disable=traefik --disable=metrics-server --write-kubeconfig-mode=0600` |
| agent | `k3s agent --server=https://<server のアドレス>:6443 --token-file=/etc/rancher/k3s/cluster-token` |

- **参加のトークン:** Control Plane がクラスタごとに乱数で作る。そのクラスタのノードの署名した revision にだけ入れる（ノードとの接続は相互認証の TLS）。宣言・plan・報告には出ない。
- **server のアドレス:** server のマシンが固定アドレス（`spec.network`）を宣言していれば、それを使う（報告を待たず、報告にも従わない）。DHCP なら、server の mamelet が報告した IPv4 アドレスを使う。報告するのは、既定経路を持つインターフェースのアドレスだけである。k3s 自身が足すアドレス（flannel.1 と cni0 の 10.42.0.0/16、サービスの 10.43.0.0/16）は報告しない。これがないと、k3s の起動後に agent の接続先が 10.42.0.0 に変わってしまう。plan はアドレスを表示し、承認したアドレスと revision のアドレスが違えば導入しない。server のアドレスが変わると、plan は agent の `server` を「旧 -> 新」で示し、入れ直しの承認を求める。

## プロバイダーのリソース（`mamela-graph/v2`）

外部のプロバイダーの種類のリソースは、`provider` を持つ。これがある文書は `"schema": "mamela-graph/v2"` にする（v1 の文書にあれば MAM-GRAPH-0011）。v1 の文書は、プロバイダーのリソースがない v2 の文書と同じ意味である。Mamela は両方を読み、プロバイダーのリソースがないときは v1 を書く。

~~~json
{"kind": "KVItem", "name": "web", "provider": "kv-example", "spec": {"labels": ["prod"], "value": "v1"}}
~~~

| フィールド | 規則 |
|---|---|
| `kind` | 英大文字で始まり、英数字が 63 文字まで。組み込みの kind は使えない |
| `provider` | プロバイダーの名前（マニフェストの `provider`）。英小文字・数字・`-` |
| `name` | 英数字と `.` `_` `:` `@` `-`、253 文字まで |
| `spec` | JSON のオブジェクト。文字列・整数・真偽値・配列・オブジェクトだけ（`null` と小数は不可）。整数は ±(2^53−1) まで（正規形は数を倍精度で書くので、それを超える整数は別の値になる。黙って変えずに拒否する。大きな値は文字列で書く）。深さ 32 まで。Mamela は中身を解釈せず、正規形（RFC 8785）にしてプロバイダーに渡す |
| `after` | 省略できる。先にできていなければならないプロバイダーのリソースの ID（`"Kind name"`）の配列（下の「依存」） |

宣言では、プロバイダーの Pome の関数（`Kv.item(...)`、`Ec2.instance(...)`）で書く。それらは `Mamela.provided`（依存があれば `Mamela.provided_after`）を呼ぶ。マシンの上のものは、プロバイダーの Pome が返す `Mamela.Node`（`Ec2.node(app)` など）に対して `Mamela.package`・`Mamela.service` で書く。k3s は K3s の Pome の `K3s.server`・`K3s.agent` で書く（上の「NodeK3s」）。Mamela の Pome 0.3.0 には、0.2 の `*_on` の名前も `k3s_server`・`k3s_agent` もない（ADR 0007）。

### 依存（`after`、ADR 0007）

クラウドでは、あるリソースが、作るまで決まらない別のリソースの ID を要る（ECS の service は、cluster・task definition・security group の ID が要る）。そのリソースには `after` を書く。次は形だけを示す例である（`spec` の中身はプロバイダーが決める）。

~~~json
{"after": ["AwsEcsCluster main", "AwsEcsTaskDefinition web"], "kind": "AwsEcsService", "name": "web", "provider": "aws",
 "spec": {"cluster": "main", "count": 1, "taskDefinition": "web"}}
~~~

- **参照:** 各要素は、同じ Graph で宣言したプロバイダーのリソースの ID（kind と名前を空白 1 つでつないだもの）。別のプロバイダーのリソースでもよい。
  - 宣言にない参照、組み込みの kind のリソースへの参照は MAM-GRAPH-0103。
  - 循環（自分自身を含む）は MAM-GRAPH-0108。
  - 組み込みの kind のリソース（`NodePackage` など）に `after` を書くと MAM-GRAPH-0003。マシンの上のものは、もともとマシンを待つ。
- **正規形:** 名前の順に並べ、重複を除く。空なら書かない（`after` のないリソースと同じ）。`after` は digest に入る。
- **plan:** 依存先のどれかが、まだないか、Mamela のものと確かめられない（Foreign など）とき、そのリソースは待つ（MAM-PLAN-0113）。プロバイダーには、そのリソースの diff を求めない。依存先ができた次の plan で、変更が計画される。依存の深さだけ plan と apply を繰り返す。
- **プロバイダーに渡すもの:** diff には、待たないリソースの依存先の観測（Mamela が所有を確かめた ID と状態）を `dependencies` として渡す（[provider-authoring.md](provider-authoring.md)）。
- **宣言:** Mamela の Pome では `Mamela.provided_after(graph, provider, kind, name, spec, after)` と書く。`after` は、Mamela の Pome の型 `Ref(kind: ..., name: ...)` の一覧である。`Ref` は、プロバイダーの Pome が自分の型の値から作る。使う人は型の値を渡すだけなので、宣言にない名前を書き間違えない。

## 正規化と digest

- 正規形: リソースを kind の順（NodePackage → NodeService → NodeK3s → プロバイダーの kind を名前の順）、次に名前の順に並べ、キーをソートし、空白なしで書いたもの。`after` は名前の順に並べて重複を除き、空なら書かない。
- digest: 正規形の SHA-256（`sha256:<hex>`）。宣言の順番を入れ替えても digest は変わらない。
- `pome/mamela` の出力は、リソースが kind 順に並んでいれば正規形と 1 バイトも違わない（テストで確認）。ただし `after` は宣言した順に書くので、名前の順でなければ、Go が並べ直してから digest を取る。

## 互換性

形を変えるときは `mamela-graph/v2` にする。v1 のまま意味を変えない。
