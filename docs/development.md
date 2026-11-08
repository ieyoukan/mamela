# Mamela — 開発ガイド / 実装ロードマップ

> Draft / 2026-11-08。以下は開発計画であり、記載のコマンドや機能が Mamela で実装済みとは限らない。
> Emela は v0.27.0 を基準にする。

## 進め方の方針

- 不確実性の大きい順に潰す。最大の不確実性は Emela → WASM → Go の経路（ステップ 0）。
- 決定性と人に優しい出力を、最初の PoC から完了条件に入れる。後付けにしない。
- 実機 Proxmox は、既存ゲストを壊さない境界（[検証環境](lab.md)）を作り、読み取りだけの L0 から始める。

## ステップ 0: Emela / Wasmtime の PoC（ローカル）

目的: Emela v0.27 で **副作用なしに、決定的に**構成を評価し、Go で読み取れることを実証する。

1. emela（v0.27.0）と Wasmtime（49 以上の特定バージョン）を、バージョンと checksum を固定して導入する。固定した値はリポジトリに記録する。
2. `Pome.toml`、`Pome.lock`、`src/main.emel` の最小 Pome を作り、`github.com/emela-lang/x/json`（v0.3.0）を依存に入れる。`emela check`、`emela fmt --check`、`emela test` を通す。
3. Graph の型に `Dynamic.Encode` を実装し、`X.Json.encode` で `mamela-graph/v1` の JSON を stdout に書く最小の Emela コードを書く（VM 1 台 + package + service）。`x/json` が v0.27 で動かなければ、その事実を記録して文字列組み立ての代替に切り替える。
4. `emela build --target wasm` で `main.wasm` を作る。
5. Go から:
   - `main.wasm` の import を読み、stdout / stderr / exit 以外があれば拒否する。
   - `wasmtime run -S p3=y` を子プロセスで直接起動する（`--dir`・ネットワーク・`--env` なし、stdin 空）。
   - 時間・メモリ・出力量の制限を付ける（Wasmtime のオプションを実測して決める）。
   - stdout の JSON を厳格にデコードし（未知フィールド・重複キー・末尾データを拒否）、正規化して digest を出す。
6. 負のテスト:
   - `Std.Env` / `Std.Random` / `Std.Clock` を使うと、import 検査で拒否される。
   - 無限ループ、巨大出力、メモリの使いすぎが制限で止まる。
   - 途中で切れた JSON、未知の `schema`、未知のフィールド、重複キー、浮動小数点の数値が拒否される。
7. 決定性の確認: 同じソースを 2 回評価し、Graph digest が一致する。
8. ネットワークなしで `emela build` する方法（依存キャッシュ）を確認する。
9. 分かった制約（ABI、制限オプション、エラー時の挙動）を `docs/` に記録する。`mamela-graph/v1` の JSON スキーマもここで `schemas/` に書く。

**完了条件:** Emela → WASM → Go で、1 件の検証済み Resource Graph を安全に、決定的に受け渡せる。できなければ制約と再現例を `docs/` に残し、`mamela-graph/v1` の JSON を直接読むテスト用アダプタで次へ進む。

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
