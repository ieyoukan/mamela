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
- 書かれていないメンバー、重複したメンバー、`null`、小数・指数表記の数値、不正な UTF-8、文書の後に続くデータは、すべて拒否する（末尾の改行 1 つだけは許す）。省略できるメンバー（k3s の `server`、mamelet の `version`）も、`null` は書けない（書かずに省略する）。
- **見たとおりに表示されない文字は入れられない**（MAM-GRAPH-0109）。manifest の内容とプロバイダーの `spec` の文字列・メンバー名には、改行 `\n` とタブ `\t` 以外の制御文字（復帰 `\r`、ESC を含む）、bidi 制御、ゼロ幅文字などを書けない。plan は全文を読んで承認するもので、k3s は manifest を cluster-admin で適用するので、表示を偽装できる文字を通さない。それでも、plan とエラーは、引用する文字列の制御文字を `\x1b`・`\u202e` のように書き出して表示する（二重の防御）。
- 宣言として正しいが意図と違いそうなもの（MAM-GRAPH-02xx）は、拒否せず**警告**にして、plan のそのリソースの `warnings` に出す。plan の状態と操作は変えない。
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

  - 形が違えば MAM-GRAPH-0106。`spec.network` は、上の 4 つのメンバーだけを持つオブジェクトでなければならない。文字列・配列・数値、未知のメンバー、型違い（`dns` が文字列など）も拒否する（以前は、検査ごと素通りした）。
  - **同じ固定アドレスを 2 台に宣言できない**（MAM-GRAPH-0107）。
  - **site の範囲:** 固定アドレスは site（`mamela.site.toml`）の `network.static_first`–`static_last` の中だけ、DHCP は `network.dhcp = true` のときだけ許す。作るマシンの seed のネットワークが合わなければ、plan の時点で止め（MAM-SITE-0304）、apply でも seed を作る前にもう一度確かめる。
  - ネットワークを作った後で変えるかどうかは、プロバイダーが決める（変えないなら、マニフェストで unsupported とした操作として示し、plan が MAM-POL-0309 で拒否する）。
- **node:** 別の kind のリソースが、マシンと同じ名前を持ってよい（マシン `Vm web` とそのディスク `Disk web`）。`NodePackage`・`NodeService`・`NodeK3s` などの `spec.node` が指すのは、そのうち、マニフェストで `node: true` の kind の 1 つだけである。graph は kind を知らないので、同じ名前のマシンが 2 台あること、node が machine でないリソースを指すことは、プロバイダーが動いたあと（plan の観測の前）に MAM-GRAPH-0103 で止め、どのリソースかを示す。
- **大きさ:** vCPU・メモリ・ディスクの上限は、Graph ではなく site の予算（`[budget]`）が決める。プロバイダーの変更が超えれば MAM-SITE-0305。

### NodePackage

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/<package>` |
| `spec.node` | string | 同じ Graph で宣言したマシン（プロバイダーのリソース）の名前 |
| `spec.package` | string | Debian のパッケージ名（小文字・数字・`+` `-` `.`。英小文字か数字で始まり、2〜128 文字） |
| `spec.state` | string | `"Present"` のみ。削除は宣言できない |

### NodeService

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/<service>` |
| `spec.node` | string | 同じ Graph で宣言したマシン（プロバイダーのリソース）の名前 |
| `spec.service` | string | systemd のユニット名（`.service` なし）。英数字で始まり、英数字と `@` `.` `_` `-` の 128 文字まで（`:` は不可） |
| `spec.enabled` | bool | |
| `spec.running` | bool | |

署名する revision（`internal/noderev`）が node 名・パッケージ名・サービス名・manifest 名・k3s の版・sha256 に課す規則は、graph と同じ定義（`internal/names`）である。**graph が受理した node のリソースは、署名できる**（以前は、`a:b` のサービス名や 129 文字以上のパッケージ名が graph を通り、署名で拒否された。revision は追記だけなので、宣言を直しても、そのノードは revision を受け取れないままになった）。

**同じ unit への矛盾した宣言は拒否する**（MAM-GRAPH-0110）。同じ node の `nginx` と `nginx.service` は同じ unit で、`enabled`・`running` を変えて 2 回宣言できない（同じなら 1 つと同じ）。

### NodeK3s

k3s（軽量な Kubernetes）の server または agent。宣言は、K3s の Pome（`github.com/ieyoukan/mamela/pome/k3s`、モジュール `K3s`）の `K3s.server(graph, node, version, sha256)` と `K3s.agent(graph, node, server, version, sha256)` で書く（ADR 0007）。mamelet が、承認のあと一度だけ、GitHub のリリースからバイナリを取得し、sha256 が一致したときだけ `/usr/local/bin/k3s` に置いて、Mamela が決めた systemd unit を書く。起動は `NodeService`（`k3s`）で行う。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/k3s` |
| `spec.node` | string | 同じ Graph で宣言したマシン（プロバイダーのリソース）の名前 |
| `spec.role` | string | `"server"` か `"agent"` |
| `spec.server` | string | agent のとき、同じ Graph で k3s server を宣言したマシンの名前（server のときは書かない。`null` や空の文字列で「なし」とは書けない） |
| `spec.version` | string | k3s のリリース（`v1.37.2+k3s1`） |
| `spec.sha256` | string | そのノードのアーキテクチャのバイナリの sha256（16 進の小文字 64 桁。リリースの `sha256sum-<arch>.txt`） |

- **起動の宣言:** バイナリを置くだけでは k3s は起動も維持もされず、plan は in sync になる。`NodeK3s` を宣言したノードには、`NodeService`（`k3s`）も宣言する。書き忘れには警告（MAM-GRAPH-0202）が出る。k3s の service を `running=false` にするのは矛盾なので拒否する（MAM-GRAPH-0110）。
- **agent の版:** agent は、その server より新しい版にできない（Kubernetes は API server より新しい kubelet を支えない）。新しければ警告（MAM-GRAPH-0201）。

**版を変える（版上げ）:** 宣言の `version` と `sha256` を変えると、動いている k3s の版上げになる。plan は `upgrade-k3s`（MAM-POL-0212、承認が要る）を出し、承認のあと mamelet がバイナリを置き換えて k3s を再起動する。順番と条件は次のとおり。

- **server が先、agent は名前の順に 1 台ずつ**（MAM-PLAN-0116）。
- **全ノードが Ready のあいだだけ**（MAM-PLAN-0115）。server が `k3s kubectl get nodes` で読んだ、ノードの名前・kubelet の版・Ready を、report で知らせる。読めなければ進まない。
- **動いている版の観測:** バイナリの sha256 が宣言どおりでも、プロセスがそのバイナリより前に起動していれば（`stale`）、収束していない。plan は同じ `upgrade-k3s` で再起動を出す。
- **宣言の `version` も収束に入る（MAM-PLAN-0121）:** バイナリの sha256 が宣言どおりで、k3s が動いていても、Kubernetes のノードが報告する版（server が `k3s kubectl get nodes` で読む kubelet の版）が宣言の `version` と違えば、そのリソースは in sync ではなく、待ち（Waiting）になる。版だけ上げて sha256 を忘れた宣言が、何も言われずに in sync になることはない。kubelet が `v1.37.2` と k3s の revision なしで報告しても、宣言の `v1.37.2+k3s1` とは同じ版である（revision は、報告にあるときだけ比べる）。再起動の直後にノードがまだ古い版を報告しているだけなら、少し待って plan し直すと消える。k3s が止まっているとき、報告が古いとき、Kubernetes のノードの名前が宣言と違うときは、判断しない。
- **巻き戻しの警告:** 動いている版から 7 マイナー版以上戻す宣言には、plan に警告（MAM-PLAN-0401）が出る（拒否はしない）。
- **宣言していないノードは待たない。** Ready の確認は、宣言した k3s のノードだけが対象である。VM を宣言から消したあとに Kubernetes に残るノード（NotReady）は、以後の版上げを止めない。名前がどれも一致しないときは、全ノードを対象にする（できるだけ慎重に）。残ったノードを消す仕組みは、下の「宣言から消えた agent の Kubernetes ノード」にある（Mamela が作ったマシンの跡だけ。それ以外は `k3s kubectl delete node <名前>` を手で行う）。
- **マシンの再起動も 1 台ずつ。** k3s が動いているマシンを再起動する変更（仕様の変更など）は、agent が先（agent は名前の順）、server が最後の順で 1 台ずつ、全ノードが Ready のあいだだけ進む（MAM-PLAN-0118）。server の名前が agent より前（`cp` と `w1`、`w2`）でも、server は最後である。版上げの順（server が先）とは逆である。
- **巻き戻しと drain は行わない。** 版を戻したいときは、宣言を戻して同じ手順で上げ直す。drain は Kubernetes の側の仕事で、Mamela は Ready の観測までにする。

起動の引数は Mamela が決める。宣言からは変えられない。

| 役割 | 起動の引数 |
|---|---|
| server | `k3s server --token-file=/etc/rancher/k3s/cluster-token --disable=traefik --disable=metrics-server --write-kubeconfig-mode=0600` |
| agent | `k3s agent --server=https://<server のアドレス>:6443 --token-file=/etc/rancher/k3s/cluster-token` |

- **参加のトークン:** Control Plane がクラスタごとに乱数で作る。そのクラスタのノードの署名した revision にだけ入れる（ノードとの接続は相互認証の TLS）。宣言・plan・報告には出ない。
- **server のアドレス:** server のマシンが固定アドレス（`spec.network`）を宣言していれば、それを使う（報告を待たず、報告にも従わない）。DHCP なら、server の mamelet が報告した IPv4 アドレスを使う。報告するのは、既定経路を持つインターフェースのアドレスだけである。k3s 自身が足すアドレス（flannel.1 と cni0 の 10.42.0.0/16、サービスの 10.43.0.0/16）は報告しない。これがないと、k3s の起動後に agent の接続先が 10.42.0.0 に変わってしまう。plan はアドレスを表示し、承認したアドレスと revision のアドレスが違えば導入しない。server のアドレスが変わると、plan は agent の `server` を「旧 -> 新」で示し、入れ直しの承認を求める。

#### 宣言から消えた agent の Kubernetes ノード

agent を宣言から消しても、Kubernetes にはそのノードが残る。server の `NodeK3s` の plan が、次のノードを 1 つずつ消す操作（`remove-k3s-node`、MAM-POL-0214）を出す。server が収束していて、動いているときだけ。

- **機械ごと消えるとき:** そのノードのマシンを、この plan が削除する（ADR 0013）。ノードは、マシンの削除の**前**に消す（Ready なら cordon と drain をしてから delete）。
- **取り残されたノード:** Ready でなく、同じ名前のマシンがもう無く、その名前で mamelet が一度登録されている（Mamela が作ったマシンの跡）。drain はできないので、delete だけ。
- **消さないもの:** 宣言しているノード、server 自身のノード、マシンが残っているノード（止まっているだけ、宣言から外しただけ）、mamelet が一度も登録していない Ready でないノード（手で join した、プロバイダーが観測しない機械のもの。Mamela が作った物かどうか分からない。手で `k3s kubectl delete node` する）、server が報告するノードの名前に server 自身が無いクラスタ、policy が `removal = "retain"` のとき。
- **承認:** 削除なので、ホストの `mamela approve` だけ（API は断る）。plan の diff `remove-node` が、消すノードの名前を示し、grant はその名前だけを mamelet に渡す。drain が終わらなければ、何も消さず uncordon して失敗にする。

### NodeK3sManifest

k3s server の自動適用ディレクトリ（`/var/lib/rancher/k3s/server/manifests`）にある、1 つのマニフェストのファイル。k3s は、そこにあるものを cluster-admin の権限で適用し、ファイルが変わるたびにまた適用する（HelmChart、ワークロードなど）。宣言は、K3s の Pome（0.1.1 から）の `K3s.manifest(graph, server, name, content)`。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/manifest/<name>` |
| `spec.node` | string | 同じ Graph で `NodeK3s` の server として宣言したマシンの名前 |
| `spec.name` | string | `[a-z0-9-]`、63 字まで。ファイルは `mamela-<name>.yaml`（k3s 自身のファイルと名前がぶつからない） |
| `spec.content` | string | YAML（UTF-8、NUL なし、64 KiB まで）。改行 `\n` とタブ `\t` 以外の制御文字（復帰 `\r` を含む）、bidi 制御、ゼロ幅文字は不可（MAM-GRAPH-0109）。宣言の中に書く。取得はしない |

- **承認:** `write-k3s-manifest`（MAM-POL-0213）。plan の diff に内容の全文（1 行ごとに `+` を付けて。内容の 1 行が plan の行に見えないように）と sha256 が出る。承認したものと別の内容は書かない。内容を変えるのは、新しい承認になる。
- **書き込み:** mamelet が、承認のあと一度だけ、そのとおりの内容を書く（0600、root。置き換えは原子的）。観測は、ファイルの sha256 だけである。
- **消さない:** 宣言から消しても、ファイルは残る（k3s が適用したものも残る）。消すのはサーバー上で手で行う。削除は ADR 0013 の範囲で、このリソースにはまだない。
- **大きなものは HelmChart で:** 内容が 64 KiB を超えるものは、`HelmChart`（k3s 組み込みの Helm コントローラー）のマニフェストでチャートを指す。

### NodeMamelet

ノードで動く mamelet の版を、宣言で固定する（k3s の binary と同じに、sha256 で）。宣言は、Mamela Pome の `Mamela.mamelet(graph, node, version, sha256)`（0.3.1 から。公開前は `Mamela.on_node(graph, node, "NodeMamelet", ...)`）。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/mamelet` |
| `spec.node` | string | 同じ Graph のノード（provider のマシン） |
| `spec.sha256` | string | linux/amd64 の mamelet の binary の sha256（小文字 16 進 64 桁） |
| `spec.version` | string | 人のための名前（省略できる。`[A-Za-z0-9._+-]`、64 字まで）。plan に出る |

- **承認:** `update-mamelet`（MAM-POL-0215）。plan の diff に、いま動いている sha256 と宣言した sha256 が出る。
- **配る側:** Control Plane は、サイト設定の `controlplane.mamelet_binary` の binary を `/v1/mamelet/<sha256>` で配る。宣言した sha256 が配っているものと違うと、`mamela plan` が警告する（MAM-NODE-0103）。
- **更新:** mamelet が binary を取り、sha256 を確かめ、`-version` で動くことを確かめてから、前の binary を `mamelet.prev` に残して置き換え、自分を再起動する。新しい mamelet が報告しなければ、前に戻る。ノードは 1 台ずつ、名前の順に更新する。手順と限界は [mamelet の更新](mamelet-update.md)。
- **観測:** mamelet は、動いている binary の sha256 を報告する（`/proc/self/exe` のもの。置き換えた直後の、まだ再起動していないプロセスの版ではなく）。

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
| `spec` | JSON のオブジェクト。文字列・整数・真偽値・配列・オブジェクトだけ（`null` と小数は不可。文字列とメンバー名に、改行とタブ以外の制御文字などは不可、MAM-GRAPH-0109）。整数は ±(2^53−1) まで（正規形は数を倍精度で書くので、それを超える整数は別の値になる。黙って変えずに拒否する。大きな値は文字列で書く）。深さ 32 まで。Mamela は中身を解釈せず、正規形（RFC 8785）にしてプロバイダーに渡す |
| `after` | 省略できる。先にできていなければならないプロバイダーのリソースの ID（`"Kind name"`）の配列（下の「依存」） |

宣言では、プロバイダーの Pome の関数（`Kv.kv_item(...)`、`Ec2.instance(...)`）で書く。kit で書いたプロバイダーの Pome の関数は、`mamela provider pome` が manifest から生成する（ADR 0009）。それらは `Mamela.provided`（依存があれば `Mamela.provided_after`）を呼ぶ。マシンの上のものは、プロバイダーの Pome が返す `Mamela.Node`（`Ec2.node(app)` など）に対して `Mamela.package`・`Mamela.service` で書く。k3s は K3s の Pome の `K3s.server`・`K3s.agent` で書く（上の「NodeK3s」）。Mamela の Pome 0.3.0 には、0.2 の `*_on` の名前も `k3s_server`・`k3s_agent` もない（ADR 0007）。

**別の宣言が作った物:** Mamela が別の宣言（stack、Pome の名前。ADR 0013）のために作った物と同じ名前を宣言すると、plan はそのリソースを Blocked にする（MAM-PLAN-0307）。何も変えず、その上のパッケージ・サービス・k3s も待つ。同じ物を 2 つの宣言に書くのは間違いである。作った宣言から消す（プロバイダーがその kind を削除できるなら、削除の plan になる）か、名前を変える。作った宣言が記録される前に作った物（stack が空）は、これまでどおり、どの宣言からも変えられる。

### 依存（`after`、ADR 0007）

クラウドでは、あるリソースが、作るまで決まらない別のリソースの ID を要る（ECS の service は、cluster・task definition・security group の ID が要る）。そのリソースには `after` を書く。次は形だけを示す例である（`spec` の中身はプロバイダーが決める）。

~~~json
{"after": ["AwsEcsCluster main", "AwsEcsTaskDefinition web"], "kind": "AwsEcsService", "name": "web", "provider": "aws",
 "spec": {"cluster": "main", "count": 1, "taskDefinition": "web"}}
~~~

- **参照:** 各要素は、同じ Graph で宣言したプロバイダーのリソースの ID（kind と名前を空白 1 つでつないだもの）。別のプロバイダーのリソースでもよい。
  - 宣言にない参照、組み込みの kind のリソースへの参照は MAM-GRAPH-0103。
  - 循環（自分自身を含む）は MAM-GRAPH-0108。循環に入るリソースごとに、そこから一周する経路（`Thing a -> Thing b -> Thing a`）を示す。
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
