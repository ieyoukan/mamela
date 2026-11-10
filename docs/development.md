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

**x/json への移行 (2026-11-09):** upstream の `x/json` が v0.3.1 で 0.27 に対応したので（emela-lang/x#29）、`pome/mamela` の自前の JSON 書き出しを置き換えた。Graph の digest は変わらなかった。依存は `scripts/install-toolchain.sh` が proxy と sumdb を通して `.tools/pomes` に取得し、評価はそこからオフラインでビルドする。`emela build` がキャッシュを lock と照合しないことが分かったので、評価の前に `emela pome verify` を実行する（`MAM-EVAL-0103`。書き換えたキャッシュを拒否するテストと、verify を外すとそのテストが落ちることで確認）。

## ステップ 1: Go のドメインと SQLite（ローカル） — 完了 (2026-11-09)

- `internal/state`: 観測スナップショット。`asOf` を自分で持ち、計画は壁時計を読まない。digest はタイムゾーンや挿入順に依存しない。
- `internal/policy`: 決定的な判定（Auto / ApprovalRequired / Denied）と理由コード。知らない操作は Denied。
- `internal/plan`: `Build(graph, snapshot, policy)` は純粋関数。差分・操作・判定・待ち理由・保持（Retain）を出し、Plan digest は全入力を含む。`Render` で人が読める表示。
- `internal/store`: SQLite（WAL、`modernc.org/sqlite`）。revision / snapshot / policy / plan は digest で保存し、読み出すたびに中身と digest を照合する。Plan は保存された文字列を信じず、入力から再計算して一致を確かめる。承認は Plan digest に紐づき期限付き。操作ジャーナルは Intended → Started（lease）→ Succeeded / Failed / Unknown。
- `internal/reconcile`: 実行直前に再観測し、Plan の前提と違うリソースには触れない。Denied は承認があっても実行しない。結果不明なら止め、次回は観測で完了を確かめてから再試行する。
- `internal/provider/fake`: 失敗の注入（変更前の失敗、応答の喪失）ができるメモリ上の Provider。
- CLI: `mamela plan [--snapshot file] <pome>`、`mamela plan --replay <digest>`、`mamela approve <digest>`。

**完了条件の確認:** DB と Fake Provider だけで、正常な収束（承認 → 作成 → 導入・起動 → 一致）、Blocked（古い観測）、応答喪失・変更前の失敗・変更後のクラッシュ・変更前のクラッシュからの復旧、並行実行で作成が 1 回だけになることをテストで再現した。Reconciler の 5 つの安全装置（Denied、承認、前提の照合、観測による確認、claim）は、それぞれ外すとテストが落ちることを確認した（ミューテーションテスト）。同じ入力から同じ Plan digest が出る。

## ステップ 2: Proxmox（検証環境 L0 → L1）

**L0 の道具は用意済み (2026-11-09):** `mamela lab bootstrap`（root 用スクリプトの生成のみ）、`mamela lab preflight` / `postflight`（読み取り専用）、`internal/proxmox`（GET しか送れない API クライアント、証明書の fingerprint 固定）。次は人が bootstrap を実行し、L0 を実機で回す。

**L0 を実機で通した (2026-11-09):** bootstrap の後、preflight（quorum、ノード、Token の権限が境界内、資源の余裕、予算）と postflight（管理外ゲストの不変）がすべて通った。Token の秘密ファイルが group / others から読める間は preflight が止まることも実機で確かめた。

**L1 の道具を実装した (2026-11-09):** `internal/provider/pve`（テンプレートなしで VM を作る Provider。[lab.md §4.2](lab.md#42-テンプレートを使わない-vm-の作り方)）、Proxmox クライアントの書き込み（GET / POST / PUT だけ。DELETE は送れない）、VMID の予約（`store` の `vmids` 表）、`mamela plan --site`（実機を観測して計画）、`mamela apply --site`（preflight → 再観測して実行 → postflight）、L1 用の宣言 `examples/lab-vm`。偽の Proxmox API に対して、応答の喪失・作成タスクの未完了・作成の失敗・予約した VMID の横取り・マーカーだけの VM・予算外の要求をテストした。安全装置（VMID の予約、作成中の VM を「まだない」と見ること、予約のない VM の拒否、応答喪失を結果不明とすること）は、外すとテストが落ちることを確認した。

**L1 を実機で実行した (2026-11-10):** 2 回目の実行で、lab ノードにテンプレートなしで VM を 1 台作成・起動でき、SSH でログインできた（Ubuntu 24.04、cloud-init のユーザーと鍵、DHCP、ディスク 10 GiB に拡張済み）。postflight で管理外のゲストが変わっていないことを確かめた。実機で分かったこと:

- 作成時にタグを付けると、ディスク複製のあとで 403 になり VM が消える（Proxmox はタグの権限を pool に入れる前に確かめる）。完成時に付けるように直した。
- 操作用 Token に `Pool.Audit` がないと `/cluster/resources` が pool を返さず、作った VM を自分のものと見分けられない（plan がまた作成を出した）。権限に加え、pool が分からないときは観測をエラーにし、足りない権限を preflight（LAB-0104）で検出するようにした。
- image の複製は lab ノードのディスクで遅い（1 回目は約 20 分）。待ち時間を延ばし、作成タスクを Proxmox のタスク一覧から追って、結果不明で止まる場面を減らした。

前提: [検証環境](lab.md) の一度だけの準備（pool・ロール・Token、cloud image 用ストレージの `import`）を人が実行済み。

- L0: Token で読み取りだけ行う。preflight / postflight（資源の余裕、権限の範囲、管理外ゲストの不変性）を実装し、何も作らずに回す。
- Token の最小権限を実測して `docs/lab.md` の表を確定する。
- L1: API の read / create を実装。VMID 予約範囲・タグ・所有マーカーで管理する。
- VMID と Intent を先に保存し、Timeout や Control Plane 再起動で重複 VM を作らない。
- cloud-init: L1 は Proxmox の cloud-init ドライブ（ユーザー・SSH 公開鍵・DHCP）。任意の user-data が要る Agent の導入（ステップ 3）で NoCloud seed ISO を検証する。
- 削除、ディスク縮小、VM 置換を実装しない（API で拒否）。

**完了条件:** 予算内の Ubuntu VM を 1 台作成し、障害再実行でも 2 台にならない。postflight で管理外ゲストに変化がない。

## ステップ 3: Pull 型 Go Agent（ローカル → 検証環境 L2）

**実装した (2026-11-10):** Agent の名前は mamelet。`internal/noderev`（署名付き revision と報告の形式）、`internal/mamelet` と `cmd/mamelet`（agent）、`internal/cp`（一回限りのトークンによる登録、自前の CA による mTLS、revision の署名と配信、報告の検証）、`internal/provider/node`（報告を観測にし、承認された操作を一度きりの許可として revision に加え、結果を待つ）、`internal/seed`（NoCloud seed ISO）、`mamela serve`、`mamela lab tunnel`。APT はインストールの変更集合（apt-get -s）を plan に載せて承認の対象にし、mamelet は実行直前の再シミュレーションと一致したときだけ入れる。ローカルの QEMU は使わず、偽の OS と偽の Control Plane で通しのテストをしたうえで、L2 は実機で確かめる。Ubuntu 24.04 の実機で dpkg-query・apt-get -s・systemctl show の出力形式と、seed ISO が `LABEL=CIDATA` として読めることを確かめた。

**L2 を実機で実行した (2026-11-10):** `examples/lab-l2` で、seed 付きの VM `mamela-l2` をテンプレートなしで作成した。cloud-init が qemu-guest-agent と mamelet を入れ、mamelet は `mamela lab tunnel`（踏み台ホスト経由の ssh -R）越しに一度きりのトークンで登録し、mTLS で revision を取得した。mamelet が報告した `apt-get -s install nginx` の変更集合（nginx と nginx-common 1.24.0-2ubuntu7.19、Ubuntu の公式 origin）を plan に載せて承認し、mamelet が再シミュレーションと一致を確かめてから一度だけインストールした。Ubuntu の nginx はインストール時に有効化・起動されるので、サービスの宣言もそのまま満たされ、plan は in sync になった。postflight では管理外のゲストに変化がなかった。権限は権限カタログから導いたもの（余分を外した版）で足りた。

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
