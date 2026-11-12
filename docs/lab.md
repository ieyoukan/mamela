# Mamela — 検証環境 (Lab) 設計

> Draft / 2026-11-08。動作保証は、実機の Proxmox 上で宣言的に定義した検証環境を使って行う。
> この文書は公開リポジトリにあるため、サイト固有の値（ホスト名、IP、既存 VM の一覧、トークン）は書かない。それらは gitignore された `lab/site.local.toml` に置く。

## 1. 目的と前提

- 既存のワークロードが稼働している自宅 Proxmox クラスタの中で、**既存のゲスト（VM / LXC）を一切壊さずに** Mamela の E2E テストを行う。
- 検証環境は手作業で作らず、`lab/` の宣言から作る。境界が守られていることを、毎回の実行前後に機械的に確認する。
- 実機テストは明示的に opt-in したときだけ動く（`MAMELA_LAB=1`）。CI の標準ジョブでは動かさない。

## 2. 安全の考え方: 境界を三重にする

Mamela のコードにバグがあっても既存ゲストに届かないよう、外側から順に次の三層で守る。

| 層 | 仕組み | 何を防ぐか |
|---|---|---|
| 1. Proxmox の権限 | Mamela 専用の pool・ユーザー・権限分離した API Token。権限は pool と、必要なストレージ・ブリッジにだけ付ける | Mamela が既存ゲストを見る・変える・消すこと自体を API が拒否する |
| 2. Mamela の所有判定 | pool + VMID 予約範囲 + タグ + 所有マーカーがすべて揃ったものだけを管理対象にする（[architecture §4](architecture.md#所有の目印)） | 権限設定の誤りがあっても、管理外のものは `Foreign` として変更しない |
| 3. 実行前後の検査 | preflight / postflight（§5）で、資源の余裕と管理外ゲストの不変性を確認する | 資源の枯渇による巻き添えや、想定外の変更を検出し、実行を止める |

### 共有資源による巻き添えに注意

権限で守れないのは **ホストの共有資源** である。

- **メモリ**: ホストの空きメモリが尽きると、OOM killer が既存の大きな VM を止め得る。
- **LVM thin pool**: thin pool が満杯になると、同じ pool にディスクを持つ全ゲストが I/O エラーになる。
- **クラスタのクォーラム**: ノードが停止するとクォーラムを失い得る。期待票数とオンラインのノード数に余裕がない構成では、1 台の停止で管理操作ができなくなる。

そのため、検証用 VM には小さな予算を設け、preflight で余裕を確認してから作る（§4、§5）。

### 推奨: 本番クラスタの外にある専用ホスト

共有資源とクォーラムによる巻き添えを根本から断つには、**本番クラスタに属さない、単独の Proxmox ホスト**を検証に使うのが最もよい。

| 構成 | 隔離の強さ | 注意点 |
|---|---|---|
| A. クラスタ外の専用ホスト（推奨） | 強い。メモリ・ストレージ・クォーラムを本番と共有しない | 専用ホストを用意して導入する手間がかかる |
| B. 本番クラスタ内の専用ノード | 中。資源は分かれるが、クォーラムと `/etc/pve` を共有する | ノードの停止や設定ミスがクラスタ全体に影響し得る |
| C. 本番ノードの空き資源 | 弱い。予算としきい値だけが頼り | 資源に余裕がないと実行できない |

どの構成でも §4 の pool・Token・ACL は同じように作る。将来、本番クラスタを Mamela で管理するときと同じ権限モデルを検証するためである。

### 長く停止していたクラスタノードを検証に使う場合

停止していたノードを起動すると、クラスタに自動で再参加する。次を **起動前に** 確認する。

- そのノードのクラスタ用アドレス（corosync の ring アドレス）を、今は別の機器が使っていないか。IP が衝突すると、そのノードだけでなくクラスタの通信とクォーラムが不安定になり得る。
- `onboot: 1` のゲストが自動起動し、他のシステム（Kubernetes クラスタなど）に古い状態のまま再参加しないか。
- Proxmox / corosync のバージョンが他のノードと大きく離れていないか。
- 構成 A にするなら、起動する前に本番クラスタからノードを外し（`pvecm delnode`）、単独ホストとして入れ直すほうが安全。

## 3. 宣言 (`lab/`)

~~~text
lab/
├── site.example.toml   # コミットする雛形（値は架空）
└── site.local.toml     # 実際の値。gitignore 済み
~~~

`site.local.toml` に書くもの:

- Proxmox API の URL、検証に使う 1 台のノード名
- pool 名、VMID 予約範囲、タグ
- ディスク用ストレージ、cloud image 用ストレージ（content type `import`）、ブリッジ
- 検証用 VM の cloud-init（ログインユーザーと SSH 公開鍵ファイル）
- 予算（VM 数、vCPU、メモリ、ディスク）
- 安全しきい値（ホストの残りメモリ、thin pool の使用率上限）
- API Token の秘密を置いたファイルのパス（リポジトリ外）

Token の秘密そのものはリポジトリにもこのファイルにも書かない。

## 4. Proxmox 側の境界

### 4.1 一度だけの準備（人が実行）

pool・ロール・ユーザー・Token・ACL の作成と、ストレージの content type の追加には root 権限が要る。Mamela にその権限は持たせない。`mamela lab bootstrap --site lab/site.local.toml` が冪等な bash スクリプトを**表示するだけ**なので、**人が内容を確認してから**クラスタのノードで root として実行する。スクリプトは追加が中心で、消すのは**操作用ユーザーと Token 自身の ACL のうち、もう要らないもの**だけ（以前のスクリプトが付けたもの。他のユーザーの権限、既存ゲスト・ネットワーク・HA には触れない。ストレージは cloud image 用ストレージの content type に `import` を足すだけで、既存の content type は残す）。もう一度実行しても同じ状態に収束する。

作るもの（名前は例）:

- pool `mamela-lab`
- cloud image 用ストレージ（例 `local`）の content type に `import` を追加
- **操作用**: ユーザー `mamela@pve`（パスワードなし）と、権限分離した Token `mamela@pve!lab`。次の ACL だけを持つ。
- **検査用**: 別ユーザー `mamela-audit@pve` と Token `mamela-audit@pve!audit`。組み込みロール `PVEAuditor`（読み取り専用）を `/` に持ち、preflight / postflight だけが使う。操作用と同じユーザーにすることは設定の検証で拒否する。

操作用 Token の ACL。パスごとに専用のロールを作り、そのパスで必要な権限だけを入れる（同じロールを複数のパスに付けると、不要な権限が別のパスにも付くため）。

**権限は手で決めない。** Mamela が呼ぶ Proxmox API のすべてと、VM の作成・設定変更で送るパラメータのすべてについて、Proxmox VE 9.2 が確かめる権限を**権限カタログ**（`internal/proxmox/permissions.go`）に書いてある。根拠は API スキーマの permissions（`/usr/share/pve-docs/api-viewer/apidoc.js`）と、ハンドラの中のチェック（`PVE/API2/Qemu.pm` の `create_vm`・`check_storage_access`・`check_vm_modify_config_perm`・`check_vm_create_serial_perm`、`PVE/Storage.pm` の `check_volume_access`、`PVE/GuestHelpers.pm` の `assert_tag_permissions`、`PVE/API2/Cluster.pm` の `resources`）。bootstrap の付与内容（`lab.Grants`）は、このカタログと、provider が実際に送るパラメータから導く。

- client は、カタログにない API やパラメータを**送る前に拒否する**。権限を書かずに新しい API を使うと、テストで必ず落ちる。
- Proxmox が `Permission check failed (path, priv)` を返したら、Mamela は「カタログにある権限が Token にない（bootstrap を再実行）」のか「カタログに漏れがある（Mamela のバグ）」のかを添えて報告する。
- テストでは、Proxmox と同じ権限チェックをする偽の API（カタログとは別にソースから書いたもの）に対して、導いた権限で作成・観測・拡張・アドレスの読み取りがすべて通ること、**どの権限を一つ外しても何かが失敗すること**（余分な権限がないこと）を確かめる。
- `mamela lab permissions --site <file>`（読み取りのみ）は、パスごとの権限と、それを必要とする API・パラメータ、Token が実際に持つ権限との差を表示する。

**編集中と plan での具体化。** カタログには API ごとの権限に加えて、操作ごとにどの API を呼ぶか（`pve.ObserveCalls`、`pve.OperationCalls`、`lab.ToolCalls`）を持たせてある。これは偽の Proxmox が受けたリクエストと照合して、実際の動きとずれないことをテストで確かめている。

- `mamela eval [--site <file>]` は、宣言にあるリソースの種類から**必要になり得る権限**を出す（VirtualMachine なら作成・拡張、NodePackage なら Proxmox の権限は不要で mamelet の apt、など。Mamela が行わない操作はそう書く）。`--site` がなければパスは `<lab pool>` のような仮の名前になる。
- `mamela plan --site <file>` は、plan の操作ごとに**今回使う権限**を実際のパスで出す。`?` の付いた権限は場合によってだけ使う（イメージがまだアップロードされていないときのアップロードなど）。Denied の操作は実行されないので出さない。この表示は plan の digest には含めず、同じカタログから決定的に導いている。apply の前の preflight は、Token がカタログの権限を過不足なく持つことを確かめる。
- bootstrap の付与内容も同じ呼び出しの表から導くので、plan に出る権限と付与される権限は食い違わない。

導いた結果（例のサイト。Control Plane あり＝seed で mamelet を入れる場合）:

| パス | ロール | 権限 | 主な用途 |
|---|---|---|---|
| `/pool/mamela-lab` | `MamelaLabVM` | `VM.Allocate`, `VM.Audit`, `VM.Config.{CPU,Disk,HWType,Memory,Network,Options}`, `VM.PowerMgmt`, `Pool.Audit`, `VM.GuestAgent.Audit` | pool 内の VM の作成（オプションごとの権限）、観測、拡張、起動、マーカーとタグ。`Pool.Audit` がないと `/cluster/resources` が VM の pool を返さない。`VM.GuestAgent.Audit` は `mamela lab tunnel` が VM のアドレスを読むため |
| `/storage/<disk storage>` | `MamelaLabDisk` | `Datastore.AllocateSpace` | VM のディスクの確保と拡張 |
| `/storage/<image storage>` | `MamelaLabImage` | `Datastore.AllocateTemplate`, `Datastore.Audit` | cloud image と seed のアップロード（AllocateTemplate）、一覧と、取り込み・CD-ROM での利用（Audit） |
| `/sdn/zones/<zone>/<bridge>` | `MamelaLabNet` | `SDN.Use` | ネットワーク接続 |

Control Plane がなければ（Proxmox の cloud-init ドライブを使う場合）、`MamelaLabVM` に `VM.Config.Cloudinit` が加わる。以前の版が付けていた `VM.Config.CDROM`、ディスク用ストレージの `Datastore.Audit`、`/nodes/<node>` の `Sys.Audit` は、どの API も必要としないので外した（VM 作成時の CD-ROM やディスクは、ドライブではなくストレージの権限だけが確かめられる）。

`/`、`/vms`、他の pool、他のノードには何も付けない。preflight は操作用 Token の実効権限（`GET /access/permissions`）がこの一覧（と、pool に属する予約範囲内の `/vms/<id>`）を一つでも超えていれば LAB-0101 で、足りなければ LAB-0104 で止める。

`download-url`（ノードに URL から直接ダウンロードさせる API）は使わない。`Sys.AccessNetwork`（ノードのネットワークを任意に探れる権限）が要るためである。代わりに、Mamela を動かすマシンが cloud image をダウンロードして sha256 を確かめ、それをアップロードする。

Token の秘密は作成時に一度だけ表示される。`site.local.toml` に書いたパス（リポジトリ外）に保存し、`chmod 600` にする（グループや他人が読めるファイルは Mamela が拒否する）。

### 4.2 テンプレートを使わない VM の作り方

テンプレートは使わない。既存のテンプレートも、Mamela 用のテンプレートも作らない。テンプレートには、人が手で作って更新する手間がかかり、中身（どの image から、いつ作ったか）が宣言から見えないという弱点があるためである。代わりに、宣言の `image`（例 `ubuntu-24.04`）から毎回次のように作る（`internal/provider/pve`）。

1. **image は Mamela のコードに固定する**（`pve.Catalog`）。URL、sha256、サイズ、最小ディスクを持つ。catalog にない名前は拒否する。image の更新は、レビューされるコードの変更になる。
2. **一度だけアップロードする。** Mamela を動かすマシンが image をダウンロードし、サイズ・sha256・qcow2 のヘッダーを確かめてキャッシュする（`~/.cache/mamela/images` など。開くたびに確かめ直す）。image 用ストレージに `import` としてアップロードし、Proxmox にも sha256 を確かめさせる。ファイル名に sha256 を含めるので、別の中身と取り違えない。
3. **image から直接 VM を作る。** `POST /nodes/{node}/qemu` に `scsi0: <disk storage>:0,import-from=<image storage>:import/<file>` を渡すと、Proxmox が image を新しいディスクに複製する。Proxmox の cloud-init ドライブ（`ide2: <disk storage>:cloudinit`）に、ユーザー・SSH 公開鍵・DHCP を設定する。
4. ディスクを宣言のサイズまで広げ、起動し、タグと所有マーカー `ready` を一度の変更で付ける。

作成時にはタグを付けない。Proxmox は作成タスクの最後に、VM を pool に入れる**前**に `/vms/<vmid>` の `VM.Config.Options` でタグの権限を確かめる（`PVE/GuestHelpers.pm` の `assert_tag_permissions`。pool を渡さない）。そのため pool にだけ権限を持つ Token がタグ付きで作ると、ディスクの複製まで終えたあとで 403 になり、VM ごと消される（L1 の最初の実行で起きた）。pool に入ったあとならタグを付けられる。

image からディスクへの複製は遅いことがある。L1 の最初の実行では 3.5 GiB に約 20 分かかった。作成タスクは最大 2 時間待つ。

image のバージョンは宣言の `image` と catalog で決まるので、同じ宣言からは同じ image の VM ができる。

**中断と再試行:**

- VMID は作成の前に Mamela の DB に予約し、以後その名前の VM には同じ VMID しか使わない。応答が失われて再試行しても、2 台目は作れない（同じ VMID への 2 回目の作成は Proxmox が拒否する）。
- 作成中の VM（Proxmox はタスクの最後に pool に入れる）は、Token から見えない。予約した VMID が使用中なのに見えないときは、別の VMID を選ばず、Proxmox のタスク一覧（`GET /nodes/{node}/tasks?vmid=<id>&typefilter=qmcreate&source=all`。Token は自分のタスクだけ見える）からその VMID の自分の作成タスクを探す。
  - 実行中または成功なら、そのタスクを待って仕上げる（作り直さない）。
  - 失敗していれば、Proxmox が VM を消しているので VMID は空くはずである。それでも使用中なら他人のゲストなので、理由を示して止める（結果不明ではない）。自分のタスクがない場合も同じ。
- 作成の応答が失われたときも、同じくタスク一覧から作成タスクを探し、同じ実行の中で続ける。タスクが失敗すれば、Proxmox の理由（例: 403 Permission check failed）をそのまま失敗として報告する。
- タスクの状態の読み取りが一時的に失敗しても（ノードが忙しいなど）、数回までは待ち続ける。
- 結果不明（`MAM-REC-0003`）になるのは、タスクが 2 時間を過ぎても終わらないときと、状態を長く読めないときだけである。どちらもタスク ID は Proxmox に残るので、もう一度 apply すれば同じタスクを待ち直す。
- マーカーが `ready` でない VM は「まだない」と観測する。次の実行の create は、新しく作らずに途中から再開する（ディスク拡張・起動・マーカー）。
- 所有マーカーを持つのに DB に予約がない VM（別の DB や失われた DB）は管理せず、同じ名前で作り直すことも拒否する。

### 4.3 VMID とタグ

- VMID は `site.local.toml` の予約範囲だけを使う。`/cluster/nextid` に次の ID を選ばせることはしない（範囲外の ID になり得るため）。範囲内の候補が空いているかを `/cluster/nextid?vmid=<id>` で確かめ、空いている最小の ID を DB に予約する。
- 予約範囲に、Mamela の所有マーカーを持たないゲストが一つでもあれば、preflight で停止する。
- 所有マーカーは VM の説明（description）に書く。`mamela-name`、`mamela-image`、`mamela-state`（`creating` / `ready`）を持つ。
- タグは完成時（`ready` と同時）に付く。作成途中（`creating`）の VM にはタグがない。`ready` なのにタグのない VM は誰かが変えたものとして管理しない（`Foreign`）。
- preflight / postflight の「検証環境の内側」は pool と VMID 予約範囲で決める（タグは問わない）。作成途中の VM を続きから仕上げられるようにするためである。管理するかどうか（マーカー・予約・タグ）は provider が判断する。

### 4.4 mamelet（VM 内の agent）を入れる（L2）

`site.local.toml` に `[controlplane]` があると、新しい VM には Proxmox の cloud-init ドライブの代わりに、Mamela が作った NoCloud seed（ISO 9660、ラベル `CIDATA`）を CD-ROM としてつなぐ（`internal/seed`）。Proxmox の API は cloud-init の snippets をアップロードできないが、ISO（content `iso`）ならアップロードできる（`Datastore.AllocateTemplate`。つなぐには `Datastore.Audit` と `VM.Config.CDROM`）。

seed の中身:

- `user-data`（JSON で書いた `#cloud-config`）: ログインユーザーと SSH 公開鍵（パスワードなし）、`qemu-guest-agent` の導入、mamelet の設定（`/etc/mamelet/config.json`: Control Plane の URL・CA 証明書・revision の署名を確かめる公開鍵・許可する APT origin）、**一度きりの登録トークン**（`/etc/mamelet/enroll-token`）、systemd unit。mamelet 本体は seed から sha256 を確かめてから `/usr/local/sbin/mamelet` に置く。
- `meta-data`、`network-config`（DHCP）、`mamelet`（linux/amd64。`scripts/build-mamelet.sh`）。

登録トークンは VM 名と VMID に結び付き、24 時間で期限が切れ、一度しか使えない（DB にはハッシュだけを保存する）。seed は Proxmox のストレージに残るが、使用済み・期限切れのトークンでは何もできない。作成を再試行するときは、トークンがまだ有効なら同じ seed を使う。

mamelet は VM の中から Control Plane（`mamela serve`）に接続して、自分宛ての署名付き revision を取りに行く（Pull）。Control Plane を動かす Mac は lab の LAN から見えない（NAT の内側）ので、`mamela lab tunnel <vm>` が SSH のリバーストンネルを張り、VM の `127.0.0.1:8443` を Mac の `mamela serve` につなぐ（VM のアドレスは qemu-guest-agent から `VM.GuestAgent.Audit` で読む。踏み台は `controlplane.tunnel_jump`）。

~~~bash
scripts/build-mamelet.sh                                  # .mamela/bin/mamelet-linux-amd64
mamela serve --site lab/site.local.toml                   # 別の端末で動かしておく
mamela plan --site lab/site.local.toml examples/lab-l2    # VM の作成（承認が要る）
mamela approve <digest> && mamela apply --site lab/site.local.toml <digest>
mamela lab tunnel --site lab/site.local.toml mamela-l2    # 別の端末で動かしておく
mamela plan --site lab/site.local.toml examples/lab-l2    # mamelet の報告から、nginx の変更集合を計画
~~~

mamelet の安全装置（`internal/mamelet`）:

- revision の署名（Ed25519）、ノード名と VMID、有効期限、serial の巻き戻しを確かめてから使う。
- 実行するのは `dpkg-query`、`apt-get`、`systemctl` だけで、シェルは使わない。名前はオプションやパスになり得ない形に限る。
- インストールは、承認された apt-get -s の変更集合と、直前の再シミュレーションが一致するときだけ、試行ごとに一度だけ行う。削除を含む集合と、許可していない APT origin のパッケージは、revision が許しても拒否する。
- Control Plane に届かないときは、まだ有効な前回の revision（既定 24 時間）で、policy が許す起動の修復（`autoStartServices`）だけを行う。一度きりの操作は新しく取得した revision でしか行わない。

### 4.5 軽量な Kubernetes（k3s）を作る

`examples/k3s` は、k3s の server 1 台と agent 2 台のクラスタの宣言である（ingress controller と metrics server は外す）。VM 3 台に合計 6 vCPU と 8 GiB を使うので、`site.local.toml` の予算を広げておく（例: `max_vms = 4`、`max_vcpu_per_vm = 2`、`max_memory_mib_per_vm = 4096`、`max_disk_gib_per_vm = 20`）。手順は L2 と同じで、承認が要る。

1. VM の作成（mamelet が入る）
2. k3s の導入。plan には、版、sha256、取得元、役割、起動の引数が出る。server が DHCP なら、agent は server の mamelet がアドレスを報告するまで待つ（MAM-PLAN-0109）。server に固定アドレスを宣言すれば、agent はそのアドレスに参加し、DHCP でアドレスが変わって切れることもない（下の「ネットワーク」）
3. サービスの有効化と起動

~~~bash
mamela serve --site lab/site.local.toml                    # 別の端末で動かしておく
mamela plan --site lab/site.local.toml examples/k3s         # 1. VM の作成
mamela approve <digest> && mamela apply --site lab/site.local.toml <digest>
mamela lab tunnel --site lab/site.local.toml k3s-server     # VM ごとに、別の端末で動かしておく
mamela plan --site lab/site.local.toml examples/k3s         # 2. k3s の導入（mamelet の報告から）
mamela approve <digest> && mamela apply --site lab/site.local.toml <digest>
mamela plan --site lab/site.local.toml examples/k3s         # 3. k3s の有効化と起動
mamela approve <digest> && mamela apply --site lab/site.local.toml <digest>
~~~

### ネットワーク（DHCP と固定アドレス）

VM の宣言は、`network: Dhcp` か `network: Static(address: ..., gateway: ..., dns: [...])` を必ず書く（既定はない）。lab では `site.local.toml` の `[network]` が、どちらをどこまで許すかを決める。

~~~toml
[network]
dhcp = true                  # Dhcp を宣言した VM を許す
static_first = "10.0.0.40"   # 固定アドレスに使ってよい範囲（両端を含む）
static_last = "10.0.0.49"
~~~

- **範囲の決め方:** LAN の DHCP サーバーの払い出し範囲と、使われているアドレスのどちらにも重ならない範囲にする。Mamela はルーターの設定を読めないので、範囲が空いていることは人が確かめる。lab のノードから、範囲内のアドレスに応答がないことを確かめておくとよい。
- **範囲の外は plan で止める**（LAB-0304）。apply でも、Proxmox に書く前にもう一度確かめる。
- **公開リポジトリに実アドレスを書かない。** 固定アドレスを使う lab 用の宣言は、gitignore してある `lab/local/` に置く（例: `examples/k3s` を `lab/local/k3s-static` に写し、`Dhcp` を `Static(...)` に変える）。

mamelet の安全装置（k3s）:

- 取得元は `https://github.com/k3s-io/k3s/releases/download/<version>/k3s`（arm64 では `k3s-arm64`）に固定している。revision が渡すのは版と sha256 だけである。
- sha256 が承認したものと違えば、何も置かない。導入は承認ごとに一度だけ行う。
- unit の中身は mamelet が決める。手で変えられた unit は観測で報告し、plan は止まる（MAM-PLAN-0204）。

**VM を複数台まとめて作るとき:** apply は、全台を作り終えてから起動する（作成は `create-vm`、起動は `start-vm` という別の操作で、`start-vm` は実行の最後にまとめて行う）。以前は 1 台ずつ「作成 → 起動」していたため、先に起動した VM の初回起動が次の VM のディスクのインポートと同じストレージで重なり、I/O 待ちで `/boot` のデバイスが時間切れになって緊急モード（sulogin）で止まった。作成が 1 台でも失敗した実行では、どの VM も起動しない（次の apply が続きから作って起動する）。

**起動で止まった VM:** VM が 15 分を超えて動いているのに mamelet が一度も報告しなければ、plan はその VM の上のリソースを MAM-PLAN-0110 で止め、理由と対処を示す。コンソールで状態を確かめてから、`mamela reboot` で再起動する。緊急モードのように ACPI の要求に応えない guest には `--hard`（リセット）を使う。

## 5. preflight / postflight

~~~bash
mamela lab preflight  --site lab/site.local.toml   # 検査し、問題がなければ管理外ゲストの baseline を .mamela/lab-baseline.json に保存
# ... 実機テスト ...
mamela lab postflight --site lab/site.local.toml   # baseline と比べ、管理外ゲストが変わっていないことを確認
~~~

L1 から先は、`mamela apply` が同じ preflight / postflight を前後に自動で実行する:

~~~bash
mamela plan --site lab/site.local.toml examples/lab-vm   # 実機を読み取りで観測して計画し、保存する
mamela approve <plan-digest>                             # その計画だけを承認する
mamela apply --site lab/site.local.toml <plan-digest>    # preflight → 再観測して実行 → postflight
~~~

preflight / postflight はどちらも読み取りだけで、一つでも BLOCK があれば終了コード 1 になる。結果のコードは [reason-codes.md](reason-codes.md#lab--検証環境) にある。

### preflight

- クラスタがクォーラムを満たし、対象ノードがオンライン。
- Token の実効権限（`GET /access/permissions`）が §4.1 の表の範囲に収まっている。範囲外の権限があれば停止する（広すぎる Token を使わない）。
- 対象ノードの空きメモリから、これから作る VM のメモリを引いても、しきい値以上残る。
- ディスク用 thin pool の使用率が上限未満で、作る VM のディスクを足しても上限を超えない。
- pool 内の VM 数・vCPU・メモリ・ディスクの合計が予算内。
- cloud image 用ストレージが content type `import` を受け付け、操作用 Token から使える。
- 予約範囲に、所有マーカーのないゲストがない。
- **管理外ゲストのスナップショット**を取る: 全ゲストの VMID、ノード、状態、設定 `digest`、稼働中なら uptime。

### postflight

- 管理外ゲストのスナップショットを再取得し、preflight と比べる。
  - 設定 `digest` と状態が同じ。
  - 稼働中だったものは uptime が増えている（再起動していない）。
- 違いがあればテストを失敗にし、差分を表示する。Mamela 起因かどうかに関わらず人が確認する。

## 6. 段階的な進め方

| 段階 | 内容 | 既存ゲストへのリスク |
|---|---|---|
| L0 | 読み取りのみ。Token で pool・ノード状態を観測し、preflight / postflight を回す | なし（権限上も読み取りだけ） |
| L1 | 予算内で VM を 1 台、テンプレートなしで作成・起動・観測する。重複作成防止と中断からの再観測を試す（停止・削除は実装しない） | メモリ・thin pool のみ（予算と preflight で抑える） |
| L2 | cloud-init で Agent を導入し、Enrollment、APT、systemd、ドリフト修復を試す | L1 と同じ |
| L3 | 検証環境の VM 自体を Emela で宣言し、Mamela で管理する（dogfooding） | L1 と同じ |

ローカル（macOS の QEMU + HVF、Ubuntu arm64 cloud image + NoCloud seed ISO）でも cloud-init と Agent の多くは試せる。L2 の前にローカルで Agent を固めると、実機での試行回数を減らせる。

## 6.5 再起動（`mamela reboot`）

起動で止まった VM（MAM-PLAN-0110）などを、Mamela の記録の中で再起動する。`mamela destroy` と同じく plan の外の明示的なコマンドで、所有の確認（pool、VMID の範囲、タグ、マーカー、VMID の予約）、名前の打ち直し、preflight と postflight を行う。変えるのは VM の電源の状態だけである。

~~~bash
mamela reboot --site lab/site.local.toml k3s-agent-1                                  # 何をするかを表示するだけ
mamela reboot --site lab/site.local.toml --confirm k3s-agent-1 k3s-agent-1            # guest に再起動を頼む（ACPI）
mamela reboot --site lab/site.local.toml --hard --confirm k3s-agent-1 k3s-agent-1     # 応答しない guest をリセットする
~~~

- 動いている VM だけを再起動する。止まっている VM は、plan と apply が起動する（`start-vm`）。
- 権限は `VM.PowerMgmt` で、増えない（`status/reboot` と `status/reset`）。

## 7. 後片付け（`mamela destroy` と `mamela lab cleanup`）

宣言から消しても実物は消さない（plan に削除は現れない）。VM を消すのは、plan / apply とは別の明示的なコマンド `mamela destroy` だけである。

~~~bash
mamela destroy --site lab/site.local.toml mamela-l2                       # 何が消えるかを表示するだけ（変更なし）
mamela destroy --site lab/site.local.toml --confirm mamela-l2 mamela-l2   # 名前を打ち直したときだけ実行する
mamela lab cleanup --site lab/site.local.toml > cleanup.sh                 # どの VM も使わないアップロードを消すスクリプト（人が root で実行）
~~~

- 対象は、pool 内・VMID 予約範囲内・タグと所有マーカーあり・Mamela の DB に予約あり、をすべて満たす VM だけ（作成途中の VM も含む。タグや予約のない VM は消さない）。
- 消す前に、ディスクと接続中のイメージを表示する。実行時にもう一度観測し、表示したときから変わっていれば何もしない。
- 動いていれば止めてから、ディスクごと消す（`DELETE /nodes/{node}/qemu/{vmid}?destroy-unreferenced-disks=1`。バックアップやレプリケーションのジョブから外す `purge` はクラスタ全体の設定を変えるので使わない）。消えたことを VMID が空いたことで確かめ、DB の予約と mamelet の登録を消す（その証明書はもう使えない）。前後に preflight / postflight を行う。
- DELETE を送れるのは `mamela destroy` が作る専用の client だけで、provider と reconciler の client は送れない。権限は `VM.Allocate`（作成と同じ）と `VM.PowerMgmt` で、増えない。
- アップロードした cloud image と seed ISO は Mamela の権限では消さない。ストレージ上のファイルを消すには、そのストレージ全体への `Datastore.Allocate` が要り、同じストレージにある他のゲストのバックアップやテンプレートまで消せてしまうためである。`mamela lab cleanup` が、名前が `mamela-` で始まり、どの VM も使っていないファイルだけを `pvesm free` するスクリプトを出すので、人が確認して root で実行する。

## 8. 運用ルール

- 実機テストは `MAMELA_LAB=1` と `MAMELA_LAB_SITE=lab/site.local.toml` を明示したときだけ動く。
- サイト固有の値、既存ゲストの一覧、Token をコミットしない。ログやテストの fixture にも残さない。
- 予算やしきい値を緩める変更は、理由を書いたうえで人がレビューする。
