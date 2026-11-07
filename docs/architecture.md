# Mamela — アーキテクチャ設計書

> Status: Draft / v0.1 / 2026-11-08。設計の合意事項と未決事項を分けて記載する。
> Mamela は開発初期の構想であり、以下の API・DSL・CLI 例は実装済みを意味しない。

## 1. 目的

Mamela は、[Emela](https://github.com/emela-lang/emela) で宣言したインフラの Desired State を、Go 製 Control Plane と Pull 型 Go Agent が継続的に収束させる IaC ツール。Kubernetes の Reconciliation モデルを Proxmox VM、Linux パッケージ、systemd サービスに拡張する。

最大の目標は **AI が生成した構成でも、制限された権限と決定的な安全ポリシーの下で自動適用できること**。コードの意味や実世界の副作用を完全に予測できるわけではないため、不明な変更は止める。

### 原則

1. **Desired State first**: 宣言は命令列でなく目標状態を表す。
2. **Safe by default**: 削除・置換・権限拡大は自動許可しない。安全性が不明なら停止。
3. **Least authority**: Emela、Control Plane、Provider、Agent の権限を分ける。
4. **Continuous reconciliation**: 宣言と観測の差分を継続的に調整する。
5. **Observable & auditable**: 決定、変更、失敗、再試行の理由を追跡できる。
6. **Explicit ownership**: 宣言から消えたリソースを勝手に削除しない。
7. **Idempotent operations**: 再起動・ネットワーク断・重複配送でも収束可能にする。

## 2. スコープ

MVP: Proxmox VE 上の Ubuntu VM の作成/観測、cloud-init による Agent 配備、Agent の Pull、APT の状態観測と承認済みインストール、systemd サービスの有効化/稼働、差分/承認/履歴、SQLite を用いた単一 Control Plane。Emela → WASM の構成評価経路は早期に PoC を行う。

MVP 対象外: 任意コマンド/任意スクリプト、VM/Volume の自動削除、未知の APT ソースの自動追加、無条件アップグレード、マルチクラウド、HA Control Plane、無停止や完全ロールバックの保証。

## 3. 全体構成

~~~text
AI / Human / Git
      |
   *.emel
      |
 Emela compiler (type / effect check)
      |
 Wasmtime sandbox (Emela WASI 0.3 component)
      |  Resource Graph (versioned, validated)
      v
+-----------------------------------------------------------+
| Go Control Plane                                          |
| API + AuthN/AuthZ + Ownership + Plan + Safety Policy      |
| SQLite: resources, revisions, observations, operations    |
| Proxmox reconciler + Agent config/signing + event log     |
+----------------------+------------------------------------+
          |           ^
   Proxmox API         |  HTTPS Pull / report (mTLS)
          |            |
     Ubuntu VM    Go Agent (per VM)
     cloud-init   APT / systemd / local observation & policy
~~~

**信頼境界:** Emela/WASM は構成グラフを生成するだけで、クラウド API・任意ネットワーク・任意ホストファイルにはアクセスさせない。Go 側は生成物も信頼せず、型付きスキーマ、権限、計画と状態を検証する。Proxmox 操作は Control Plane、OS 操作は VM 内の Go Agent が実行する。

### 技術選定

| 領域 | 採用/方針 |
|---|---|
| DSL | Emela (`.emel`, Pome) |
| DSL 実行 | WASM + Wasmtime CLI の隔離プロセスから検証 |
| Control Plane / CLI | Go |
| 状態ストア | SQLite (WAL、単一 writer を想定) |
| VM Provider | Proxmox API |
| 初期設定 | cloud-init |
| VM 内の変更 | Pull 型 Go Agent、APT、systemd |
| ノード認証 | 初回限定トークン → mTLS 証明書 |
| Agent ローカル永続化 | SQLite または原子的なローカルキャッシュ（検証時に確定） |

### 実行形態

初期リリースは **単一プロセス/単一アクティブ Control Plane + SQLite**。API と Reconciler は同一バイナリに統合可能。SQLite を複数の独立ノードで共有しない。HA と分散ロックが必要になった場合は、ストレージとリーダー選出を別途再設計する（初期段階から PostgreSQL 移行を前提にはしない）。

## 4. リソースモデル

### Emela DSL（構想例 / 実在 API ではない）

~~~emela
fn infrastructure() {
  vm = Proxmox.vm(
    name: "app-server",
    cpu: 4,
    memory: 8192,
    image: "ubuntu-24.04"
  )

  Apt.packages(vm, ["nginx", "git"])
  Systemd.service(vm, name: "nginx", enabled: True, running: True)
}
~~~

この記述は目標状態の定義であり、評価時に `apt install` や Proxmox API を実行しない。

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
  externalID: "pve-1/101"
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
- `conditions`: Ready / Reconciling / Blocked / Degraded と理由。
- `externalID`: Proxmox 等の実リソースとの対応。必ず再観測して確認。
- `deletionPolicy: Retain`: 定義から消しても実体を消さない。

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
- `revisions`: immutable graph / source hash / revision digest / approvals / activation。
- `plans`: changes / target IDs / preconditions / risk decisions / digest。
- `approvals`: plan digest / approver / scope / validity / consumedAt。
- `operations`: operation ID / resource ID / intent / stage / lease / retry / external request/task ID。
- `agents`: node identity / certificate serial / revision / heartbeat。
- `events`: durable append-oriented event log。

可変リソース構造は JSON 列に格納し、UID、Generation、Operation ID 等は独立した列・制約にする。SQLite の JSON 機能を活用しつつ型検証は Go の Schema レイヤで実施する。

### 同時実行・障害復旧

- `resourceVersion` / Generation を用いて競合更新を拒否する。
- リソース/ノードごとに操作を直列化し、実行前に DB に Intent を書く。
- 外部 API と DB を一つのトランザクションにできない。Exactly-once を仮定しない。
- Proxmox タスク ID、VM ID、タグ等で再起動後に再観測し、二重作成を避ける。
- Lease は期限付き。失効後の引継ぎ時も再観測と fencing 相当の仕組みを用いる。
- Status と Event は、実行結果が不明なときに「成功」を推測しない。

## 6. Reconciliation

1. ソース変更を受け付け、Emela を型検査・Effect 検査する。
2. WASM を隔離評価し、Versioned Resource Graph を生成する。
3. Go が Schema、所有権、参照、依存関係、Policy を検証する。
4. Desired/Observed の差から Plan を作り、観測の鮮度と前提条件を付ける。
5. `Auto` / `ApprovalRequired` / `Denied` に分類する。
6. 許可された操作だけを immutable Revision / Plan digest に紐づける。
7. Provider / Agent が実行前に事前条件を再評価し、実行する。
8. 結果・Conditions・Events を書き戻す。ドリフト時は再計画する。

VM Ready → Agent enrolled/healthy → NodePackage Ready → NodeService Ready の依存グラフで進める。Agent が未登録でも VM を作り直さない。APT 操作はノードごとに直列化する。失敗時は指数 Backoff + jitter と回数制限を使う。

### Pull 型 Agent

- Agent は自ノード宛ての構成のみ HTTPS で Pull する。
- 普段は定期 Pull（初期案 30–60 秒 + jitter）。Long Poll は後から追加可能。
- Revision にノード ID、digest、有効期限、許可操作を含め、署名を検証する。
- Agent は実状態を観測し、ローカル Policy と前提条件を確認してから操作する。
- 結果・観測・heartbeat を Control Plane に返す。
- オフライン時は新規の高リスク処理を行わない。キャッシュを使った修復は有効期限と許可済み操作範囲に限定する。
- Policy を Agent に送る場合も、Agent 自体に変更不能な禁止操作を持たせる。ネットワークからの命令だけで制限を緩和できないようにする。

## 7. セキュリティ / AI 自動適用

### 2段階のポリシー

**構成評価の安全性:** Emela の型と Effect は「構成評価でどの能力を要求するか」を表す。WASM への Host API、ネットワーク、ファイル、CPU/メモリ/時間/出力量を明示的に制限する。コンパイル成功はインフラ変更の安全性を意味しない。

**実操作の安全性:** Go Policy Engine が操作種別、変更量、所有権、承認、実状態を評価し、Agent/Provider が実行直前にも検証する。生成されたコードや Plan 自体を権限の正本としない。

| 判定 | 例 | デフォルト |
|---|---|---|
| Auto | 読み取り、明示許可されたサービス再起動 | 事前条件を満たした場合のみ |
| ApprovalRequired | 初回パッケージ追加、VM サイズ変更、サービス停止、Firewall 拡大 | 承認待ち |
| Denied | VM/Volume 削除、破壊的置換、任意 shell 実行、未知の APT source | 拒否 |

APT インストールは maintainer script の特権実行を含み得るため「追加だから安全」としない。許可済み apt source / package/version、signature verification、変更対象の検証と承認が必要。サービスを起動するだけでも副作用はあり得る。

### 削除と承認

- 未宣言 → `Unmanaged/Orphaned` として保持。`delete` は別の明示行為。
- 承認は Plan digest、対象 ID、操作範囲、期限に結びつけ、変更で失効する。
- Agent は署名不正、期限切れ、対象ノード不一致、未知の操作を拒否する。
- Provider credential、Agent secret、署名鍵は Resource Graph/ログに含めず、安全な権限境界で渡す。
- Rollback は best-effort。不可逆な OS / DB 操作は安全性を保証できない。
- Control Plane 自身の乗っ取り、管理者権限濫用、供給元侵害は WASM だけでは防げない。最小権限・監査・強制的な拒否ルールを合わせる。

## 8. API / CLI（設計案）

~~~text
POST /v1/agents/enroll             # 一回限り短命 Bootstrap Token
GET  /v1/nodes/{id}/desired        # ETag による変更検出、将来 Long Poll
POST /v1/nodes/{id}/observations
POST /v1/nodes/{id}/events
POST /v1/nodes/{id}/heartbeat
~~~

初期登録では Token を Proxmox VM ID と期待ノードに紐づけ、再利用を拒否。以降は mTLS と証明書ローテーション・失効を導入。Node ID は URL だけでなく証明書主体と照合する。

~~~bash
mamela get vms
mamela get nodes
mamela describe vm app-server
mamela diff
mamela events --node app-server
mamela approve <plan-id>
mamela reconcile app-server
~~~

CLI 名は `mamela`、Node Agent は `mamela-agent`（いずれも未実装）。

## 9. Emela / WASM 技術方針

Emela v0.25.0 の README では、JS / C / WASM ターゲットがあり、WASM は WASI 0.3 Component として Wasmtime 49+ を使う。現段階で JSON / Bytes は標準機能として未搭載。したがって **Go と Emela 間の構造化データ交換 ABI は未決定**。

1. `emela build --target wasm` と `emela run --target wasm` を最小プログラムで検証。
2. Wasmtime CLI を隔離した子プロセスで起動し、ネットワーク / ホスト FS を与えずに出力を収集。
3. まずは versioned textual protocol 等による最小 Resource Graph の受け渡しを PoC。仕様確定後に WIT/正式 ABI を検討。
4. タイムアウト、メモリ、出力サイズの限界を実測・設定。未確認のランタイム API を実装済みとして扱わない。
5. Emela の Effect/Handler は構成生成またはテストの能力制御に使用し、実変更は Go 側へ限定する。
6. WASM 経路が塞がった場合も Reconciler を並行開発できるよう Go 側に Graph 入力のテスト用アダプタを持つ。

## 10. 初回 VM 作成から OS 管理まで

1. Emela で VM、Package、Service を宣言。
2. Go Policy が許可した Proxmox 作成 Operation を記録して実行。
3. cloud-init で Agent を導入し、systemd に登録。
4. 一回限りの Token を使って Agent を Enrollment。
5. Agent が自ノードの署名済み Desired Revision を Pull。
6. APT / systemd 状態を観測し、許可された差分だけ実行。
7. 定期的にドリフトを観測し、許可範囲だけ継続修復。

## 11. 初期コード構成（予定）

~~~text
mamela/
├── cmd/
│   ├── mamela/           # CLI / Control Plane
│   └── mamela-agent/     # Pull Node Agent
├── internal/
│   ├── api/
│   ├── graph/
│   ├── runtime/          # Emela / Wasmtime
│   ├── store/            # SQLite
│   ├── planner/
│   ├── policy/
│   ├── reconciler/
│   ├── provider/proxmox/
│   └── agent/
│       ├── pull/
│       └── resource/
│           ├── apt/
│           └── systemd/
├── schemas/
├── examples/homelab/
├── docs/
└── README.md
~~~

各 Handler は Observe / Plan / Apply に分離する。依存方向はドメイン → Interface → Adapter とし、テストで偽 Provider と Fake Clock を差し込めるようにする。

## 12. MVP 完了条件

- Emela の宣言から VM と nginx の Resource Graph を生成可能。
- Proxmox 上に Ubuntu VM を新規作成し、重複作成を防げる。
- cloud-init で Agent が自動登録し、mTLS で自ノードの構成を Pull。
- 承認済みパッケージだけ導入し、nginx の Service を Ready にする。
- nginx 手動停止後にドリフト検出・許可された再起動を行う。
- 無許可パッケージ、期限切れ/改ざん Revision、削除要求は実行されない。
- Control Plane/Agent の再起動中断後も、誤成功や重複実行を起こさず再観測できる。

## 13. 未決事項

- Emela の WASM 出力と Go 間の安定した Graph ABI。
- Node Agent のローカルキャッシュ方式と署名・証明書・鍵のライフサイクル。
- APT 操作の privilege separation と sandbox / 保守スクリプトの扱い。
- Provider の外部 API での idempotency / fencing。
- 宣言の所有権粒度（リソース単位 → 必要に応じフィールド単位）。
- SQLite の永続ストレージ・バックアップ・マイグレーション規約。
- 更新失敗・再起動時の確認と rollback をどこまで保証するか。

## 14. 参考資料

- [Emela repository](https://github.com/emela-lang/emela) / [Emela specification](https://github.com/emela-lang/emela/tree/main/spec)
- [Wasmtime](https://docs.wasmtime.dev/)
- [WASI](https://github.com/WebAssembly/WASI)
- [cloud-init](https://cloudinit.readthedocs.io/)
- [Kubernetes controllers](https://kubernetes.io/docs/concepts/architecture/controller/)

関連: [開発計画](development.md)、[SQLite 採用 ADR](adr/0001-sqlite.md)。
