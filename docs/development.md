# Mamela — 開発ガイド / 実装ロードマップ

> Draft / 2026-11-08。以下は開発計画であり、記載のコマンドや機能が Mamela で実装済みとは限らない。
> Emela は v0.27.0 を基準にする。

## 進め方の方針

- 不確実性の大きい順に潰す。最大の不確実性は Emela → WASM → Go の経路（ステップ 0）。
- 決定性と人に優しい出力を、最初の PoC から完了条件に入れる。後付けにしない。
- 実機 Proxmox は、既存ゲストを壊さない境界（[検証環境](lab.md)）を作り、読み取りだけの L0 から始める。

## ステップ 0: Emela / Wasmtime の PoC（ローカル） — 完了 (2026-11-08)

目的: Emela v0.27 で **副作用なしに、決定的に**構成を評価し、Go で読み取れることを実証する。結果と分かった制約は [emela-wasm.md](emela-wasm.md)。

- `scripts/install-toolchain.sh`: emela v0.27.0 と Wasmtime v49.0.5 を sha256 固定で `.tools/` に導入。
- `pome/mamela`: Graph を組み立てて `mamela-graph/v1` の JSON を stdout に書く Emela ライブラリ（JS / WASM の両方でテスト）。
- `examples/homelab`: VM 1 台 + nginx の宣言例。
- `internal/component`: WASM コンポーネントのトップレベル import を読み、許可リストで検査。
- `internal/evaluate`: オフラインビルド → import 検査 → 制限付き実行 → 厳格なデコード。拒否・制限のテスト用 Pome は `internal/evaluate/testdata/`。
- `internal/graph`: 厳格なデコード、意味の検証、正規化、digest。
- `mamela eval [--twice] [--json] <pome>`: 人が読める要約、または正規化済み JSON。
- `scripts/check.sh` と CI: 上記すべてのテストと、例の決定性チェック。

**完了条件の確認:** Emela → WASM → Go で 1 件の検証済み Resource Graph を受け渡せた。2 回の評価で graph digest と `main.wasm` の digest がともに一致した。env / random / clock を使う宣言は実行前に拒否され、無限ループ・巨大出力・メモリの使いすぎは制限で止まることをテストで確認した。

## ステップ 1: Go のドメインと SQLite（ローカル）

- `Resource`、`Revision`、`Observation`、`Snapshot`、`Plan`、`Operation`、`PolicyDecision` の型とバリデーション。
- SQLite の migration、WAL、永続化、revision CAS、観測タイムスタンプ。
- Planner は純粋関数: (Revision, Snapshot, Policy) → Plan。壁時計を読まない。出力順は安定。
- `mamela plan` の表示（差分・判定・理由コード・解消方法）と `--replay`。
- 偽 Provider で Observe → Diff → Decide → Execute → Record の Reconciler。
- 更新競合、操作途中クラッシュ、観測不明、リトライ/バックオフのテスト。

**完了条件:** DB と Fake Provider のみで正常収束・Blocked・再起動復旧を再現できる。同じ入力から同じ Plan digest が出る。Plan の表示を人が読んで理由が分かる。

## ステップ 2: Proxmox（検証環境 L0 → L1）

前提: [検証環境](lab.md) の一度だけの準備（pool・ロール・Token・テンプレート）を人が実行済み。

- L0: Token で読み取りだけ行う。preflight / postflight（資源の余裕、権限の範囲、管理外ゲストの不変性）を実装し、何も作らずに回す。
- Token の最小権限を実測して `docs/lab.md` の表を確定する。
- L1: API の read / create を実装。VMID 予約範囲・タグ・所有マーカーで管理する。
- VMID と Intent を先に保存し、Timeout や Control Plane 再起動で重複 VM を作らない。
- cloud-init: NoCloud seed ISO を生成・アップロードして CD-ROM に接続する方式を検証する。
- 削除、ディスク縮小、VM 置換を実装しない（API で拒否）。

**完了条件:** 予算内の Ubuntu VM を 1 台作成し、障害再実行でも 2 台にならない。postflight で管理外ゲストに変化がない。

## ステップ 3: Pull 型 Go Agent（ローカル → 検証環境 L2）

- まずローカルの QEMU + HVF（Ubuntu arm64 cloud image + NoCloud seed ISO）で開発する。
- Enrollment 用のワンタイム Token と mTLS。
- `GET /v1/nodes/{id}/desired`、観測・イベント・Heartbeat。
- APT: 状態観測、`apt-get -s` による変更集合の報告、承認された集合だけ導入、実行直前の再シミュレーション。
- systemd: enabled / running の検証と許可された変更。
- Revision の署名、ノード紐付け、期限、ローカル拒否ルール。
- その後、検証環境 L2 で実機の Proxmox VM に対して同じことを確認する。

**完了条件:** nginx の導入と手動停止後の収束、期限切れ/不正 Revision の拒否。

## ステップ 4: AI 用自動適用

- Risk Policy: Auto / ApprovalRequired / Denied と理由コードの一覧。
- 承認と Plan digest の結合、Plan 変更時の失効。
- Resource/Field Ownership と安全な管理解除。
- 監査イベント、運用メトリクス、異常停止後の整合性検査。
- 検証環境 L3: 検証環境の VM を Emela で宣言し、Mamela で管理する。

**完了条件:** AI が誤った宣言を出しても禁止操作が実行されず、理由を記録できる。危険でないと証明できない操作は自動適用しない。

## テストの優先順位

1. 非許可の操作と任意 shell コマンドを拒否する。
2. 定義から削除しても実リソースは保持される。
3. `Foreign`（Mamela 管理外）のリソースを変更しない。
4. 同時に二つの Reconciler が同じリソースを更新しない。
5. Provider 応答前後にクラッシュしても重複作成を防ぐ。
6. Plan と実状態がずれたら停止・再計画する。
7. 不正/期限切れ/別ノード向け Revision を Agent が拒否する。
8. ネットワーク断で期限切れ構成を勝手に適用しない。
9. 評価と計画が決定的である（同じ入力 → 同じ digest）。

## 初期実装ポリシー

- 最初から大規模なディレクトリ雛形を作らず、動く PoC に沿って追加。
- Emela の未実装 API を仮定しない。未知の仕様は `emela-spec` と upstream `spec/` を参照。
- 無許可の外部コマンド、APT/VM の削除、自動の破壊的操作をテスト対象外にしない。
- 新機能には少なくとも正常系・拒否系・障害再試行のテストを付ける。
- 時刻・乱数・map の反復順に依存するコードを、計画と評価の経路に入れない。時刻は Fake Clock か Snapshot から受け取る。
- Go: `go test ./...`、Emela: `emela check` / `emela test`、`emela fmt --check`（対象ファイルがあるとき）。
- 実 Proxmox に対するテストは `MAMELA_LAB=1` の明示的な opt-in とし、CI の標準ジョブでは実行しない。

詳しい設計は [architecture.md](architecture.md)、検証環境は [lab.md](lab.md)。SQLite の根拠は [ADR 0001](adr/0001-sqlite.md)。
