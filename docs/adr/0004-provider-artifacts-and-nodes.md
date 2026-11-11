# ADR 0004 — プロバイダーの成果物（ファイル）とノード

- 状態: Accepted（2026-11-11 に合意。1–4 を実装し、検証環境の実機で確かめた）
- 日付: 2026-11-11
- 対象: プロバイダーの契約（ADR 0002）。Proxmox のプロバイダーを Mamela の外（`github.com/ieyoukan/mamela-proxmox`）へ出すために必要な拡張

## 文脈

組み込みの Proxmox プロバイダー（`internal/provider/pve`）を、Pome で配る外部のプロバイダーに移したい。今の契約（ADR 0002）では、次の 3 つができない。

1. **大きなファイル:** クラウドイメージ（数百 MB）や seed ISO をアップロードできない。`host/request` はフォーム（`application/x-www-form-urlencoded`）だけを送る。
2. **秘密を含むファイル:** mamelet の seed には、一度きりの登録トークン、CP の CA、mamelet のバイナリが入る。登録トークンは秘密なので、信頼しないプロバイダーに作らせてはならない。中身を見せてもいけない。
3. **ノード:** NodePackage、NodeService、NodeK3s は、組み込みの `VirtualMachine` を名前で参照する。プロバイダーの kind の上では、mamelet を動かせない。

## 決定

### 1. カタログは Mamela に残す

各呼び出しが何をするか（効果、パラメータの場所、前提条件）は、引き続き host のカタログが決める（`builtin:proxmox`）。プロバイダーの申告は、安全の判断に使わない。

- **カタログに足すもの:**
  - アップロード: どのパラメータがファイルか（`file`）
  - 起動、再起動
  - ディスクの拡張
  - タスクの一覧
- **前提条件の例外:** 前提条件を持つ呼び出し（設定の `digest`）は、これまでどおり観測した版を求める。例外は 1 つだけ認める。**同じ許可の作成要求が、この apply で作ったばかりの対象**への変更は、版なしで送ってよい。plan の時点で存在しなかった対象には、上書きしてしまう他人の変更がないからである。

### 2. 成果物: プロバイダーはファイルの中身に触れない

変更（Change）は、必要なファイルを `artifacts` に宣言する。

| 種類 | 中身 | 作るもの |
|---|---|---|
| `download` | `url`（https）と `sha256`、保存する名前 `name` | host が取得し、sha256 を確かめる（キャッシュする） |
| `seed` | mamelet の seed。`node`（リソースの名前）、`nodeId`（VMID）、`network` | host が作る。登録トークンを発行し、site のネットワークの規則（ADR 0003 の範囲）を確かめる |

- **plan:** plan はこれらを表示し、承認の対象にする（URL、sha256、seed のノードとネットワーク）。
- **参照の仕方:** プロバイダーは成果物を ID でだけ参照する。
  - 承認した要求のパラメータには、`{artifact:<id>.name}` と `{artifact:<id>.sha256}` を書ける。host は apply の中で成果物を用意したときに、これを実際の値に置き換えてから照合する。seed の名前とハッシュは、トークンを含むので apply まで決まらない。
  - プロバイダーは `host/artifact`（ID）で、名前、sha256、大きさを知る。中身は渡さない。
  - アップロードは `host/request` に `upload: <id>` を付けて頼む。host は、承認したフォームの値に、カタログが `file` とするパラメータとしてファイルを足し、`multipart/form-data` で送る。
- **用意できないとき:** 用意できない成果物（取得の失敗、sha256 の不一致、site が許さないネットワーク）は、どの要求も送る前に、その apply を失敗にする。

### 3. ノード: プロバイダーの kind も mamelet の載るマシンになれる

- **宣言:** マニフェストの kind に `node: true` を書ける。
- **観測:** その kind の観測は、存在するリソースについて `node` を返す。
  - `id`: 整数。revision と登録に使う。Proxmox では VMID
  - `running`
  - `uptimeSeconds`
- **Graph:** NodePackage、NodeService、NodeK3s の `node` は、`VirtualMachine` か、プロバイダーのリソースの名前を指せる。プロバイダーのリソースを指すときは、その kind が `node: true` でなければならない（プロバイダーを起動した後に確かめる）。ノードの名前は、種類をまたいで一意にする。
- **plan:** ノードの存在、Mamela のものか、起動しているか、稼働時間（MAM-PLAN-0110、0111）を、`VirtualMachine` と同じように扱う。
- **revision と登録:** revision と登録トークンは、観測した `id` で発行する。
- **Emela:** `Mamela.Node` 型と `Mamela.package_on` などを足す。プロバイダーの Pome は、自分の VM から `Mamela.Node` を返す関数を出す。

### 4. Proxmox のプロバイダーを Mamela の外に出す

- **リポジトリ:** `github.com/ieyoukan/mamela-proxmox` に置く。
  - Go のプロバイダー（`GOOS=wasip1`）
  - Pome（`Proxmox.Site`、`Proxmox.Vm`、`Proxmox.vm`、`Proxmox.node`）
- **できること:**
  - 固定したクラウドイメージ（成果物 `download`）と mamelet の seed（成果物 `seed`）から VM を作る。
  - 作成、起動、vCPU とメモリの変更（動いていれば再起動）、ディスクの拡張。
- **しないこと:** 縮小、イメージの変更、ネットワークの変更、削除はしない（理由をマニフェストに書く）。
- **組み込みとの関係:** 組み込みの `VirtualMachine` は、機能が揃うまで残す。どちらで作った VM も、相手の所有マーカーを持たないので、互いに変えない。
- **公開:** タグの push（Pome の公開は取り消せない）は、人が確かめてから行う。

## 実機で見つけたこと

- **pveproxy のフィールドの順:** pveproxy は、マルチパートのフィールドを決まった順に読む（`content`、`checksum-algorithm`、`checksum`、ファイル）。host は、呼び出しが宣言したフォームの順に書く。
- **拒否の理由の場所:** Proxmox は、拒否の理由を本文でなく状態行に書く。`HostResponse.reason` で渡す。
- **許可の期限:** イメージの取り込みを待つ間に、許可（10 分）が切れた。期限は apply の期限（2 時間）に合わせた。許可は 1 回の apply で一度だけ使う。

## 結果

- Proxmox の扱いを変えるのに、Mamela 本体の版を上げなくてよくなる。プロバイダーの版は Pome で固定する。
- 同じ仕組みで、AWS などのプロバイダーも、イメージや seed を使うマシンを作れる。
- host の仕事が増える。具体的には、取得、seed の作成、マルチパートの送信である。これらは host の中にあり、プロバイダーからは見えない。
