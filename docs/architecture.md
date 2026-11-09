# Mamela — アーキテクチャ設計書

> Status: Draft / v0.2 / 2026-11-08。設計の合意事項と未決事項を分けて記載する。
> Mamela は開発初期の構想であり、以下の API・DSL・CLI 例は実装済みを意味しない。
> Emela の記述は **Emela v0.27.0（2026-11-07）** の README / `spec/` に基づく。

## 1. 目的

Mamela は、[Emela](https://github.com/emela-lang/emela) で宣言したインフラの Desired State を、Go 製 Control Plane と Pull 型 Go Agent が継続的に収束させる IaC ツール。Kubernetes の Reconciliation モデルを Proxmox VM、Linux パッケージ、systemd サービスに拡張する。

最大の目標は **AI が生成した構成でも、制限された権限と決定的な安全ポリシーの下で自動適用できること**。コードの意味や実世界の副作用を完全に予測できるわけではないため、不明な変更は止める。

### 設計目標

1. **決定論的 (Deterministic)**: 同じ入力からは、いつ・どこで実行しても同じ Resource Graph と同じ Plan が得られる。実世界の実行結果は決定的にできないので、決定性の境界（§3.1）を明示し、その外側は「再観測して判断する」。
2. **人間に優しい (Human-friendly)**: 何が・なぜ・どう変わるか（変わらないか）を、人が数秒で読めること。止めたときは理由と次の一手を必ず示す（§3.2）。
3. **安全 (Safe by default)**: 削除・置換・権限拡大は自動許可しない。安全性が不明なら停止。
4. **動作を IaC で保証する**: 検証環境そのものも宣言的に定義し、受け入れテストを実機 Proxmox 上で再現可能に回す（[検証環境](lab.md)）。

### 原則

1. **Desired State first**: 宣言は命令列でなく目標状態を表す。
2. **Least authority**: Emela、Control Plane、Provider、Agent の権限を分ける。権限境界は Mamela のコードだけでなく、Proxmox の ACL など外部の仕組みでも二重に強制する。
3. **Continuous reconciliation**: 宣言と観測の差分を継続的に調整する。
4. **Observable & auditable**: 決定、変更、失敗、再試行の理由を追跡できる。Plan はその入力から再計算して検証できる。
5. **Explicit ownership**: 宣言から消えたリソースを勝手に削除しない。Mamela が作っていないリソースを取り込まない。
6. **Idempotent operations**: 再起動・ネットワーク断・重複配送でも収束可能にする。

## 2. スコープ

MVP: Proxmox VE 上の Ubuntu VM の作成/観測、cloud-init による Agent 配備、Agent の Pull、APT の状態観測と承認済みインストール、systemd サービスの有効化/稼働、差分/承認/履歴、SQLite を用いた単一 Control Plane。Emela → WASM の構成評価経路は最初に PoC を行う。

MVP 対象外: 任意コマンド/任意スクリプト、VM/Volume の自動削除、未知の APT ソースの自動追加、無条件アップグレード、既存（Mamela 管理外）VM の取り込み、マルチクラウド、HA Control Plane、無停止や完全ロールバックの保証。

## 3. 全体構成

~~~text
AI / Human / Git
      |
   *.emel + Pome.toml + Pome.lock
      |
 emela build --target wasm   (固定バージョン / ネットワークなし / lock 必須)
      |
 main.wasm  ── Go: import allowlist 検査 (wasi:cli の stdout / stderr / exit / types のみ)
      |
 wasmtime run -S p3=y   (固定バージョン / FS・ネットワーク・env なし / 時間・メモリ・出力量制限)
      |  stdout: mamela-graph/v1 (JSON 1 文書)
      v
+-----------------------------------------------------------+
| Go Control Plane                                          |
| Parse + Canonicalize + Schema + Ownership + Plan + Policy |
| SQLite: resources, revisions, observations, operations    |
| Proxmox reconciler + Agent config/signing + event log     |
+----------------------+------------------------------------+
          |           ^
   Proxmox API         |  HTTPS Pull / report (mTLS)
   (pool 限定 Token)   |
          |            |
     Ubuntu VM    Go Agent (per VM)
     cloud-init   APT / systemd / local observation & policy
~~~

**信頼境界:** Emela/WASM は構成グラフを stdout に書くだけで、クラウド API・ネットワーク・ホストファイルにはアクセスできない。これは Emela の型/Effect 検査ではなく、**WASM コンポーネントの import 集合**で構造的に保証する（§9）。Go 側は生成物も信頼せず、型付きスキーマ、権限、計画と状態を検証する。Proxmox 操作は Control Plane、OS 操作は VM 内の Go Agent が実行する。

### 3.1 決定性の境界

| 段階 | 入力 | 決定的か | 担保の方法 |
|---|---|---|---|
| 評価 (Emela → Graph) | ソースツリー hash、`Pome.lock`、emela / wasmtime のバージョン | **決定的** | env / random / clock の import を禁止。引数・stdin を与えない。正規化後の digest で比較 |
| 検証・正規化 | Graph テキスト、Schema バージョン | **決定的** | 純粋関数。キー順・単位・既定値を正規化してから digest |
| 計画 (Plan) | Desired Revision digest、Observation Snapshot、Policy バージョン | **決定的** | 壁時計を読まない。時刻は Snapshot の `asOf` としてだけ入る。出力順は安定ソート。Plan digest は全入力を含む |
| 実行 | Plan + 実世界 | **非決定的** | Operation ID を Plan digest + 手順番号から導出し冪等キーにする。結果が不明なら再観測 |
| Agent のローカル判断 | 署名済み Revision、ローカル観測、組込み禁止ルール | **決定的** | 同じ Revision と観測から同じ操作列を出す |

- 計画の入力は SQLite に保存し、`mamela plan --replay <plan-id>` で再計算して digest 一致を確認できるようにする（監査と回帰テストのため）。
- ツールチェーン（Go、emela、wasmtime）と VM イメージは、バージョンと digest を固定する。暗黙の「latest」を使わない。
- CI では同じソースを 2 回評価して Graph digest の一致を確認する。

### 3.2 人間に優しい UX

- **Plan は Terraform 風の差分 + 判定 + 理由**で表示する。各変更に `Auto` / `ApprovalRequired` / `Denied` と、安定した理由コード（例: `MAM-POL-0102`）、平易な一文、解消方法を付ける。
- **何もしないことも明示する**: 「宣言から消えたが Retain のため保持」「観測が古いため保留」など、実行しない理由も表示する。
- `mamela plan` / `diff` / `describe` は常に読み取り専用で、いつ実行しても安全。変更は `mamela apply <plan-id>` だけが行い、Plan digest が一致しなければ拒否する。
- 人が書く値は人の単位で書ける（`memory: 8 GiB`）。内部では正規化する。
- 名前は人が付けたものを主に表示し、UID は短縮して併記する。
- エラーは「何が」「どこで（宣言のリソース名/フィールド）」「なぜ」「どうすればよいか」の順で表示する。`mamela explain <理由コード>` で詳細を出す。
- 既定値は安全側に寄せ、必須フィールドを最小にする。

### 技術選定

| 領域 | 採用/方針 |
|---|---|
| DSL | Emela v0.27 系（`.emel`、Pome）。バージョン固定 |
| DSL 実行 | WASM（WASI 0.3 component）+ Wasmtime CLI（49 以上、固定バージョン）の隔離子プロセス |
| Emela → Go の受け渡し | stdout に JSON 1 文書 `mamela-graph/v1`（Emela 側は `x/json`（v0.3.1）、Go 側は `encoding/json/v2`。§9.2） |
| Control Plane / CLI | Go |
| 状態ストア | SQLite（WAL、単一 writer を想定） |
| VM Provider | Proxmox VE API（Mamela 専用 pool に限定した API Token） |
| 初期設定 | cloud-init（ステップ 2 は Proxmox の cloud-init ドライブ。Agent の導入には NoCloud seed ISO を候補とする。§10） |
| VM 内の変更 | Pull 型 Go Agent、APT、systemd |
| ノード認証 | 初回限定トークン → mTLS 証明書 |
| Agent ローカル永続化 | SQLite または原子的なローカルキャッシュ（検証時に確定） |

### 実行形態

初期リリースは **単一プロセス/単一アクティブ Control Plane + SQLite**。API と Reconciler は同一バイナリに統合可能。SQLite を複数の独立ノードで共有しない。HA と分散ロックが必要になった場合は、ストレージとリーダー選出を別途再設計する（初期段階から PostgreSQL 移行を前提にはしない）。

## 4. リソースモデル

### Emela DSL（構想例 / 実在 API ではない）

Emela 側は「Graph を返す純粋な関数」と「それを stdout に書く `main`」だけで構成する。`Mamela.*` は今後作る Mamela 用 Pome の想定であり、存在しない。

~~~emela
import Mamela.{Graph, Vm, Service}

fn infrastructure() -> Graph {
  app = Vm(name: "app-server", cpu: 4, memory_mib: 8192, image: "ubuntu-24.04")

  Graph.empty()
    |> Graph.vm(app)
    |> Graph.package(app, "nginx")
    |> Graph.service(app, Service(name: "nginx", enabled: True, running: True))
}

fn main() {
  infrastructure() |> Graph.print   # mamela-graph/v1 (JSON) を stdout に書く
}
~~~

この記述は目標状態の定義であり、評価時に `apt install` や Proxmox API を実行しない。Emela の WASM 出力からはそもそも実行できない（§9）。

### 中間表現（説明用 YAML）

~~~yaml
apiVersion: mamela.dev/v1alpha1
kind: VirtualMachine
metadata:
  uid: vm-01
  name: app-server
  generation: 3
  resourceVersion: "105"
spec:
  provider: proxmox
  image: ubuntu-24.04
  cpu: 4
  memoryMiB: 8192
  deletionPolicy: Retain
status:
  observedGeneration: 3
  phase: Ready
  externalID: "pve/<node>/<vmid>"
  conditions:
    - type: Ready
      status: "True"
---
apiVersion: mamela.dev/v1alpha1
kind: NodePackage
metadata:
  name: app-server/nginx
spec:
  nodeRef: app-server
  name: nginx
  state: Present
  removalPolicy: Deny
---
apiVersion: mamela.dev/v1alpha1
kind: NodeService
metadata:
  name: app-server/nginx-service
spec:
  nodeRef: app-server
  name: nginx
  enabled: true
  running: true
  dependsOn:
    - app-server/nginx
~~~

識別子は `kind + scope + stable UID` を基本とする。名前の再利用で別の VM を誤操作しない。型固有の Schema と参照解決は Go 側で行う。

- `metadata.generation`: Spec の更新世代。
- `metadata.resourceVersion`: 楽観的な競合検出用。
- `status.observedGeneration`: Controller が処理を試みた世代。成功を意味しない。
- `conditions`: Ready / Reconciling / Blocked / Degraded と理由コード。
- `externalID`: Proxmox 等の実リソースとの対応。必ず再観測して確認。
- `deletionPolicy: Retain`: 定義から消しても実体を消さない。

### 所有の目印

Mamela が作った Proxmox VM は、次の **すべて** を満たすものだけを管理対象とする。一つでも欠けたら `Foreign` として扱い、観測だけ行い変更しない。

- Mamela 専用 pool に属する。
- 設定された VMID 予約範囲内にある（`/cluster/nextid` は使わない）。
- Mamela のタグと、description 内の所有マーカー（Control Plane ID + resource UID）を持つ。

## 5. Control Plane と SQLite

**3種類の状態を区別する。**

| データ | 意味 | 正本 |
|---|---|---|
| Desired State | 管理したい構成 | SQLite の有効 Revision |
| Observed State | 最後に見えた実状態（時刻付き） | 観測結果のキャッシュ。実機が最終情報源 |
| Execution State | 操作、事前条件、リトライ、結果 | SQLite の操作ジャーナル |

SQLite の WAL モードとトランザクションを使い、**単一 Control Plane** の変更を整合させる。DB は API サーバーのローカル永続ディスク上に置き、バックアップ/復元、マイグレーションを運用要件とする。別プロセス/複数ノードからの共有ファイル直接書き込みや NFS 上の WAL 前提運用はしない。

### 概念テーブル

- `resources`: UID / kind / owner / desired spec / generation / resourceVersion / lifecycle。
- `observations`: observed spec/status / observedGeneration / observedAt / condition。
- `revisions`: canonical graph / source tree hash / lock hash / toolchain versions / revision digest / approvals / activation。
- `plans`: inputs (revision digest, snapshot id, policy version) / changes / preconditions / decisions / digest。
- `approvals`: plan digest / approver / scope / validity / consumedAt。
- `operations`: operation ID / resource ID / intent / stage / lease / retry / external request/task ID。
- `agents`: node identity / certificate serial / revision / heartbeat。
- `events`: durable append-oriented event log。

可変リソース構造は JSON 列に格納し、UID、Generation、Operation ID 等は独立した列・制約にする。JSON 列は Go 側で正規化（キー順固定）してから書く。型検証は Go の Schema レイヤで行う。

### 同時実行・障害復旧

- `resourceVersion` / Generation を用いて競合更新を拒否する。
- リソース/ノードごとに操作を直列化し、実行前に DB に Intent を書く。
- 外部 API と DB を一つのトランザクションにできない。Exactly-once を仮定しない。
- Proxmox では VMID を Intent に先に記録し、その VMID を明示して作成する。同じ VMID での再作成は Proxmox 側で失敗するため、二重作成の自然な防止になる。再起動後はタスク ID、VMID、所有マーカーで再観測する。
- Lease は期限付き。失効後の引継ぎ時も再観測と fencing 相当の仕組みを用いる。
- Status と Event は、実行結果が不明なときに「成功」を推測しない。

## 6. Reconciliation

1. ソース変更を受け付け、`emela check` で型検査・Effect 検査する。
2. ネットワークなしで `emela build --target wasm` し、`main.wasm` の import を allowlist 検査する。
3. WASM を隔離評価し、`mamela-graph/v1` を読み取って正規化した Resource Graph を作る。
4. Go が Schema、所有権、参照、依存関係、Policy を検証し、immutable Revision とする。
5. Desired と Observation Snapshot の差から Plan を作り、観測の鮮度と前提条件を付ける。
6. 各変更を `Auto` / `ApprovalRequired` / `Denied` に分類し、理由コードを付ける。
7. 許可された操作だけを Plan digest に紐づける。
8. Provider / Agent が実行前に事前条件を再評価し、実行する。
9. 結果・Conditions・Events を書き戻す。ドリフト時は新しい Snapshot で再計画する。

VM Ready → Agent enrolled/healthy → NodePackage Ready → NodeService Ready の依存グラフで進める。Agent が未登録でも VM を作り直さない。APT 操作はノードごとに直列化する。失敗時は指数 Backoff と回数制限を使う。jitter は乱数でなく、Operation ID から導いた値にして再現可能にする。

### Pull 型 Agent

- Agent は自ノード宛ての構成のみ HTTPS で Pull する。
- 普段は定期 Pull（初期案 30–60 秒 + ノード ID 由来の固定オフセット）。Long Poll は後から追加可能。
- Revision にノード ID、digest、有効期限、許可操作を含め、署名を検証する。
- Agent は実状態を観測し、ローカル Policy と前提条件を確認してから操作する。
- 結果・観測・heartbeat を Control Plane に返す。
- オフライン時は新規の高リスク処理を行わない。キャッシュを使った修復は有効期限と許可済み操作範囲に限定する。
- Policy を Agent に送る場合も、Agent 自体に変更不能な禁止操作を持たせる。ネットワークからの命令だけで制限を緩和できないようにする。

### APT の計画

パッケージ名だけでは実際の変更を決められない（依存・Recommends・削除が起き得る）。

1. Agent が `apt-get -s`（シミュレーション）で実際の変更集合（導入/更新/削除されるパッケージとバージョン）を計算して報告する。
2. Control Plane はその集合を Plan に含め、判定と承認をその集合に紐づける。
3. Agent は実行直前に再シミュレーションし、集合が承認時と異なれば実行せず再計画を要求する。
4. 削除を含む集合は `Denied`。

## 7. セキュリティ / AI 自動適用

### 2段階のポリシー

**構成評価の安全性:** WASM コンポーネントの import を `wasi:cli` の stdout / stderr / exit / types に限定し、Wasmtime にはディレクトリ・ネットワーク・環境変数を渡さない。CPU 時間、メモリ、出力量も制限する。Emela の型と Effect は、構成を書く人への補助と、Mamela 用 Pome 内の能力分離に使う。コンパイル成功はインフラ変更の安全性を意味しない。

**実操作の安全性:** Go Policy Engine が操作種別、変更量、所有権、承認、実状態を評価し、Agent/Provider が実行直前にも検証する。生成されたコードや Plan 自体を権限の正本としない。

| 判定 | 例 | デフォルト |
|---|---|---|
| Auto | 読み取り、明示許可されたサービス再起動 | 事前条件を満たした場合のみ |
| ApprovalRequired | 初回パッケージ追加、VM サイズ変更、サービス停止、Firewall 拡大 | 承認待ち |
| Denied | VM/Volume 削除、破壊的置換、任意 shell 実行、未知の APT source、`Foreign` リソースへの変更 | 拒否 |

APT インストールは maintainer script の特権実行を含み得るため「追加だから安全」としない。許可済み apt source / package/version、signature verification、変更集合の検証と承認が必要。サービスを起動するだけでも副作用はあり得る。

### 削除と承認

- 未宣言 → `Unmanaged/Orphaned` として保持。`delete` は別の明示行為。
- 承認は Plan digest、対象 ID、操作範囲、期限に結びつけ、変更で失効する。
- Agent は署名不正、期限切れ、対象ノード不一致、未知の操作を拒否する。
- Provider credential、Agent secret、署名鍵は Resource Graph/ログに含めず、安全な権限境界で渡す。
- Rollback は best-effort。不可逆な OS / DB 操作は安全性を保証できない。
- Control Plane 自身の乗っ取り、管理者権限濫用、供給元侵害は WASM だけでは防げない。最小権限・監査・強制的な拒否ルールを合わせる。

### ビルドのサプライチェーン

- `emela build` は Pome の依存（`x/json` など）を取得し得る。Mamela では `Pome.lock` を必須とし（lock がなければ emela 自体がエラーにする）、依存は事前取得したキャッシュから、ネットワークなしでビルドする。`EMELA_POMEPROXY=off` と `EMELA_POME_CACHE` で実現でき、キャッシュにない依存はビルドが失敗する（ステップ 0 で確認済み。[emela-wasm.md](emela-wasm.md)）。
- **`emela build` はキャッシュの中身を `Pome.lock` の hash と照合しない**（中身を書き換えたキャッシュからでも、そのままビルドする。実測）。そのため評価の前に必ず `emela pome verify` を実行し、違えば何もビルドせずに止める（`MAM-EVAL-0103`）。キャッシュは `scripts/install-toolchain.sh` が proxy と sumdb を通して `.tools/pomes` に取得する。
- emela / wasmtime のバイナリは checksum を固定して導入する。

## 8. API / CLI（設計案）

~~~text
POST /v1/agents/enroll             # 一回限り短命 Bootstrap Token
GET  /v1/nodes/{id}/desired        # ETag による変更検出、将来 Long Poll
POST /v1/nodes/{id}/observations
POST /v1/nodes/{id}/events
POST /v1/nodes/{id}/heartbeat
~~~

初期登録では Token を Proxmox VMID と期待ノードに紐づけ、再利用を拒否。以降は mTLS と証明書ローテーション・失効を導入。Node ID は URL だけでなく証明書主体と照合する。

~~~bash
mamela get vms
mamela get nodes
mamela describe vm app-server
mamela plan                  # 読み取り専用。差分・判定・理由
mamela plan --replay <plan-id>
mamela explain MAM-POL-0102
mamela apply <plan-id>
mamela approve <plan-id>
mamela events --node app-server
mamela reconcile app-server
~~~

CLI 名は `mamela`、Node Agent は `mamela-agent`（いずれも未実装）。

## 9. Emela / WASM 技術方針

### 9.1 Emela v0.27 で確認できる事実

upstream の README と `spec/targets/wasm.md`（`[target.wasm]`）より:

- `--target wasm` は `main.wasm` 一つだけを出力する。WASI 0.3 component で、`wasi:cli/run@0.3.0` を export し、`pub fn main` を呼ぶ。
- import は `wasi:cli/stdout`・`stderr`・`exit` が常に入る。実測では、stdout / stderr が使う型だけの interface `wasi:cli/types`（`error-code` 列挙のみ、関数なし）も入る（spec には書かれていない）。`wasi:cli/environment`、`wasi:random/random`、`wasi:clocks/system-clock` は、`Std.Env`・`Std.Random`・`Std.Clock` に到達したときだけ入る。HTTP サービスでない限り、それ以外は import しない。
- `Std.FileSystem`、`Std.Http` のクライアント、`Clock.sleep` は WASM ではまだ使えない。
- ユーザーが書く `@export(wasm, ...)` と `@external(wasm, ...)` は WASM ではコンパイルエラー。独自 WIT の宣言も未対応（upstream open question #56）。**ホストと関数単位でやり取りする手段は現時点でない。**
- `emela run` は `wasmtime run -S p3=y main.wasm` を実行する。Wasmtime 49 以上が必要。
- v0.26 で `Std.Bytes` が入った。
- JSON は std にはまだない（spec では `derive Encode, Decode` で std に入れる方針だが、導出の構文は未確定）。upstream の実験用 Pome `github.com/emela-lang/x/json` は v0.3.1（2026-11-09）で 0.27 に対応した（v0.3.0 は 0.26 向けで、0.27 ではコンパイルできない）。
- 終了コード: 正常 0、error が残れば 1（`error: ...` を stderr へ）、defect が残れば 101。
- 0.y の間はマイナー更新で破壊的変更があり得る（v0.27 でも `Exit` の移動など）。バージョン固定が前提。

### 9.2 受け渡し方式: `mamela-graph/v1`（JSON）

上記より、当面の ABI は **`wasi:cli/run` を実行し、stdout に書かれた JSON 1 文書を読む** 方式に決める。

- Emela 側は `x/json`（v0.3.1、`Json.to_string`）で書く。空白なし、キーは `Map` の順（ソート順）で、出力は決定的。`x/json` が 0.27 に対応するまでの間は `pome/mamela` に同じ形式の小さな書き出しを持っていたが、置き換えても Graph の digest は変わらなかった。std に JSON が入ったら、そちらに移る。
- 文書は `{"schema":"mamela-graph/v1","resources":[...]}` の形。途中で切れた出力は JSON として不正になるため検出できる。
- 数値は整数だけを使う（浮動小数点の表記揺れを避ける）。単位はフィールド名で表す（`memoryMiB` など）。
- Go 側は `encoding/json/v2`（Go 1.27 で標準）で厳格にデコードする。未知のフィールド、重複キー、小数・指数表記の数値、不正な UTF-8、末尾の余分なデータ、未知の `schema` はすべて拒否する。
- 出力サイズに上限を設ける。stderr は診断として保存するが、Graph としては解釈しない。
- Go 側で正規化し直してから digest を取る。Emela の出力の並びに依存しない。
- JSON は `jq` などでそのまま読めるので、人が中身を確かめやすい。
- スキーマは [graph-schema.md](graph-schema.md)、理由コードは [reason-codes.md](reason-codes.md)。WIT による正式な ABI は、upstream が独自 WIT に対応してから再検討する。

### 9.3 評価のサンドボックス

1. `emela build --target wasm` を固定バージョンで実行する（ネットワークなし、lock 必須）。
2. Go が `main.wasm` のトップレベル import をバイナリから読み、`wasi:cli/stdout`・`stderr`・`exit`・`types`（いずれも `@0.3.0`）以外があれば評価せずに拒否する。読めない形式も拒否する。これにより env / random / clock を使った非決定的な評価も弾ける。
3. `wasmtime run -S p3=y` を子プロセスで直接起動する（`emela run` は経由しない）。`--dir`、ネットワーク、`--env` は渡さない。stdin は空にする。
4. 実行時間（`-W timeout`、既定 10 秒）、メモリ（`-W max-memory-size`、既定 256 MiB）、stdout（4 MiB）を制限する。`-S inherit-env=n,inherit-stdin=n,inherit-network=n,tcp=n,udp=n,allow-ip-name-lookup=n,http=n`、`-C cache=n` も付ける。
5. 終了コードが 0 以外、制限超過、JSON として不完全な出力は、すべて評価失敗として扱う。
6. emela のコンパイラ自体はネイティブに動くので、サンドボックスの外にある。ネットワークなし・空の環境変数・時間制限で動かすが、悪意あるソースに対する隔離は今後の課題（§13）。

### 9.4 その他

- Emela の Effect/Handler は構成生成とテストの能力制御に使い、実変更は Go 側に限定する。
- WASM 経路が塞がっても Reconciler を並行開発できるよう、Go 側に `mamela-graph/v1` の JSON を直接読むテスト用アダプタを持つ（同じデコーダを使う）。

## 10. 初回 VM 作成から OS 管理まで

1. Emela で VM、Package、Service を宣言。
2. Go Policy が許可した Proxmox 作成 Operation を、予約範囲の VMID とともに記録してから実行。テンプレートは使わず、固定した cloud image（sha256）を取り込んで作る（[lab.md §4.2](lab.md#42-テンプレートを使わない-vm-の作り方)）。
3. Agent 登録情報を含む cloud-init を渡す。ステップ 2 ではユーザー・SSH 公開鍵・DHCP だけなので Proxmox の cloud-init ドライブで足りる。任意の user-data は Proxmox API では渡せない（snippets はアップロードできない）ため、Agent の導入（ステップ 3）では NoCloud seed ISO を生成して ISO としてアップロードし、CD-ROM に接続する方式を第一候補とする。
4. cloud-init で Agent を導入し、systemd に登録。
5. 一回限りの Token を使って Agent を Enrollment。
6. Agent が自ノードの署名済み Desired Revision を Pull。
7. APT / systemd 状態を観測し、許可された差分だけ実行。
8. 定期的にドリフトを観測し、許可範囲だけ継続修復。

## 11. 初期コード構成（予定）

~~~text
mamela/
├── cmd/
│   ├── mamela/           # CLI / Control Plane
│   └── mamela-agent/     # Pull Node Agent
├── internal/
│   ├── api/
│   ├── graph/            # mamela-graph/v1 parser, canonicalization, digest
│   ├── component/        # WASM component import reader / allowlist
│   ├── evaluate/         # emela build / wasmtime sandbox / decode
│   ├── state/            # observation snapshots
│   ├── policy/           # Auto / ApprovalRequired / Denied
│   ├── plan/             # pure planner + human rendering
│   ├── store/            # SQLite
│   ├── reconcile/        # apply with journal, leases, re-observation
│   ├── provider/fake/    # in-memory provider with fault injection
│   ├── proxmox/          # Proxmox API client (read-only for now)
│   ├── lab/              # lab site, bootstrap script, preflight / postflight
│   ├── provider/proxmox/
│   └── agent/
│       ├── pull/
│       └── resource/
│           ├── apt/
│           └── systemd/
├── pome/mamela/          # Mamela 用 Emela Pome（Graph 構築と出力）
├── scripts/            # install-toolchain.sh / check.sh
├── lab/                  # 検証環境の宣言（サイト固有値は gitignore）
├── examples/homelab/
├── docs/
└── README.md
~~~

各 Handler は Observe / Plan / Apply に分離する。依存方向はドメイン → Interface → Adapter とし、テストで偽 Provider と Fake Clock を差し込めるようにする。

## 12. MVP 完了条件

- Emela の宣言から VM と nginx の Resource Graph を生成でき、同じ入力で 2 回評価して digest が一致する。
- 同じ Revision と Snapshot から同じ Plan digest が得られる（`--replay` で確認）。
- Proxmox 上に Ubuntu VM を新規作成し、重複作成を防げる。
- cloud-init で Agent が自動登録し、mTLS で自ノードの構成を Pull。
- 承認済みパッケージだけ導入し、nginx の Service を Ready にする。
- nginx 手動停止後にドリフト検出・許可された再起動を行う。
- 無許可パッケージ、期限切れ/改ざん Revision、削除要求、`Foreign` リソースへの変更は実行されない。
- Control Plane/Agent の再起動中断後も、誤成功や重複実行を起こさず再観測できる。
- 検証環境で、Mamela 管理外のゲストの設定 digest と稼働状態が実行前後で変わらない（[検証環境](lab.md)）。

## 13. 未決事項

- std の JSON への移行時期。upstream が独自 WIT に対応した後の正式 ABI。
- emela コンパイラ自体の隔離（コンテナ / 別ユーザー / VM 内でのビルド）。
- Node Agent のローカルキャッシュ方式と署名・証明書・鍵のライフサイクル。
- APT 操作の privilege separation と sandbox / 保守スクリプトの扱い。
- 固定した cloud image の更新の手順（新しい release の sha256 をどう確かめて catalog に入れるか。SHA256SUMS の署名の検証）。
- 宣言の所有権粒度（リソース単位 → 必要に応じフィールド単位）。
- SQLite の永続ストレージ・バックアップ・マイグレーション規約。
- 更新失敗・再起動時の確認と rollback をどこまで保証するか。

## 14. 参考資料

- [Emela repository](https://github.com/emela-lang/emela) / [Emela specification](https://github.com/emela-lang/emela/tree/main/spec) / [WASM target](https://github.com/emela-lang/emela/blob/main/spec/targets/wasm.md)
- [Wasmtime](https://docs.wasmtime.dev/)
- [WASI](https://github.com/WebAssembly/WASI)
- [cloud-init](https://cloudinit.readthedocs.io/)
- [Proxmox VE API](https://pve.proxmox.com/pve-docs/api-viewer/) / [User Management](https://pve.proxmox.com/wiki/User_Management)
- [Kubernetes controllers](https://kubernetes.io/docs/concepts/architecture/controller/)

関連: [開発計画](development.md)、[検証環境](lab.md)、[SQLite 採用 ADR](adr/0001-sqlite.md)。
