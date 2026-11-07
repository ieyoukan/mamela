# Mamela — 開発ガイド / 実装ロードマップ

> Draft。以下は開発計画であり、記載のコマンドや機能が Mamela で実装済みとは限らない。

## ステップ 0: Emela と Wasmtime の PoC

目的: Emela の現行バージョンで **副作用を与えずに型付き構成を評価し、Go 側で読み取れるか**を実証する。

1. [Emela の現在の README](https://github.com/emela-lang/emela) と `spec/` を確認する（0.1x の古い仕様と混同しない）。
2. Emela CLI を導入し、`Pome.toml` と `src/main.emel` の最小 Pome を作成。
3. `emela check`、`emela fmt --check`、`emela test` を実行。
4. `emela build --target wasm` の出力と `emela run --target wasm` の実行を確認（Wasmtime 49+、WASI 0.3 設定が必要）。
5. 最小のリソース記述を静的な出力へ変換し、Go でパース/検証する PoC。
6. 実行に不要なネットワーク・ホスト FS・秘密情報を渡さず、制限が効いていることを負のテストで確認。
7. 実用 ABI（WIT、export、出力形式、シリアライズ）の可否を記録し、仕様が定まるまでは特定方式に固定しない。

**完了条件:** Emela → WASM → Go で 1 件の検証済み Resource Graph を安全に受け渡せる。できなければ制約と再現例を `docs/` に残し、Go の Fake Graph で次の開発へ進む。

## ステップ 1: Go のドメインと SQLite

- `Resource`、`Revision`、`Observation`、`Plan`、`Operation`、`PolicyDecision` の型とバリデーション。
- SQLite の migration、WAL、永続化、revision CAS、観測タイムスタンプ。
- 偽 Provider で Observe → Diff → Decide → Execute → Record の Reconciler。
- 更新競合、操作途中クラッシュ、観測不明、リトライ/バックオフのテスト。

**完了条件:** DB と Fake Provider のみで正常収束・Blocked・再起動復旧を再現できる。

## ステップ 2: Proxmox

- API の read / create と明示的 ID / タグでの管理を実装。
- cloud-init に Agent 登録情報を注入。
- 外部タスク ID と Intent を保存し、Timeout や Control Plane 再起動で重複 VM を作らない。
- 削除、ディスク縮小、VM 置換を実装しない（API で拒否）。

**完了条件:** Ubuntu VM を 1 台作成し、障害再実行でも 2 台にならない。

## ステップ 3: Pull 型 Go Agent

- Enrollment 用のワンタイム Token と mTLS。
- `GET /v1/nodes/{id}/desired`、観測・イベント・Heartbeat。
- APT: 状態観測、承認済みソース/パッケージ/バージョンのみ導入。
- systemd: enabled / running の検証と許可された変更。
- Revision の署名、ノード紐付け、期限、ローカル拒否ルール。

**完了条件:** nginx の導入と手動停止後の収束、期限切れ/不正 Revision の拒否。

## ステップ 4: AI 用自動適用

- Risk Policy: Auto / ApprovalRequired / Denied。
- 承認と Plan digest の結合、Plan 変更時の失効。
- Resource/Field Ownership と安全な管理解除。
- 監査イベント、運用メトリクス、異常停止後の整合性検査。

**完了条件:** AI が誤った宣言を出しても禁止操作が実行されず、理由を記録できる。危険でないと証明できない操作は自動適用しない。

## テストの優先順位

1. 非許可の操作と任意 shell コマンドを拒否する。
2. 定義から削除しても実リソースは保持される。
3. 同時に二つの Reconciler が同じリソースを更新しない。
4. Provider 応答前後にクラッシュしても重複作成を防ぐ。
5. Plan と実状態がずれたら停止・再計画する。
6. 不正/期限切れ/別ノード向け Revision を Agent が拒否する。
7. ネットワーク断で期限切れ構成を勝手に適用しない。

## 初期実装ポリシー

- 最初から大規模なディレクトリ雛形を作らず、動く PoC に沿って追加。
- Emela の未実装 API を仮定しない。未知の仕様は `emela-spec` と upstream `spec/` を参照。
- 無許可の外部コマンド、APT/VM の削除、自動の破壊的操作をテスト対象外にしない。
- 新機能には少なくとも正常系・拒否系・障害再試行のテストを付ける。
- Go: `go test ./...`、Emela: `emela check` / `emela test`、`emela fmt --check`（対象ファイルがあるとき）。
- 実 Proxmox に対する書き込みを伴うテストは明示的な opt-in とし、CI の標準ジョブでは実行しない。

詳しい設計は [architecture.md](architecture.md)。SQLite の根拠は [ADR 0001](adr/0001-sqlite.md)。
