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

pool・ロール・ユーザー・Token・ACL の作成と、ストレージの content type の追加には root 権限が要る。Mamela にその権限は持たせない。`mamela lab bootstrap --site lab/site.local.toml` が冪等な bash スクリプトを**表示するだけ**なので、**人が内容を確認してから**クラスタのノードで root として実行する。スクリプトは追加しかしない（削除しない。既存ゲスト・ネットワーク・HA には触れない。ストレージは cloud image 用ストレージの content type に `import` を足すだけで、既存の content type は残す）。もう一度実行しても同じ状態に収束する。

作るもの（名前は例）:

- pool `mamela-lab`
- cloud image 用ストレージ（例 `local`）の content type に `import` を追加
- **操作用**: ユーザー `mamela@pve`（パスワードなし）と、権限分離した Token `mamela@pve!lab`。次の ACL だけを持つ。
- **検査用**: 別ユーザー `mamela-audit@pve` と Token `mamela-audit@pve!audit`。組み込みロール `PVEAuditor`（読み取り専用）を `/` に持ち、preflight / postflight だけが使う。操作用と同じユーザーにすることは設定の検証で拒否する。

操作用 Token の ACL。パスごとに専用のロールを作り、そのパスで必要な権限だけを入れる（同じロールを複数のパスに付けると、不要な権限が別のパスにも付くため）。

| パス | ロール | 権限 | 用途 |
|---|---|---|---|
| `/pool/mamela-lab` | `MamelaLabVM` | `VM.Allocate`, `VM.Audit`, `VM.Config.{CDROM,CPU,Cloudinit,Disk,HWType,Memory,Network,Options}`, `VM.PowerMgmt` | pool 内の VM の作成・設定・起動停止 |
| `/storage/<disk storage>` | `MamelaLabDisk` | `Datastore.AllocateSpace`, `Datastore.Audit` | 検証用 VM のディスク |
| `/storage/<image storage>` | `MamelaLabImage` | `Datastore.AllocateTemplate`, `Datastore.Audit` | 固定した cloud image のアップロード（AllocateTemplate）と、新しいディスクへの取り込み（Audit） |
| `/sdn/zones/<zone>/<bridge>` | `MamelaLabNet` | `SDN.Use` | ネットワーク接続 |
| `/nodes/<node>` | `MamelaLabAudit` | `Sys.Audit` | ノード状態の読み取り |

`/`、`/vms`、他の pool、他のノードには何も付けない。preflight は操作用 Token の実効権限（`GET /access/permissions`）がこの表（と、pool に属する予約範囲内の `/vms/<id>`）を一つでも超えていれば停止する。各権限が必要な理由は、Proxmox VE 9.2 のソース（`PVE/API2/Qemu.pm` の `create_vm` と `check_storage_access`、`PVE/Storage.pm` の `check_volume_access`、`PVE/API2/Storage/Status.pm` の `upload`）で確認した。L1 の実機の実行で過不足を確かめて確定する。

`download-url`（ノードに URL から直接ダウンロードさせる API）は使わない。`Sys.AccessNetwork`（ノードのネットワークを任意に探れる権限）が要るためである。代わりに、Mamela を動かすマシンが cloud image をダウンロードして sha256 を確かめ、それをアップロードする。

Token の秘密は作成時に一度だけ表示される。`site.local.toml` に書いたパス（リポジトリ外）に保存し、`chmod 600` にする（グループや他人が読めるファイルは Mamela が拒否する）。

### 4.2 テンプレートを使わない VM の作り方

テンプレートは使わない。既存のテンプレートも、Mamela 用のテンプレートも作らない。テンプレートには、人が手で作って更新する手間がかかり、中身（どの image から、いつ作ったか）が宣言から見えないという弱点があるためである。代わりに、宣言の `image`（例 `ubuntu-24.04`）から毎回次のように作る（`internal/provider/pve`）。

1. **image は Mamela のコードに固定する**（`pve.Catalog`）。URL、sha256、サイズ、最小ディスクを持つ。catalog にない名前は拒否する。image の更新は、レビューされるコードの変更になる。
2. **一度だけアップロードする。** Mamela を動かすマシンが image をダウンロードし、サイズ・sha256・qcow2 のヘッダーを確かめてキャッシュする（`~/.cache/mamela/images` など。開くたびに確かめ直す）。image 用ストレージに `import` としてアップロードし、Proxmox にも sha256 を確かめさせる。ファイル名に sha256 を含めるので、別の中身と取り違えない。
3. **image から直接 VM を作る。** `POST /nodes/{node}/qemu` に `scsi0: <disk storage>:0,import-from=<image storage>:import/<file>` を渡すと、Proxmox が image を新しいディスクに複製する。Proxmox の cloud-init ドライブ（`ide2: <disk storage>:cloudinit`）に、ユーザー・SSH 公開鍵・DHCP を設定する。
4. ディスクを宣言のサイズまで広げ、起動し、タグと所有マーカー `ready` を一度の変更で付ける。

作成時にはタグを付けない。Proxmox は作成タスクの最後に、VM を pool に入れる**前**に `/vms/<vmid>` の `VM.Config.Options` でタグの権限を確かめる（`PVE/GuestHelpers.pm` の `assert_tag_permissions`。pool を渡さない）。そのため pool にだけ権限を持つ Token がタグ付きで作ると、ディスクの複製まで終えたあとで 403 になり、VM ごと消される（L1 の最初の実行で起きた）。pool に入ったあとならタグを付けられる。

image からディスクへの複製は遅いことがある。L1 の最初の実行では 3.5 GiB に約 20 分かかった。作成タスクは最大 60 分待ち、それを過ぎたら結果不明として止める。

image のバージョンは宣言の `image` と catalog で決まるので、同じ宣言からは同じ image の VM ができる。

**中断と再試行:**

- VMID は作成の前に Mamela の DB に予約し、以後その名前の VM には同じ VMID しか使わない。応答が失われて再試行しても、2 台目は作れない（同じ VMID への 2 回目の作成は Proxmox が拒否する）。
- 作成中の VM（Proxmox はタスクの最後に pool に入れる）は、Token から見えない。予約した VMID が使用中なのに見えないときは、結果不明として止め、別の VMID を選ばない。
- マーカーが `ready` でない VM は「まだない」と観測する。次の実行の create は、新しく作らずに途中から再開する（ディスク拡張・起動・マーカー）。
- 所有マーカーを持つのに DB に予約がない VM（別の DB や失われた DB）は管理せず、同じ名前で作り直すことも拒否する。

### 4.3 VMID とタグ

- VMID は `site.local.toml` の予約範囲だけを使う。`/cluster/nextid` に次の ID を選ばせることはしない（範囲外の ID になり得るため）。範囲内の候補が空いているかを `/cluster/nextid?vmid=<id>` で確かめ、空いている最小の ID を DB に予約する。
- 予約範囲に、Mamela の所有マーカーを持たないゲストが一つでもあれば、preflight で停止する。
- 所有マーカーは VM の説明（description）に書く。`mamela-name`、`mamela-image`、`mamela-state`（`creating` / `ready`）を持つ。
- タグは完成時（`ready` と同時）に付く。作成途中（`creating`）の VM にはタグがない。`ready` なのにタグのない VM は誰かが変えたものとして管理しない（`Foreign`）。
- preflight / postflight の「検証環境の内側」は pool と VMID 予約範囲で決める（タグは問わない）。作成途中の VM を続きから仕上げられるようにするためである。管理するかどうか（マーカー・予約・タグ）は provider が判断する。

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

## 7. 後片付け

Mamela 本体は削除をしない。検証環境の後片付けは別のツールで行い、次をすべて満たすものだけを対象にする。

- pool 内、VMID 予約範囲内、Mamela タグと所有マーカーあり。
- 対象を一覧表示し、人が確認してから実行する。

権限も pool 内に限られるため、このツールにバグがあっても既存ゲストには届かない。

## 8. 運用ルール

- 実機テストは `MAMELA_LAB=1` と `MAMELA_LAB_SITE=lab/site.local.toml` を明示したときだけ動く。
- サイト固有の値、既存ゲストの一覧、Token をコミットしない。ログやテストの fixture にも残さない。
- 予算やしきい値を緩める変更は、理由を書いたうえで人がレビューする。
