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
- **配布物の sha256 は、宣言が書かずに `mamela.sum` に任せられる**（`NodeArtifact` の `sha256`、`NodeAptRepo` の `keySha256`、`NodeK3s` の `sha256`）。評価が、検証の前に `mamela.sum` から埋めるので、正規形と digest には必ず入る（下の「配布物の sha256 と `mamela.sum`」）。
- **見たとおりに表示されない文字は入れられない**（MAM-GRAPH-0109）。manifest とファイル（`NodeFile`）の内容とプロバイダーの `spec` の文字列・メンバー名には、改行 `\n` とタブ `\t` 以外の制御文字（復帰 `\r`、ESC を含む）、bidi 制御、ゼロ幅文字などを書けない。plan は全文を読んで承認するもので、k3s は manifest を cluster-admin で適用するので、表示を偽装できる文字を通さない。それでも、plan とエラーは、引用する文字列の制御文字を `\x1b`・`\u202e` のように書き出して表示する（二重の防御）。
- 宣言として正しいが意図と違いそうなもの（MAM-GRAPH-02xx）は、拒否せず**警告**にして、plan のそのリソースの `warnings` に出す。plan の状態と操作は変えない。
- 文書は 4 MiB まで、リソースは 10,000 件まで。
- `NodeFile` と `NodeK3sManifest` の内容は、`content` に書く代わりに、宣言の Pome の中のファイルを `contentFrom` で指せる（下の「ファイルから取り込む」）。

## ファイルから取り込む（`contentFrom`）

`NodeFile` と `NodeK3sManifest` の spec は、`content`（テキストそのもの）か `contentFrom`（宣言の Pome のルート、つまり `Pome.toml` のあるディレクトリからの相対パス）の、どちらか一方を持つ。両方あれば MAM-GRAPH-0106、どちらもなければ MAM-GRAPH-0004。宣言は Mamela Pome（0.4.1 から）の `Mamela.file_from(graph, node, path, "files/app.conf")`、`Mamela.file_from_with`、`Mamela.systemd_service_from`。K3s の Pome（0.1.2 から）の `K3s.manifest_from(graph, server, name, source)`。設計は [宣言の入力](design/declaration-inputs.md)。

- **正規形は同じ:** Go が、評価のとき、Graph を検査する前にファイルを読み、`contentFrom` を `content` に置き換える。正規形・digest・plan の digest・承認・revision に `contentFrom` は残らず、同じ中身を `content` に書いた宣言と 1 バイトも違わない。中身の検査（大きさ、MAM-GRAPH-0109 など）も、書いたときと同じに行い、問題の場所に `(from files/app.conf)` と、どのファイルかを添える。
- **読むもの:** 評価した Pome のディレクトリの中の、通常のファイルだけ。パスは相対で、`path.Clean` したものと同じ（`./`、`//`、末尾の `/` なし）、`..` を含まない、`\` と制御文字を含まない、1024 バイトまで。パスのどの段もシンボリックリンクではいけない（Pome の中を指すものも）。名前付きパイプなどは開く前に断り、開いたものが見たものと同じ通常のファイルであることを確かめる。
- **中身:** UTF-8 のテキスト（NUL なし）を、バイトのとおりに使う。末尾の改行も含め、改行の変換、空白の除去、変数の展開はしない（CRLF のファイルは、復帰があるので MAM-GRAPH-0109 で断る）。
- **大きさ:** その member の上限（`NodeFile` 256 KiB、manifest 64 KiB）と、1 回の評価で読む中身の合計（4 MiB。同じファイルを 2 か所で指せば 2 回数える。Graph が、書いた宣言で出せるより大きくならないように）。
- **拒否:** 理由ごとに MAM-EVAL-0501〜0506（ない、範囲の外、リンク、通常のファイルでない、テキストでない、大きすぎる）。`contentFrom` の場所と、パスと、直し方を示す。
- **評価の外:** Pome の評価でない Graph（DB から読み直したもの、手で書いたもの）の `contentFrom` は MAM-GRAPH-0003。古い `mamela` も、知らない member として MAM-GRAPH-0003 で断る（黙って違う中身で動くことはない）。
- **plan:** 中身の diff に、どのファイルから来たかを示す（`content (from files/app.conf):`）。これは表示だけで、plan の digest と DB の plan には入らない（承認するのは中身であり、それは全文が出る）。`mamela eval` の一覧にも `(content from files/app.conf)` と出る。
- **Control Plane（mamelacd）:** 評価は、コミットのツリーの宣言のディレクトリだけを読む。ファイルはコミットする（[gitops.md の「宣言から指すファイル」](gitops.md#宣言から指すファイル)）。

## kind

組み込みの kind は、核の `NodePackage`、`NodeService`、`NodeMamelet` と、ノード拡張（[ノード拡張](design/node-extensions.md)）の `NodeK3s`、`NodeK3sManifest`（k3s）、`NodeUser`、`NodeAptRepo`、`NodeArtifact`、`NodeFile`、`NodeAptUpgrades`（linux、ADR 0016）で、どれもマシンの上のものである。

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

k3s（軽量な Kubernetes）の server または agent。宣言は、K3s の Pome（`github.com/ieyoukan/mamela/pome/k3s`、モジュール `K3s`）の `K3s.server(graph, node, version, sha256)` と `K3s.agent(graph, node, server, version, sha256)` で書く（ADR 0007）。sha256 を `mamela.sum` に任せるときは、`K3s.server_pinned(graph, node, version)` と `K3s.agent_pinned(graph, node, server, version)`（0.1.2 から）。mamelet が、承認のあと一度だけ、GitHub のリリースからバイナリを取得し、sha256 が一致したときだけ `/usr/local/bin/k3s` に置いて、Mamela が決めた systemd unit を書く。起動は `NodeService`（`k3s`）で行う。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/k3s` |
| `spec.node` | string | 同じ Graph で宣言したマシン（プロバイダーのリソース）の名前 |
| `spec.role` | string | `"server"` か `"agent"` |
| `spec.server` | string | agent のとき、同じ Graph で k3s server を宣言したマシンの名前（server のときは書かない。`null` や空の文字列で「なし」とは書けない） |
| `spec.version` | string | k3s のリリース（`v1.37.2+k3s1`） |
| `spec.sha256` | string | そのノードのアーキテクチャのバイナリの sha256（16 進の小文字 64 桁。リリースの `sha256sum-<arch>.txt`）。省略すると、`mamela.sum` の、リリースの linux/amd64 のバイナリ（`https://github.com/k3s-io/k3s/releases/download/<version の + を %2B にしたもの>/k3s`）の sha256 になる（今は linux/amd64 だけ） |

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
| `spec.contentFrom` | string | `content` の代わりに、宣言の Pome の中のファイル（`"manifests/argocd.yaml"`）。評価のときに読み、`content` に置き換える（上の「ファイルから取り込む」）。`content` とどちらか一方 |

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

### NodeFile

ノードの上の 1 つのファイル。宣言したとおりの内容・mode・所有者にする（linux 拡張、ADR 0016）。宣言は Mamela Pome（0.4.0 から）の `Mamela.file(graph, node, path, content)`（mode 0644、root の所有、再起動なし）、または `Mamela.file_with(graph, node, Mamela.File(mode: "0640", owner: "www-data", restart: ["nginx"], ..Mamela.file_of(path, content)))`。検査と reload は 0.5.0 から `Mamela.File(check: Some(Mamela.Nginx), reload: ["nginx"], ..Mamela.file_of(path, content))`（`FileFrom` も同じ）。内容を宣言の Pome の中のファイルから取るときは、0.4.1 から `Mamela.file_from(graph, node, path, source)`、または `Mamela.file_from_with(graph, node, Mamela.FileFrom(mode: "0640", ..Mamela.file_from_of(path, source)))`。systemd のサービスは `Mamela.systemd_service(graph, node, name, unit)`（`/etc/systemd/system/<name>.service` に unit を置き、書いたら再起動の対象にし〔`restart: [name]`〕、有効にして動かす〔`Mamela.running`〕）と、unit をファイルから取る `Mamela.systemd_service_from(graph, node, name, source)`。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/file<path>`（`web/file/etc/nginx/nginx.conf`） |
| `spec.node` | string | 同じ Graph のノード（provider のマシン） |
| `spec.path` | string | 絶対 path。`filepath.Clean` したもの（`//`、`.`、`..`、末尾の `/` なし）、255 バイトまで。各部分は英数字と `. _ + @ = : , -` だけ。下の「書かない場所」は拒否する |
| `spec.content` | string | テキスト（UTF-8、NUL なし、256 KiB まで）。改行とタブ以外の制御文字、bidi 制御、ゼロ幅文字は不可（MAM-GRAPH-0109）。空でもよい。1 つのノードのファイルの内容の合計は 1 MiB まで（承認した書き込みは revision に乗る） |
| `spec.contentFrom` | string | `content` の代わりに、宣言の Pome の中のファイル（`"files/app.service"`）。評価のときに読み、`content` に置き換える（上の「ファイルから取り込む」）。`content` とどちらか一方。Mamela Pome（0.4.1 から）の `Mamela.file_from`、`Mamela.file_from_with`、`Mamela.systemd_service_from` |
| `spec.mode` | string | 省略できる（既定 `"0644"`）。8 進 4 桁。setuid・setgid・sticky と、誰でも書けるもの（`o+w`）は拒否 |
| `spec.owner`、`spec.group` | string | 省略できる（既定 `"root"`）。ユーザー名・グループ名（`[a-z_][a-z0-9_-]`、32 字まで）。ノードにないと plan は Blocked（MAM-PLAN-0141） |
| `spec.restart` | string の配列 | 省略できる。同じノードの systemd のサービス名。ファイルを書いたあと、動いていれば再起動する（`systemctl try-restart`。unit がまだない（読み込まれていない）サービスは動いていないので、何もしない。同じ周で unit のファイルがあとから書かれるときもこれ）。宣言していないサービスは警告（MAM-GRAPH-0204）。`mamelet`（`mamelet.service`）は不可（書き込みを行っている mamelet 自身を止めてしまう。graph と mamelet の両方が断る） |
| `spec.reload` | string の配列 | 省略できる。同じノードの systemd のサービス名。ファイルを書いたあと（`check` があれば、それが通ったあと）、active なら `systemctl reload` し、そのあとも active であることを確かめる（active でなくなれば、試みの結果を失敗として報告する。もう一度の reload は求めない）。active でないサービスは reload せず、そう報告する。`restart` と同じ規則で、同じサービスを `restart` と `reload` の両方に書けない（MAM-GRAPH-0106。どちらか一方）。宣言していないサービスは警告（MAM-GRAPH-0204） |
| `spec.check` | string | 省略できる。mamelet に組み込んだ、決まった検査の名前。今は `"nginx"`（`/usr/sbin/nginx -t -q`）だけ。ほかの名前は MAM-GRAPH-0106（hint が使える名前を言う）。宣言はプログラムも引数も書かない。mamelet は、名前が指す決まったプログラムだけを、決まった引数で、シェルなしで実行する（ADR 0016 の追記）。下の「検査」 |

- **書かない場所**（graph が拒否し、mamelet も書く前にもう一度断る。理由と代わりを示す）: `/proc`、`/sys`、`/dev`、`/run`（`/var/run`、`/var/lock`）、`/boot`、mamelet 自身（`/usr/local/sbin/mamelet*`、`/etc/mamelet`、`/var/lib/mamelet`、`mamelet.service`）、k3s のもの（`/etc/rancher/k3s`、`/var/lib/rancher/k3s`、`/usr/local/bin/k3s`、`k3s.service`）、ユーザーとグループ（`/etc/passwd`、`/etc/shadow`、`/etc/group`、`/etc/gshadow`、`/etc/subuid`、`/etc/subgid` とその `-` の控え。NodeUser で宣言する）、`/etc/sudoers` と `/etc/sudoers.d`、`*/.ssh/authorized_keys`（NodeUser のもの）、`/var/lib/dpkg`。APT が信頼するもの（ソースは NodeAptRepo で宣言する。手で書いたソースは、NodeAptRepo がソースごとに限る鍵〔Signed-By〕と mamelet が許す origin を迂回できる。`[trusted=yes]` など）: `/etc/apt/sources.list` と `/etc/apt/sources.list.d`、`/etc/apt/trusted.gpg*`、`/etc/apt/keyrings`、`/usr/share/keyrings`、pinning の `/etc/apt/preferences` と `/etc/apt/preferences.d`（apt が入れる版と出どころを変える）。mamelet が root で実行するものを変えるもの: mamelet と拡張が実行する固定のプログラム（`/usr/bin/systemctl`、`/usr/bin/apt-get`、`/usr/bin/dpkg`、`/usr/bin/dpkg-query`、`/usr/sbin/useradd`、`/usr/sbin/usermod`、検査の `/usr/sbin/nginx` と、merged /usr の `/bin`・`/sbin` の名前）、パッケージのプログラムのディレクトリ（`/usr/sbin`、`/usr/bin`、`/sbin`、`/bin` のすべて。apt・dpkg・パッケージのスクリプトは root でここのプログラムを名前で実行する〔PATH=/usr/sbin:/usr/bin:/sbin:/bin〕ので、新しいファイルはどれかを置き換えるか、あとのディレクトリの同じ名前のものの代わりに動く）、`/usr/local/sbin` と `/usr/local/bin` の、root が実行するプログラムの名前（mamelet のプログラム、apt・dpkg・debconf とその道具、`sh`・`bash`・`perl`・`python3`・`env`、coreutils などパッケージのスクリプトが呼ぶもの〔`sed`・`grep`・`awk`・`find`・`cp`・`mv`・`chown`・`mktemp`・`install`・`ln`・`cat` など〕、`gpgv`、`getent`、`gzip`、`ldconfig`、`start-stop-daemon`、`invoke-rc.d`、`deb-systemd-helper`、`update-alternatives`、`update-initramfs`、`systemd-sysusers`、`useradd`、`sudo` など。root のシェルと systemd のサービスの PATH では先に来る。`apt-daily-upgrade` の unattended-upgrades は、その PATH で dpkg とパッケージのスクリプトを動かす。graph が知るのはこの名前だけで、mamelet は、ノードの `/usr/sbin`・`/usr/bin`・`/sbin`・`/bin` にある名前をすべて断る〔書かず、報告し、plan は MAM-PLAN-0210・0216 で Blocked〕）、`/usr/lib/apt`、`/usr/lib/dpkg`、`/usr/share/debconf`、useradd と usermod が読むもの・実行するもの（`/etc/login.defs`、`/etc/default/useradd`、`/etc/shadow-maint`）と adduser のもの（`/etc/adduser.conf`、`/usr/local/sbin/adduser.local`・`deluser.local`）、動的リンカと名前解決（`/etc/ld.so.preload`、`/etc/ld.so.conf`、`/etc/ld.so.conf.d`、`/etc/ld.so.cache`、`/etc/nsswitch.conf`）、システムのライブラリ（`/lib64`・`/lib32`・`/libx32` と `/usr` のもの、`/lib`・`/usr/lib`・`/usr/local/lib` の multiarch のディレクトリ〔`x86_64-linux-gnu` など〕のすべて、その下の shared object〔`*.so`、`*.so.*`〕。NSS・PAM・gconv のモジュールも名前で読まれる）、apt と dpkg が root で動かすインタプリタのモジュール（`/etc/perl`、`/usr/local/share/perl`、`/usr/local/lib/site_perl`、`/usr/share/perl`、`/usr/share/perl5`、`/usr/lib/python3*`、`/usr/local/lib/python3*`。debconf は Perl、APT のフックは Python）、APT と dpkg の設定（`/etc/apt/apt.conf`、`/etc/apt/apt.conf.d`、`/etc/dpkg/dpkg.cfg`、`/etc/dpkg/dpkg.cfg.d`。`DPkg::Pre-Invoke` などは導入のたびに root でコマンドを動かす）、APT のフックとパッケージのスクリプトが root で動かす・読むもの（`/etc/python3*`〔python3 が起動のたびに読む `sitecustomize`〕、`/etc/needrestart`〔Ubuntu の APT のフックが Perl として評価する〕、`/etc/kernel`〔カーネルの導入のたびに動くスクリプト〕、`/etc/initramfs-tools`〔`update-initramfs` が動かし、initramfs に入れる〕、`/etc/ca-certificates/update.d`〔`update-ca-certificates` が動かす〕）。すべてのサービスに効く systemd のもの: unit のディレクトリ（`/etc/systemd/system`、`/etc/systemd/system.control`、`/etc/systemd/system.attached`、`/usr/local/lib/systemd/system`、`/usr/lib/systemd/system`、`/lib/systemd/system`）の `mamelet.service` と `k3s.service`（その drop-in と、有効にする `*.wants` のリンクを含む）、すべてのサービスの drop-in の `service.d/`、すべてのサービスが入る slice（`system.slice`、根の `-.slice`、すべての slice の `slice.d/`。その制限とネットワーク）、`system.conf` と `system.conf.d`、generator（`system-generators`、`system-environment-generators`）。新しいユーザーの home に写される `/etc/skel` は書ける（中身は新しいユーザーのものになる。`authorized_keys` は上のとおり書かない）。誰でも書けるディレクトリ（`/tmp`、`/var/tmp`。ほかのユーザーが先に何かを置いたり、置いたものを置き換えたりできる）。どれも理由と代わりを示す（これで root の書き込みがすべて安全になるわけではない。承認する人がファイルを読んでも分からない効果を持つ場所を断る）。同じノードで同じ path を 2 度宣言するもの、ファイルの path がほかのファイルの上のディレクトリになるもの、同じノードの `NodeUser` の home の中にあるもの（home はそのユーザーのもので、root が置いたものを置き換えられる）は、MAM-GRAPH-0110。1 つのノードの `NodeFile` は 256 個まで（MAM-GRAPH-0106。Control Plane はそれより多くを revision に入れない。版の違う Control Plane がそれより多くを入れても、mamelet は部分を断らず、許可のあるもの、いま効いている宣言のもの、残りの順に上限まで観測し、ほかは理由だけを報告する〔plan は Blocked〕。断るのは、許可が上限を超えるときだけ）。
- **承認:** `linux.write-file`（MAM-POL-0220）。plan の diff に、新しい内容の全文（1 行ごとに `+`）と sha256、古いものは sha256 だけ（ノードはファイルの内容を Control Plane に送らない）、mode・owner・group の変化、作るディレクトリ（0755、root）、unit なら `daemon-reload`、再起動するサービスが出る。in sync は、sha256・mode・owner・group がすべて宣言と同じとき。
- **書き込み:** mamelet が、承認のあと一度だけ、そのとおりに書く。path の規則、内容の sha256（承認したもの）、path とその上のディレクトリがシンボリックリンクでないことを確かめてから、同じディレクトリの一時ファイルに書き、fsync、chown、chmod、rename で置き換える。ファイルシステムに、ファイルと余白（64 MiB）の空きがなければ書かずに断る。作るディレクトリは、0700 で作り、開いたもの（名前ではなく）の所有者と mode を 0755・root にする。作るまでの間にほかの誰かが作ったものは使わない。`/etc/systemd/system` などの unit なら `systemctl daemon-reload`、そのあと `restart` のサービスを `try-restart` する。途中で止まった書き込みの一時ファイル（`.mamelet-*.tmp`）は、次の周に消す。
- **検査（`check`）:** 同じ apply の、同じノードの、同じ `check` のファイル（検査の組）を、mamelet はまとめて扱う。Control Plane は、組の最初の操作の許可に、承認した plan のほかの組のファイルの書き込みも入れる（reconciler は操作を 1 つずつ行い、その結果を待ってから次に進むので、そうしなければ 2 つで 1 つの設定〔`nginx.conf` とサイトのファイル〕の 1 つめだけで検査が走る）。あとの操作は、ファイルが書かれているのを見つけ、検査をもう一度走らせるだけで、何も書かない。mamelet は (1) 組のすべての書き込みを、ふつうの書き込みと同じく確かめ、1 つでも断れば何も書かない。(2) 書く前に、上書きするもの（内容を状態ディレクトリの `linux-checks.d/<sha256>` に、mode・所有者とともに。なかったことも）を残し、組を記録（`linux-checks.json`）してから書く（256 KiB を超えるファイルは残せないので、検査のある書き込みは断る）。(3) 組のファイルをすべて書いてから、検査を一度だけ走らせる（1 分まで）。(4) 通れば、記録から外し、unit なら `daemon-reload`、`reload` と `restart` のサービスを（組で 1 度ずつ）reload・再起動し、active のままかを確かめる。(5) 通らなければ（検査を走らせられないとき、書き込みが失敗したときも）、書いたファイルを書く前の内容・mode・所有者に戻し（なかったファイルは消す。作ったディレクトリは残る）、何も reload も再起動もしない。結果には、検査が言ったこと（制御文字を書き出し、1000 バイトまで）と、戻したものを書く。戻したファイルは、その状態の間、報告され、plan はそれを示す（MAM-PLAN-0230。宣言を直す）。書いたあとに誰かが変えたファイルは、戻さずに残し、そう言う。
- **検査の前に止まった:** 書いてから検査が終わるまでに mamelet が止まった（kill、電源断）ときは、記録が残る。mamelet は、次に動き始めたとき、どの周でも（revision がなくても、期限が切れていても）まず記録を見て、検査をやり直し、通らなければ戻す。通れば、ファイルはそのままで、reload・再起動は残っていること（MAM-PLAN-0218）になる。終わるまでの間、plan はそのファイルを待たせ（MAM-PLAN-0231）、戻せないとき（理由を示す）や記録が読めないとき（`linux-checks.json.corrupt` として除けたとき）は Blocked にする（MAM-PLAN-0232）。終わっていないものを終わったことにはしない。
- **plan:** 書き込みの変更の行に、`check: nginx -t (...)` と `reload: nginx (...)` が出る。承認する人は、何が検査し、何が reload されるかを読んで承認する。observe-only のノードでは、ほかの書き込みと同じく拒む（MAM-POL-0312）。
- **書いたあとのことが終わらない:** `daemon-reload` か reload・再起動が失敗した、または mamelet が途中で止まった（SIGTERM）ときは、ファイルは承認どおりだが、動いているものはまだそれを使っていない。mamelet は、残っていることを状態ディレクトリ（`linux-pending.json`）に、始める前に記録し、すべて終わったら消す。ファイルが書いたときのままの間、それを報告し、plan は in sync とせずにもう一度承認を求める（MAM-PLAN-0218）。承認すると、mamelet はファイルを書き直さず、残りを行う。止まった（SIGTERM で中断した）試みは、失敗として記録しない。次の周に、許可が有効な間、同じ試みを続ける（ファイルが承認どおりならそれを使う）。
- **ほかのユーザーが変えられるディレクトリ:** 上のディレクトリが root とファイルの所有者のほかのユーザーのもの、またはグループか誰でも書けるものなら、mamelet はその mode と所有者を報告し、plan は書き込みに警告を出す（MAM-PLAN-0144）。そのユーザーは置いたものを置き換えられる。
- **リンクをたどらない:** path かその上がシンボリックリンク、path がディレクトリ・デバイスなら、plan は Blocked（MAM-PLAN-0210）。mamelet は、上のディレクトリを 1 つずつ（リンクでなく、見たものと同じディレクトリであることを確かめて）開き、ファイルは通常のファイルであることを確かめてから `O_NOFOLLOW`・`O_NOCTTY`・`O_NONBLOCK` で開き、開いたもの（fstat）が見たものと同じ通常のファイルであることを確かめて読む。見てから開くまでの間にユーザーがリンクに置き換えても、端末やデバイスを開かない。
- **消さない:** 宣言から外したファイルは、ノードにそのまま残る。plan はそれを MAM-PLAN-0310（retained）で示し、apply のあと mamelet は観測しなくなる。消すのは手で（この版の制限）。
- **秘密は書かない:** 内容は宣言、plan（PR のコメント）、Control Plane の DB、revision に平文で残る。秘密鍵やトークンに見える内容は警告する（MAM-GRAPH-0203）。秘密の扱いは後の版（監査 A4）。
- **mamelet の版:** 0.10.0 より前の mamelet は linux を受け取らない。そのノードの `NodeFile` は Blocked（MAM-PLAN-0122）で、mamelet を先に更新する（[mamelet の更新](mamelet-update.md)）。

### NodeUser

ノードの上の 1 人のユーザー。なければ作り、宣言したグループに入れ、SSH の公開鍵を宣言したとおりにする（linux 拡張、ADR 0016）。宣言は Mamela Pome（0.4.0 から）の `Mamela.user(graph, node, name)`（ふつうのユーザー、補助グループなし、鍵は扱わない）、または `Mamela.user_with(graph, node, Mamela.User(groups: ["docker"], authorized_keys: Some([key]), ..Mamela.user_of(name)))`。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/user/<name>`（`web/user/deploy`） |
| `spec.node` | string | 同じ Graph のノード（provider のマシン） |
| `spec.name` | string | ユーザー名（`[a-z_][a-z0-9_-]`、32 字まで）。`root` は不可（Mamela は root を扱わない） |
| `spec.system` | bool | 省略できる（既定 `false`）。`true` なら system ユーザーとして作る（`useradd --system`）。作るときにだけ使う。あるユーザーと違っても変えず、警告する（MAM-PLAN-0148。uid が 1000 未満かで判断する。`uid` を宣言したユーザーには警告しない: system かどうかは宣言した uid が決め、毎回の plan で同じことを言うだけになるため） |
| `spec.uid` | 整数 | 省略できる（`useradd` が選ぶ）。1 以上（0 は root のもので不可）、2^31−1 まで。作るときにだけ使う。あるユーザーの uid と違えば Blocked（MAM-PLAN-0146。uid は変えない） |
| `spec.shell` | string | 省略できる。絶対 path（空の文字列は不可。省くのと区別できないため）。宣言すれば、あるユーザーの shell も変える（`usermod --shell`）。省略すると、作るときだけ `/bin/bash`（system ユーザーは `/usr/sbin/nologin`）で、あるユーザーの shell は変えない。ノードに実行できるファイルとしてなければ、待つか Blocked（MAM-PLAN-0147） |
| `spec.home` | string | 省略できる（ふつうのユーザーは `/home/<name>` を作る。system ユーザーは `useradd` の既定で、作らない。ただし `authorizedKeys` があれば `/home/<name>` を作る。作る home はいつも名前を渡し〔`--home-dir`〕、ノードの `/etc/default/useradd` に任せない）。空の文字列は不可。絶対 path で、`/` の直下でないもの。`/etc`、`/usr`、`/root`、`/tmp`、`/var/tmp`、`/run`、`/boot` などシステムの場所の下は不可。作るときにだけ使う。あるユーザーの home と違えば Blocked（MAM-PLAN-0146。home は動かさない） |
| `spec.groups` | string の配列 | 省略できる。補助グループ（グループ名の規則はユーザー名と同じ。重複は不可。ユーザー自身の名前のグループは不可: `useradd` がそれを主グループとして作り、その名前のグループがあると作らないので、ユーザーが作られなくなる）。Mamela は足りないグループに入れ（`usermod --append --groups`）、**外さない**。宣言にないグループに入っていれば、plan が警告する（MAM-PLAN-0148）。メンバーが root になれるグループ（`sudo`、`docker` など）は警告（MAM-GRAPH-0206） |
| `spec.authorizedKeys` | string の配列 | 省略できる（Mamela は `authorized_keys` に触れない）。あれば `~/.ssh/authorized_keys` は、この行だけを宣言の順に持つ。空の配列は不可（扱うのをやめるなら、メンバーを省く）。64 行まで。各行は `ssh-keygen` が書く公開鍵 `<type> <base64> [comment]`。type は `ssh-ed25519`、`sk-ssh-ed25519@openssh.com`、`ecdsa-sha2-nistp256`／`384`／`521`、`sk-ecdsa-sha2-nistp256@openssh.com`、`ssh-rsa`。base64 が復号でき、中の type が行の type と同じこと。空白は 1 つずつ。comment に制御文字は不可。行頭のオプション（`command=`、`from=`、`restrict` など）はこの版では不可。同じ鍵（fingerprint）の重複は不可 |

- **同じノードで:** 同じユーザーを 2 度宣言すると MAM-GRAPH-0102。2 人のユーザーが同じ `uid`、同じ home を持つ、home がほかのユーザーの home の中にある、`NodeFile`・`NodeArtifact` の path が home の中にあると MAM-GRAPH-0110。1 つのノードの `NodeUser` は 64 人まで。
- **ほかのユーザーの home を取らない:** `useradd --create-home` は、home にディレクトリがすでにあれば、警告するだけでそのまま使い、所有者を変えない（shadow-utils 4.17 で確かめた）。そこで mamelet は、ないユーザーの作る home にすでに何かあるかと、その所有者を報告し、plan は Blocked にする（MAM-PLAN-0146。「exists already (a directory, 0750 bob:bob)」）。mamelet も、作る前にもう一度確かめて断り、`useradd` のあと home がそのユーザーのものでなければ、その中に何も書かずに断る。作る home の上のディレクトリ（あるもの。リンクはたどる）を root 以外のユーザーが変えられるとき（そのユーザーのもの、root でないグループか誰でも書けるもの）も、mamelet はそれを報告し（`exposed`）、plan は Blocked にし（MAM-PLAN-0146）、mamelet も `useradd` を実行しない。そのユーザーは、mamelet が見てから `useradd` が作るまでの間に、自分のディレクトリやリンクをそこに置ける。
- **承認:** ないユーザーは `linux.create-user`（MAM-POL-0221）。plan の diff に、ユーザー名、`system`、uid、shell、home（作るか）、ユーザーの名前のグループ、補助グループ、`authorized_keys` の sha256 と、足す鍵（type、SHA256 の fingerprint、comment）が出る。あるユーザーは、宣言と違うときに `linux.change-user`（MAM-POL-0222）。diff に、補助グループの前とあと、shell、`authorized_keys` の sha256 の前とあと、足す鍵・外す鍵（fingerprint）、Mamela が鍵として読まない行、ファイル（0600、ユーザーのもの）と `~/.ssh`（0700、ユーザーのもの）の mode と所有者の変化が出る。鍵を外すときは、その鍵でのログインができなくなると警告する（MAM-PLAN-0149）。in sync は、宣言したグループにすべて入っていて、宣言した shell で、`authorized_keys` が宣言の内容（sha256）・0600・ユーザーのもので、`~/.ssh` が 0700・ユーザーのものであるとき。
- **待つもの:** 宣言したグループや shell がノードになく、同じノードの `NodePackage` がまだ入っていなければ、待つ（MAM-PLAN-0147。`docker-ce` が `docker` グループを作るように）。入っていれば Blocked で、作るパッケージを宣言するよう示す。所有者がまだない `NodeFile` は、同じ宣言の `NodeUser` が作るユーザー（とその名前のグループ）なら、それを待つ（MAM-PLAN-0143）。一つの plan の操作は kind の順に走り、`NodeUser` は `NodeFile` より先である。
- **実行:** mamelet が、承認のあと一度だけ、承認したとおりに行う。決まったプログラム（`/usr/sbin/useradd`、`/usr/sbin/usermod`）だけを、検査した値だけで、名前を `--` のあとに置いて実行する。作るときは `useradd [--system] [--create-home] [--uid N] --shell S [--home-dir H] --user-group -- name`、そのあと `usermod --append --groups g1,g2 -- name`。前の試みが `useradd` のあとで止まっていれば（ユーザーが承認どおりにある）、作らずに残りを続ける。承認したものと違うユーザーがすでにあれば断る（この変更が作るものとまったく同じでないもの: uid、system か〔`/etc/login.defs` の UID_MIN〕、ユーザーの名前の主グループ、shell、home とその所有者、変更が足さないグループ、承認していない `authorized_keys`。plan のあとで人が作ったユーザーを乗っ取らない）。root（名前でも uid 0 でも）、宣言にないグループや鍵、ないグループや shell、ほかのユーザーの uid、ユーザーの名前のグループがすでにあるときも、何も変えずに断る。終わったら、もう一度観測して確かめる。
- **鍵を書く:** home は `/etc/passwd` のものを、graph と同じ規則（`CheckHome`）でもう一度確かめる。home はそのユーザーのもの、`~/.ssh` はそのユーザーか root のもの（mamelet が作って、まだ渡していないもの）でなければ、書かず、chown もしない（ほかのユーザーの home。mamelet は報告し、plan は Blocked。MAM-PLAN-0212）。所有者は、開いたディレクトリから読む。home はユーザーのもので、ユーザーはそこに何でも置けるので、home と `~/.ssh` は 1 段ずつ開き、どれもシンボリックリンクでないことを確かめる（`~/.ssh` が `/etc` へのリンクなら断る）。`~/.ssh` がなければ 0700 で作り、ユーザーのものにする。`authorized_keys` は、同じディレクトリの一時ファイルに書き、fsync、chown、chmod 0600、rename で置き換える。mamelet は鍵の fingerprint だけを報告し、comment は送らない。同じ鍵は 1 度、65 個までで、残りは数だけを報告する（plan は「N more keys」として外す鍵に数える）。鍵として数えるのは、型ごとの形（Ed25519 は 32 バイト、ECDSA は曲線と点の大きさ、RSA は 1024〜16384 ビット）を持つものだけ。リンクがあれば plan は Blocked（MAM-PLAN-0212）。
- **消さない:** 宣言から外したユーザーは、グループ・home・鍵とともにノードに残る。plan はそれを MAM-PLAN-0311（retained）で示し、apply のあと mamelet は観測しなくなる。消すのは手で（`userdel`）。
- **この版でしないこと:** パスワード、sudo の設定（`/etc/sudoers.d`）、グループを作ること（グループはパッケージが作るか手で作る）、グループから外すこと、uid・home を変えること、鍵のオプション。
- **mamelet の版:** 0.10.0 より前の mamelet は linux を受け取らない。そのノードの `NodeUser` は Blocked（MAM-PLAN-0122）。

### NodeAptRepo

ノードの APT のソースと、それに署名する鍵（linux 拡張、ADR 0016）。宣言は Mamela Pome（0.4.0 から）の `Mamela.apt_repo(graph, node, name, uri, suite, components, key_url, key_sha256)`（URI と suite が 1 つ、ノードのすべてのアーキテクチャ）、または `Mamela.apt_repo_with(graph, node, Mamela.AptRepo(architectures: ["amd64"], ..Mamela.apt_repo_of(name, uri, suite, components, key_url, key_sha256)))`。鍵の sha256 を `mamela.sum` に任せるときは `Mamela.apt_repo_pinned(graph, node, name, uri, suite, components, key_url)`（0.4.1 から。`key_sha256` が `""` の `AptRepo` も同じ）。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/apt-repo/<name>`（`web/apt-repo/docker`） |
| `spec.node` | string | 同じ Graph のノード（provider のマシン） |
| `spec.name` | string | 小文字・数字・`-` の 1〜40 文字で、小文字か数字で始まる。ファイルの名前になる（`mamela-<name>.sources`、`mamela-<name>.asc`） |
| `spec.uris` | string の配列 | 1〜4 個。`http://` か `https://`、ホスト（英数字と `.` `-`。IPv6 のアドレスは不可）、ポート（省略可）、パス（英数字と `. _ ~ + -`、`%XX`。`.` と `..` の部分は不可）だけ。ユーザー名・パスワード（平文で宣言と plan に残る。認証はノードの `/etc/apt/auth.conf.d` に置く）、クエリ、フラグメント、apt のほかの方式（`file:`、`mirror+` など）は不可。http でもよい: apt は、取得するファイルをすべて鍵の署名で確かめる（盗み見と、古い内容を見せ続けることまでは防げないので、https を選べるならそちら） |
| `spec.suites` | string の配列 | 1〜8 個。`noble`、`bookworm-updates`、`stable/updates` のような名前（英数字と `. _ + ~ -`、`/` で区切る。`.` と `..` の部分は不可）。`/` で終わるものは flat なリポジトリのパス（`./`、`/`、`stable/`） |
| `spec.components` | string の配列 | 0〜16 個（`main`、`stable` など）。flat なリポジトリ（suite がすべて `/` で終わる）では空で、それ以外では 1 つ以上。`/` で終わる suite と終わらない suite は混ぜられない |
| `spec.architectures` | string の配列 | 省略できる（ノードのすべてのアーキテクチャ。空の配列も同じで、正規形では書かない）。dpkg のアーキテクチャ名（`amd64`、`arm64`）を 8 個まで |
| `spec.keyUrl` | string | `https://` だけ（512 バイトまで）。ユーザー名・パスワード、フラグメントは不可。クエリは書ける（鍵サーバーの検索） |
| `spec.keySha256` | string | 取得したままの鍵ファイルの sha256（小文字の 16 進 64 桁。取得したファイルを `sha256sum` したもの）。省略すると、`mamela.sum` の `keyUrl` の sha256 になる |

- **行を足せない:** 値は狭い形だけを通す。空白・改行・制御文字・`#`・`$`・`[` を含められないので、どの値も、ソースのファイルに行やオプション（別の `Signed-By`、`trusted=yes`）を足せない。それぞれの配列で同じ値を 2 度書けない。
- **同じソースを 2 度宣言しない:** 同じノードの 2 つの NodeAptRepo が同じ URI（末尾の `/` は区別しない）と suite を持つと MAM-GRAPH-0110。apt は 1 つのソースに 2 つの鍵を認めず、すべてのソースを読まなくなるため。`NodeFile` は apt のソースのファイルを書かないので、宣言のファイルと衝突することはない（ノードにあるソースのファイルとの衝突は、mamelet が見つける。下の「ほかのソースとの衝突」）。
- **ディストリビューション自身のソースは宣言できない（この版の制限）:** ミラーを変える、`non-free-firmware` などの component を足す、のように、ディストリビューションのソース（`/etc/apt/sources.list`、`/etc/apt/sources.list.d/debian.sources`・`ubuntu.sources`）を変えることは、今は手で行う。`NodeFile` はそれを書かず、同じ URI と suite の NodeAptRepo は、ディストリビューションのファイルと衝突する（mamelet が断る。apt は 1 つのソースに 2 つの鍵を認めない）。ADR 0016 の残り。
- **書くもの:** 鍵は `/etc/apt/keyrings/mamela-<name>.asc`（ASCII armor の鍵）か `/etc/apt/keyrings/mamela-<name>.gpg`（バイナリの鍵）、ソースは `/etc/apt/sources.list.d/mamela-<name>.sources`（deb822）。どちらも 0644、root の所有で、`/etc/apt/keyrings` がなければ 0755 で作る。`mamela-*` の名前は NodeAptRepo のもので、`NodeFile` は書けない。ファイルは Go の 1 つの関数（`AptSources`）が描き、Control Plane（plan の表示と、比べる sha256）と mamelet（書き込み）が同じものを使う:

  ```
  # Mamela's APT source docker (NodeAptRepo): change the declaration, not this file.
  Types: deb
  URIs: https://download.docker.com/linux/ubuntu
  Suites: noble
  Components: stable
  Signed-By: /etc/apt/keyrings/mamela-docker.asc
  ```

- **鍵の形:** 鍵を取得するまで、Control Plane には、鍵が armor かバイナリかが分からない。plan は armor の鍵（`.asc`）として全文を示し、バイナリなら `Signed-By` が `.gpg` を指すことを添える。違いはこの 1 か所で、鍵は sha256 で固定されているので、承認した中身は 1 つに決まる（鍵がノードにあれば、plan はその形で示す）。mamelet は、`-----BEGIN PGP PUBLIC KEY BLOCK-----` で始まり（前の空白は許す）終わりの行がある鍵を armor、最初のバイトが OpenPGP の公開鍵のパケット（tag 6。旧形式の 0x98・0x99・0x9a、新形式の 0xc6）のものをバイナリとし、それ以外（エラーの HTML、秘密鍵、GnuPG の keybox）は断る。
- **承認:** `linux.add-apt-repo`（MAM-POL-0223）。plan の diff に、ソースのファイルの全文と sha256（古いものは sha256 だけ）、鍵の URL と sha256、鍵のファイル、消すもう一方の形の鍵、作るディレクトリ、`apt-get update` が出る。判定の文は、このソースのパッケージがシステムのパッケージを置き換えうること（apt はどのソースのものでも版の高いものを入れる）と、その maintainer script が root で動くことを言う。
- **in sync:** 鍵が 2 つのファイルのどちらかに宣言した sha256 であり、ソースのファイルがその鍵の形で描いたものと同じで、どちらも 0644・root の所有、もう一方の形のファイルがなく、apt が各 URI と suite のリリースファイル（`/var/lib/apt/lists` の `InRelease` か `Release`。apt-get update が読んで確かめたもの）を持つとき。
- **mamelet の動き:** 承認のあと 1 回だけ（試みごと）。revision から描き直したファイルが承認した sha256 と合わなければ、その部分ごと断る。ノードのほかのソースのファイル（`/etc/apt/sources.list`、`sources.list.d` の `.list` と `.sources`）が同じ URI と suite を持てば断る。path とその上のリンクはたどらない。鍵は、承認した sha256 のものがノードにあればそれを使い、なければ https で取得する（64 KiB まで、30 秒、リダイレクトも https だけ）。sha256 と形を確かめるまで何も書かない。鍵、ソースの順に書き（同じディレクトリの一時ファイルから rename）、もう一方の形の鍵を消し、`apt-get update` を、核がパッケージを入れるのと同じ `/usr/bin/apt-get`、環境（`DEBIAN_FRONTEND=noninteractive`）、ロックの待ち（`-o DPkg::Lock::Timeout=300`）で走らせる（10 分まで）。途中で止まった試みは、済んだ手順を飛ばして、同じ試みでやり直す。
- **apt-get update の失敗:** 結果は failed で、apt の言葉（末尾の数行、1000 バイトまで）を示す。`apt-get update` は、ソースを取得できなくても `W:` を出して 0 で終わるので、mamelet は、終わったあとに、apt がこのソースの URI と suite を読んだか（リリースファイル）と、apt がそれについて言ったこと（`Err:`、`E:`、`W: Failed to fetch`、`W: GPG error`）を見る。読めていない（または前に読んだものを持ち続けるだけ）なら failed にし、それらの行を理由に入れる（ほかのソースの失敗や、失敗でない警告では failed にしない）。ファイルは残す。apt はまだソースを読んでいないので、plan は、ファイルは揃っているが update が要る変更として示す（MAM-PLAN-0152。diff は「apt source」と「apt-get update」だけで、最初の plan のものと同じ）。前の試みの理由は、その許可が revision にあるあいだ、mamelet が報告し、0152 のメッセージに出る。原因を直して、承認した plan をもう一度 `mamela apply` すると update をやり直す（attempt 2。鍵はノードのものを使い、取得し直さない）。新しい plan を承認してもよい。
- **パッケージは待つ（`PackageGate`）:** そのノードの `NodePackage` の導入は、そのノードの NodeAptRepo が置かれ、apt がそれを読むまで待つ（MAM-PLAN-0151）。どのパッケージがどのソースから来るかは、apt がすべてのソースを読むまで分からないので、すべてのパッケージを待たせる（入っているパッケージは関係ない）。待たせるのは、ソースの plan がファイルを書くか、ソースが待つ（報告、mamelet の更新、観測の場所）あいだ。Blocked のソースは待たせない（自分の問題を示す）。ファイルは揃っているが apt がまだ読んでいないソース（MAM-PLAN-0152。apt-get update の失敗）は、apt が見つけられないパッケージ（`Unable to locate package`、`has no installation candidate`、依存が `not installable`）だけを待たせる。そのソースにあるかもしれないからで、Blocked（MAM-PLAN-0202）にすると、update をやり直す plan を mamelacd の `sync = "push"` が承認しない。すべてのソースを読んでも見つからなければ MAM-PLAN-0202。`ca-certificates` は待たせない（mamelet は鍵を、ノードの証明書で https から取得する）。mamelet は、何かを変えた周の中でパッケージを観測し直すので、ソースを足した周の報告に、そのソースのパッケージの変更集合（apt-get -s）がもう入っている。パッケージの導入は次の段の plan になる: mamelacd の `sync = "push"`（ADR 0014）なら次の poll で承認・apply され、`"approval"` と手の運用では、その plan をもう一度承認する。
- **origin:** ソースを足しても、そのパッケージは、policy の `allowedAptOrigins`（既定は `Ubuntu:`）と、ノードの mamelet の `allowedOrigins`（`/etc/mamelet/config.json`。マシンを作るときに既定の policy から書かれる）の両方が、その origin（plan の apt の行に出る。`Docker CE:noble` など）を許すまで入らない（plan は MAM-POL-0305 で拒み、mamelet も断る）。どちらにも、確かめたうえで足す。mamelet の設定は、今は手で書き換えて mamelet を再起動する（Mamela はまだ変えない）。
- **ほかのソースとの衝突:** ノードのほかのソースのファイル（ベンダーの手順で足したもの、パッケージが置いたもの）が同じ URI と suite を持つと、plan は Blocked（MAM-PLAN-0153）。apt は 1 つのソースに 2 つの鍵（`Signed-By`）を認めず、すべてのソースを読まなくなる（セキュリティ更新も止まる）からである。そのファイルを手で消してから plan する。
- **リンクをたどらない:** ソース・鍵の path かその上のディレクトリがリンク、path がディレクトリなら Blocked（MAM-PLAN-0214）。観測できなければ MAM-PLAN-0215。
- **読めないソースのファイル:** ノードのほかのソースのファイルがリンクなら、mamelet は（見るだけで）たどって行き先を読む（ディレクトリは root のもの）。行き先がない、通常のファイルでない、1 MiB を超えるなど、読めないものがあれば、衝突を確かめられないので判断せず、Blocked（MAM-PLAN-0215）。mamelet も何も書かずに断る（fail closed）。
- **apt のリストのロック:** `apt-get update` は `/var/lib/apt/lists/lock` を待たずに失敗する（`DPkg::Lock::Timeout` は dpkg のロックのためで、update はそれを取らない。apt 3.0 で確かめた）。apt-daily などがロックを持っている間、mamelet は 5 秒ごとに、5 分まで、やり直す。
- **数:** 1 つのノードの `NodeAptRepo` は 32 個まで。
- **消さない:** 宣言から外したソースと鍵は、ノードに残り、apt はそれを読み続ける。plan は MAM-PLAN-0312（retained）で示し、apply のあと mamelet は観測しなくなる。消すなら手で（2 つのファイルを消して `apt-get update`）。
- **mamelet の版:** `NodeFile` と同じく 0.10.0 から（それより前は MAM-PLAN-0122）。

例（Docker の Ubuntu 24.04 向けのソースと docker-ce。**sha256 は例の値で、本物ではない**。使う前に鍵を取得して `sha256sum` で求め、Docker の文書の鍵の指紋と照らす。`web` は、プロバイダーの Pome が返す `Mamela.Node`）:

```emela
Mamela.empty()
  |> Mamela.apt_repo(
    web,
    "docker",
    "https://download.docker.com/linux/ubuntu",
    "noble",
    ["stable"],
    "https://download.docker.com/linux/ubuntu/gpg",
    "0000000000000000000000000000000000000000000000000000000000000000",
  )
  |> Mamela.package(web, "docker-ce")
```

最初の plan はソースを足し（承認が要る）、docker-ce は MAM-PLAN-0151 で待つ。apply のあとの plan が docker-ce の変更集合を示す。Docker のパッケージの origin（`Docker CE:noble`）を policy の `allowedAptOrigins` とノードの mamelet の `allowedOrigins` に足すまで、その導入は拒まれる。

### NodeAptUpgrades

ノードに入っているパッケージの、保留中の更新のうち、宣言した APT の origin から来るもの（Debian のセキュリティ・アーカイブなど）を、承認のたびに、plan が示した版のとおりに上げる（linux 拡張、ADR 0016 の §9）。宣言は Mamela Pome（0.5.0 から）の `Mamela.apt_upgrades(graph, node, origins)`、または Debian のセキュリティ更新だけの `Mamela.debian_security_upgrades(graph, node)`（`origins` が `["Debian-Security:"]`）。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/apt-upgrades`（`web/apt-upgrades`）。1 つのノードに 1 つ |
| `spec.node` | string | 同じ Graph のノード（provider のマシン） |
| `spec.origins` | string の配列 | 1〜8 個、同じものは 2 度書けない（順は意味を持たず、正規形では並べる）。apt-get -s が版について示す origin（`<label>:<version>/<suite>`。policy の `allowedAptOrigins` と同じ形）か、その先頭で、少なくともラベルと `:` まで（`Debian-Security:` は Debian のセキュリティ・アーカイブ、`Debian:` は point release と stable-updates、`Ubuntu:24.04/noble-security` は Ubuntu のセキュリティ。`Deb` のようなラベルの途中までは不可）。英数字と `. _ + ~ -`、ラベルの中の空白（`Docker CE:`）、`:` のあとは英数字と `. _ + ~ : / -` だけ。100 バイトまで |

- **何を上げるか:** 入っているパッケージで、候補（candidate）の版がいまより高く、その候補の origin のどれかが宣言の origin に当たる（先頭が一致する）もの。mamelet は `apt-get -s dist-upgrade`（シミュレーション、apt のロックなし）の `Inst` 行から候補とその origin を読み、当たるものを候補の版に固定して（`<package>=<version>`）`apt-get -s install --only-upgrade` でシミュレーションする。その `Inst`・`Remv` 行が変更集合で、plan は 1 行ずつ示す（`upgrade bind9-libs 1:9.20.11-4 -> 1:9.20.15-1~deb13u1 (Debian-Security:13/stable-security)`。依存で新しく入るものは `install`、消えるものは `remove`）。変更集合の digest も示し、承認はその digest に結び付く。point release のあとは、多くのセキュリティ更新が main のアーカイブにもあり、apt は両方の origin（`Debian:13.6/stable, Debian-Security:13/stable-security`）を示す。どちらかが宣言に当たれば対象にする。セキュリティと point release の両方を上げるなら、`origins` に `Debian-Security:` と `Debian:` を書く。
- **policy:** 承認が要る（MAM-POL-0227）。変更集合に削除があれば MAM-POL-0304、変更集合のどの行の origin も、宣言の `origins` そのものも、policy の `allowedAptOrigins` が許していなければ MAM-POL-0305（パッケージの導入と同じ規則。1 つの版が 2 つの origin にあれば、両方を許していなければならない。Debian なら `Debian:` と `Debian-Security:`）。observe-only のノードでは示すだけで、MAM-POL-0312 で保つ。
- **mamelet の動き:** 承認のあと 1 回だけ（試みごと）。承認したパッケージと版で、もう一度シミュレーションし、変更集合の digest が承認したものと同じで、削除がなく、すべての origin を mamelet の `allowedOrigins` が許すときだけ、`apt-get -o DPkg::Lock::Timeout=300 -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold install --only-upgrade <package>=<version> ...` を実行する（変えた設定ファイルは残す）。違えば何もせずに断り（refused、「plan again」）、次の plan が新しい変更集合を示す。すでに承認した版になっていれば（中断した試み、手での更新）、何もせずに done。上げる途中で失敗すれば failed（apt の言葉）で、上がったものは残り、次の plan が残りを示す（MAM-PLAN-0168 が前の試みの失敗を言う）。`apt-get install` は、mamelet が止められても途中で kill しない（dpkg を途中で止めると、パッケージが半端に残る）。30 分で打ち切る。
- **再起動しない:** パッケージの maintainer script は、自分のサービスを再起動する（nginx など）。Mamela はほかに何も再起動しない。ノードが再起動を求めていれば（`/run/reboot-required`）、plan は警告 MAM-PLAN-0169 で示す（情報だけで、状態も操作も変えない）。再起動は、プロバイダーの明示操作（`mamela act`）で人が行う。
- **一覧の古さ:** plan は、使ったパッケージの一覧（`/var/lib/apt/lists`）の古さを示す（MAM-PLAN-0168）。古さは、mamelet の最後の `apt-get update`（すべてのソースを読めたもの）と、apt の最も新しいリリースファイル（`*_InRelease`、`*_Release`）の mtime の、新しいほうから測る（apt は、取得したファイルに、サーバーが言う最終更新の時刻を付けるので、mtime は「持っている最も新しい索引がいつのものか」である）。12 時間より古い一覧からは plan しない: mamelet が読み直すのを待つ（MAM-PLAN-0167）か、読み直せなければ Blocked（MAM-PLAN-0166。mamelet の `apt-get update` が言ったことの抜粋と、その時刻）。
- **mamelet が自分で `apt-get update` する:** 一覧が 12 時間より古ければ、mamelet は承認なしに `apt-get -o DPkg::Lock::Timeout=300 update` を 1 回実行する（Debian・Ubuntu の apt-daily が 1 日 2 回するのと同じ）。承認なしに行う、ただ 1 つのことである。変えるのは apt が「何を入れられるか」の一覧（`/var/lib/apt/lists`）だけで、入っているものは変えない。そのノードの宣言が `NodeAptUpgrades` を持つときだけ（revision の部分の `refresh`）、Mamela が変えないノード（observe-only、Control Plane のホスト）では行わない。承認した更新が待つあいだも行わない（承認した版が新しい一覧から消えることがある）。失敗したら（ソースを読めない `Err:`・`W: Failed to fetch`・`E:` を含む）、その理由を状態ディレクトリ（`linux-apt-refresh.json`）に記録して報告し、1 時間は試さない。ほかの apt がリストのロックを持っていれば、失敗とせず次の周に試す。理由は ADR 0016 の §9。
- **dpkg が途中で止まっている:** `/var/lib/dpkg/updates` に記録があれば（更新か導入が途中で切れた）、Blocked（MAM-PLAN-0226）。`dpkg --configure -a` を手で行う。
- **計算できない:** シミュレーションが失敗した（保留にしたパッケージ、入れられない依存）、上げるパッケージが 1000 を超える（リリースの更新は手で）なら Blocked（MAM-PLAN-0225）。
- **宣言から外す:** mamelet は計算も一覧の読み直しもやめる。上げたパッケージは、そのまま残る。
- **mamelet の版:** `NodeFile` と同じく 0.10.0 から（それより前は MAM-PLAN-0122）。

例（Debian 13 のマシン `web` のセキュリティ更新。policy の `allowedAptOrigins` とノードの mamelet の `allowedOrigins` に `Debian-Security:` と `Debian:` があること）:

```emela
Mamela.empty()
  |> Mamela.debian_security_upgrades(web)
```

plan の例:

```text
~ NodeAptUpgrades web/apt-upgrades  [needs approval]
      pending upgrades: 3 from Debian-Security: -> 0
      changeSet: sha256:de1702408...
      apt: upgrade bind9-dnsutils 1:9.20.11-4 -> 1:9.20.15-1~deb13u1 (Debian-Security:13/stable-security, Debian:13.6/stable)
      apt: upgrade bind9-libs 1:9.20.11-4 -> 1:9.20.15-1~deb13u1 (Debian-Security:13/stable-security)
      apt: upgrade libssl3t64 3.5.1-1 -> 3.5.1-1+deb13u1 (Debian-Security:13/stable-security)
    linux.upgrade-packages: needs approval (MAM-POL-0227 ...)
    MAM-PLAN-0168 the package lists on web are 41m old (mamelet's last apt-get update, 2026-11-17T11:20:00Z)
```

### NodeArtifact

ノードの上の 1 つのファイルを、https のダウンロードから、sha256 で固定して置く（linux 拡張、ADR 0016）。リリースのバイナリ、または `.tar.gz` の中の 1 つのファイル。宣言は Mamela Pome（0.4.0 から）の `Mamela.artifact(graph, node, path, url, sha256)`（ダウンロードそのもの、mode 0755、root の所有、再起動なし）、または `Mamela.artifact_with(graph, node, Mamela.Artifact(extract: "app-1.0/app", restart: ["app"], ..Mamela.artifact_of(path, url, sha256)))`。sha256 を `mamela.sum` に任せるときは `Mamela.artifact_pinned(path, url)`（0.4.1 から。`sha256` が `""` の `Artifact` も同じ）。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/artifact<path>`（`web/artifact/usr/local/bin/node_exporter`） |
| `spec.node` | string | 同じ Graph のノード（provider のマシン） |
| `spec.url` | string | `https://` の URL（2048 バイトまで）。ホストがあり、ユーザー・パスワード（`user@`）と fragment（`#`）はない。文字は RFC 3986 のものだけ（空白・制御文字なし）。`http://` は拒否 |
| `spec.sha256` | string | ダウンロードしたバイト列の sha256（小文字 16 進 64 桁）。リリースのチェックサムファイル（`sha256sums.txt` など）に載っている値。省略すると、`mamela.sum` の `url` の sha256 になる |
| `spec.path` | string | 置く場所。`NodeFile` の `path` と同じ規則（書かない場所も同じ） |
| `spec.extract` | string | 省略できる。書くと、ダウンロードは gzip で圧縮した tar で、その中のこのメンバー（通常のファイル）だけを置く。`tar -tzf` が示す名前で、相対・`Clean` したもの（先頭の `/` と `./`、`..`、末尾の `/` なし）。各部分は英数字と `. _ + @ = : , -` だけ。1024 バイトまで |
| `spec.mode` | string | 省略できる（既定 `"0755"`）。`NodeFile` と同じ規則 |
| `spec.owner`、`spec.group` | string | 省略できる（既定 `"root"`）。`NodeFile` と同じ。ノードにないと Blocked（MAM-PLAN-0156）、同じ宣言の NodeUser が作るなら待つ（MAM-PLAN-0158） |
| `spec.restart` | string の配列 | 省略できる。置いたあと、動いていれば再起動するサービス（`try-restart`。unit がまだないサービスは何もしない。1 周の中で artifact は unit のファイルより先に置かれる）。宣言していないサービスは警告（MAM-GRAPH-0215） |

- **同じ path:** 同じノードで、`NodeArtifact` と `NodeFile` が同じ path を宣言するもの、一方がもう一方の上のディレクトリになるもの、`NodeUser` の home の中にあるものは、MAM-GRAPH-0110。1 つのノードの `NodeArtifact` は 64 個まで。`restart` に `mamelet` は不可（`NodeFile` と同じ）。
- **観測:** mamelet は、置いたファイルの sha256 を、ファイルの (デバイス, inode, 大きさ, mtime, ctime) が変わったときだけ計り直す（mamelet のメモリに持つ。再起動のあとは 1 度計る）。中身を変えれば ctime が変わり、ctime はユーザーが戻せない。
- **in sync:** path のファイルが、mamelet が宣言した sha256 のダウンロード（と `extract` のメンバー）から置いたままのもので、mode・owner・group が宣言と同じとき。展開したメンバーの sha256 はダウンロードの sha256 と違うので、mamelet は自分の状態ディレクトリ（`stateDir`）に、置いたものの記録（`linux-artifacts.json`。path ごとに、URL、ダウンロードの sha256、メンバー、置いたファイルの sha256）を持つ。ファイルの sha256 が記録と同じときだけ、mamelet は記録の URL・sha256・メンバーを報告する。URL だけが違う（ミラー）ものは、同じダウンロードなので in sync。
- **承認:** `linux.install-artifact`（MAM-POL-0225）。plan の diff に、path、URL、sha256、`extract`、mode・owner・group の変化、作るディレクトリ、unit なら `daemon-reload`、再起動するサービスが出る。前の状態は diff の「from」に出る。なければ新規（`+`）、mamelet が別のダウンロードから置いたものなら、その URL・sha256・メンバー、Mamela が置いたものでない（手で変えた、ほかの方法で置いた）なら、そのファイルの sha256 と MAM-PLAN-0159 の注記（置き換える）。mode・owner だけが違うときは、mamelet はダウンロードし直さない。
- **置き方:** mamelet が、承認のあと一度だけ行う。path の規則と、path とその上がシンボリックリンクでないことを確かめ、URL を https だけで（リダイレクトも https だけ、10 回まで）、時間（10 分）と大きさ（512 MiB）を限ってダウンロードする。状態ディレクトリの一時ファイル（mamelet だけが読める）に、sha256 を計りながら書き、sha256 が承認したものでなければ、何も置かずに断る。`extract` なら tar を最後まで読み、そのメンバーがちょうど 1 つあり、通常のファイル（512 MiB まで）であることを確かめて取り出す（リンク・ディレクトリ・デバイス、ないもの、2 つあるものは断る。展開した全体は 4 GiB まで）。そのあと `NodeFile` と同じく、同じディレクトリの一時ファイルに書いて rename で置き換え、置いたファイルの sha256 と mode・所有者を確かめ、記録を書き（一時ファイルと rename）、unit なら `daemon-reload`、`restart` のサービスを `try-restart` する。一時ファイルは、成功しても失敗しても消す。途中で止まった（mamelet が殺された）ダウンロードは、次の試みが消して最初からやり直す。外部のプログラムは使わない（Go の `net/http`、`compress/gzip`、`archive/tar`）。ダウンロード（大きさが分かれば）、展開、置く前に、ファイルシステムの空きを確かめ、ファイルと余白（64 MiB）がなければ断る。止まった試みの一時ファイル（状態ディレクトリの `.linux-artifact-*`、置き場所の `.mamelet-*.tmp`）は次の周に消す。書いたあとの `daemon-reload` と再起動は `NodeFile` と同じ（終わらなければ MAM-PLAN-0218）。
- **消さない:** 宣言から外したものは、ノードにそのまま残る（MAM-PLAN-0313、retained）。apply のあと mamelet は観測しなくなる。
- **mamelet の版:** `NodeFile` と同じ（0.10.0 から。MAM-PLAN-0122）。

例（Prometheus の node_exporter を、リリースの `.tar.gz` から置き、`NodeFile` の unit で動かす）。**sha256 は例のための仮の値**で、本当の値はリリースの `sha256sums.txt` から写す。

~~~emela
import Mamela

const UNIT = "[Unit]\nDescription=Prometheus node exporter\nAfter=network-online.target\n\n[Service]\nExecStart=/usr/local/bin/node_exporter\nDynamicUser=yes\nRestart=on-failure\n\n[Install]\nWantedBy=multi-user.target\n"

pub fn node_exporter(graph: Mamela.Graph, node: Mamela.Node) -> Mamela.Graph {
  dir = "node_exporter-1.8.2.linux-amd64"
  exporter = Mamela.Artifact(
    extract: "#{dir}/node_exporter",
    restart: ["node_exporter"],
    ..Mamela.artifact_of(
      "/usr/local/bin/node_exporter",
      "https://github.com/prometheus/node_exporter/releases/download/v1.8.2/#{dir}.tar.gz",
      "0000000000000000000000000000000000000000000000000000000000000000",
    ),
  )
  graph
    |> Mamela.artifact_with(node, exporter)
    |> Mamela.file_with(
      node,
      Mamela.File(
        restart: ["node_exporter"],
        ..Mamela.file_of("/etc/systemd/system/node_exporter.service", UNIT),
      ),
    )
    |> Mamela.service(node, "node_exporter", True, True)
}
~~~

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

## 配布物の sha256 と `mamela.sum`

宣言は、ノードが取得するものの sha256 を書かずに、取得先の URL だけを書ける（[設計](design/declaration-inputs.md) の 2）。sha256 は、宣言の Pome のルート（`Pome.toml` の隣）の `mamela.sum` に置き、`mamela pin` が書く。go.mod と go.sum と同じ分け方で、人は版と URL を書いて読み、sha256 は道具が書いて、レビューではその差分を見る。

| kind | 省略できるメンバー | `mamela.sum` のキー（URL） | Pome の関数 |
|---|---|---|---|
| `NodeArtifact` | `sha256` | `url` | `Mamela.artifact_pinned(path, url)` |
| `NodeAptRepo` | `keySha256` | `keyUrl`（署名鍵） | `Mamela.apt_repo_pinned(...)` |
| `NodeK3s` | `sha256` | リリースの linux/amd64 のバイナリ（k3s 拡張が版から作る） | `K3s.server_pinned`、`K3s.agent_pinned` |

`NodeMamelet` は公開の URL がないので、これまでどおり宣言に sha256 を書く。

- **形:** 1 行に 1 つ、`<url> sha256:<小文字 16 進 64 桁>`、URL のバイトの順、改行は LF、最後の行も LF で終わる。1 行目に限り、`mamela pin` が書く見出しの行（`` # Written by `mamela pin`: the sha256 of what this declaration downloads. Do not edit. ``）を置ける。それ以外のコメント、空行、同じ URL の 2 行、順序の違い、別の形の行、https でない URL は、すべて拒む（`MAM-EVAL-0602`）。シンボリックリンクも拒む。4 MiB まで。
- **評価:** Go が、Graph を検証する前に、省略された sha256 を `mamela.sum` から埋める。正規形と digest は、宣言に sha256 を書いたときと 1 バイトも違わない。`mamela.sum` にない URL があれば、検証のあと（Graph が正しいとき）に、その URL とリソースと「`mamela pin <pome-dir>` を実行する」を示して止める（`MAM-EVAL-0601`）。評価はネットワークを使わず、`mamela.sum` を書き換えない。宣言に sha256 を書いたものは、それを使い、`mamela.sum` を見ない。使われていない行があっても評価は通る（`mamela pin --check` が示す）。
- **保存した Graph:** 正規形には必ず sha256 があるので、保存した Graph（`internal/store`）を読み直すときに `mamela.sum` は要らない。sha256 のない文書を `mamela.sum` なしで読むと、これまでどおり `MAM-GRAPH-0004`。
- **古いバイナリ:** sha256 を省略した文書を、この機能より前のバイナリで評価すると `MAM-GRAPH-0004`（メンバーがない）で拒む。黙って違うものを入れることはない。Control Plane を先に更新する。
- **`mamela pin [--check | --verify] <pome-dir>`:** 宣言を評価して（このときは `mamela.sum` を読まない）、sha256 の要る URL を集める。Graph が正しくなければ、何も取得しない。
  - 既定: `mamela.sum` にない URL だけを、https だけ（リダイレクトも https だけ、10 回まで）、512 MiB まで、10 分以内で取得し、バイト列の sha256 を書く。GitHub のリリースの asset（`https://github.com/<owner>/<repo>/releases/download/<tag>/<asset>`）は、GitHub の API が返す asset の `digest` とも照合する（`MAM-PIN-0103`。digest のない古い asset は、そう表示して照合しない）。使われなくなった行は消す。書き込みは、同じディレクトリの一時ファイルと rename で行い、読んだあとに `mamela.sum` が変わっていれば書かない（`MAM-PIN-0201`）。取得の失敗、拒否、照合の食い違いのどれでも、何も書かない。足した行と消した行を表示する。
  - `--check`: ネットワークを使わない。足りない行と使われていない行を示し、足りなければ終了コード 1（`MAM-PIN-0301`）。何も書かない。
  - `--verify`: `mamela.sum` のすべての行を取得し直して照合する（`MAM-PIN-0302`）。何も書かない。
- **信頼:** `mamela pin` が確かめるのは、取得したときのバイト列だけ（初めて見たものを信じる、TOFU）。守りの中心は、PR で版・URL・sha256 の差分を人が読むこと。署名鍵（`keyUrl`）を新しく固定したときは、`mamela pin` が鍵の指紋を配布元のものと照合するよう案内する。
- **mamelacd:** `mamela.sum` は宣言の Pome のツリーにあるので、mamelacd が Control Plane に送るツリー（GitHub の tarball）にそのまま入る。版を変えたら、`mamela pin` を実行し、宣言と `mamela.sum` を同じコミットにする（[gitops.md](gitops.md)）。

## 正規化と digest

- 正規形: リソースを kind の順（NodePackage → NodeService → NodeK3s → NodeK3sManifest → NodeMamelet とプロバイダーの kind を名前の順 → linux の kind。linux は NodeUser、NodeAptRepo、NodeArtifact、NodeFile、NodeAptUpgrades の順に 6〜10 を持つ）、次に名前の順に並べ、キーをソートし、空白なしで書いたもの。`after` は名前の順に並べて重複を除き、空なら書かない。
- digest: 正規形の SHA-256（`sha256:<hex>`）。宣言の順番を入れ替えても digest は変わらない。
- `pome/mamela` の出力は、リソースが kind 順に並んでいれば正規形と 1 バイトも違わない（テストで確認）。ただし `after` は宣言した順に書くので、名前の順でなければ、Go が並べ直してから digest を取る。

## 互換性

形を変えるときは `mamela-graph/v2` にする。v1 のまま意味を変えない。

### Pome の版と Mamela のバイナリ

Pome は kind と spec のメンバーを出し、Go の graph がそれを検査する。Pome が出すものを、バイナリが知らなければ拒む。新しい Pome を古いバイナリで読むと、知らない kind は MAM-GRAPH-0005、知らない member は MAM-GRAPH-0003 になる。逆に、新しいバイナリは古い Pome を読める。ただし Pome 0.1.0 と 0.2.0 は、ADR 0006 でなくなった `VirtualMachine` を出すので、その PR（#12）以降のバイナリは MAM-GRAPH-0005 で拒む。

Mamela のバイナリにはまだ版のタグがなく、`mamela version` は build した revision を出す。次の表の「要るバイナリ」は、その kind を最初に受け付けた PR のマージコミットで、`git merge-base --is-ancestor <コミット> <mamela version の revision>` が成り立つバイナリなら読める。

| Pome | 版 | その版で足したもの（出す kind） | 要るバイナリ |
| --- | --- | --- | --- |
| `pome/mamela` | 0.1.0 | `VirtualMachine`、`NodePackage`、`NodeService` | PR #12 より前のバイナリだけ（以降は MAM-GRAPH-0005） |
| `pome/mamela` | 0.2.0 | 上に、`NodeK3s` とプロバイダーの kind（`mamela-graph/v2`）。`VirtualMachine`（旧 `Vm` の API）も出す | PR #12 より前のバイナリだけ（以降は MAM-GRAPH-0005） |
| `pome/mamela` | 0.3.0 | `NodePackage`、`NodeService`、プロバイダーの kind（`provided`、`provided_after`、`on_node`） | PR #12（`d2c2d03`）以降 |
| `pome/mamela` | 0.3.1 | `NodeMamelet`（`Mamela.mamelet`） | PR #23（`37ab2b5`）以降 |
| `pome/mamela` | 0.3.2 | Pome の API は 0.3.1 と同じ。0.3.2 の Mamela（監査 2026-11-17 の修正、k3s を `internal/ext/k3s` へ）に合わせた版。graph の検証が厳しくなった（service 名の `:`、制御文字、optional の null などを拒否） | どの版でも読めるが、0.3.2 の修正は PR #36 以降のバイナリ |
| `pome/mamela` | 0.4.0 | linux 拡張（ADR 0016）の `NodeFile`（`Mamela.file`）、`NodeUser`（`Mamela.user`）、`NodeAptRepo`（`Mamela.apt_repo`）、`NodeArtifact`（`Mamela.artifact`）。ノードの mamelet が linux を受け取る版（0.10.0 以降）でなければ、その資源は MAM-PLAN-0122 | PR #39（`7ecf714`）以降。PR #49 までの修正を含むバイナリを使う。CP を先に、mamelet をあとに更新する（ADR 0016 §4） |
| `pome/mamela` | 0.4.1 | `NodeFile` の `spec.contentFrom`（`Mamela.file_from`、`Mamela.file_from_with`、`Mamela.systemd_service_from`）。部品の `Mamela.running`、`Mamela.system_user`、`Mamela.systemd_service` は今の kind を出すだけ（0.4.0 のバイナリで読める）。`contentFrom` を古いバイナリは MAM-GRAPH-0003 で断る。`sha256` のない `NodeArtifact`（`Mamela.artifact_pinned`）と `keySha256` のない `NodeAptRepo`（`Mamela.apt_repo_pinned`）は sha256 を `mamela.sum` から取る。古いバイナリは MAM-GRAPH-0004 で断る | `contentFrom` は PR #52（`065d791`）以降。`mamela.sum` は PR #53（`4ac3755`）以降 |
| `pome/mamela` | 0.5.0 | `NodeAptUpgrades`（`Mamela.apt_upgrades`、`Mamela.debian_security_upgrades`）。古いバイナリは MAM-GRAPH-0005 で断る。ノードの mamelet が linux を受け取る版（0.10.0 以降。この kind は、その 0.10.0 の linux の部分に足したもの）でなければ MAM-PLAN-0122 | この kind を足した PR 以降（マージのあとで、そのコミットを書く） |
| `pome/mamela` | 0.5.0 | `NodeFile` の `spec.check`（`Mamela.Check` の `Nginx`）と `spec.reload`（`Mamela.File` と `Mamela.FileFrom` の `check`・`reload`）。`File` と `FileFrom` にメンバーが増えたので、`..Mamela.file_of(...)` を使わずにすべてのメンバーを書いていた宣言は直す（互換のない変更）。古いバイナリは `check`・`reload` を MAM-GRAPH-0003 で断る。検査を受け取るのは、この変更を含む mamelet（古い mamelet は、`check` のある書き込みを、知らないメンバーとして linux の部分ごと断り、何も書かない） | この変更の PR 以降 |
| `pome/k3s` | 0.1.0 | `NodeK3s`（`K3s.server`、`K3s.agent`）。`pome/mamela` 0.3.0 に依存 | PR #12（`d2c2d03`）以降 |
| `pome/k3s` | 0.1.1 | `NodeK3sManifest`（`K3s.manifest`） | PR #18（`754c452`）以降 |
| `pome/k3s` | 0.1.2 | `sha256` のない `NodeK3s`（`K3s.server_pinned`、`K3s.agent_pinned`。`mamela.sum` から）、`contentFrom` の `NodeK3sManifest`（`K3s.manifest_from`）。依存は `pome/mamela` 0.3.0 のまま | PR #53（`4ac3755`）以降（`K3s.manifest_from` の `contentFrom` は PR #52（`065d791`）以降）。古いバイナリは MAM-GRAPH-0004 / 0003 で断る |

新しい kind や member を出す版を公開する PR は、この表に行を足す（[releasing.md](releasing.md)）。Emela は依存の中で最も高い版を選ぶので、`pome/k3s` が要求する `pome/mamela` の版を上げると、K3s を使う宣言すべてが、その版の要るバイナリを求める。

**表を機械で扱う案（実装はしていない）:**

1. 表を、リポジトリの 1 つのデータ（Pome の版ごとに、出す kind と member、要るバイナリ）にして、この表をそこから作る。
2. CI で、各 Pome の最新版のソースが書く kind と、Go の graph が知る kind を、そのデータと突き合わせる。kind を足す PR が、データを更新しないと落ちる。
3. MAM-GRAPH-0005 と MAM-GRAPH-0003 の hint が、データから「この kind は Pome の `<名前>` `<版>` から。Mamela を `<版>` 以降にする」と言う。
4. バイナリに版のタグ（`mamela/vX.Y.Z`）を付けて、表を revision ではなく版で書く。公開の手順（releasing.md）の、タグを付ける前の確認に「表にその版の行がある」を足す。
