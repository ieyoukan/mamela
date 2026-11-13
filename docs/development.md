# Mamela — 開発ガイド / 実装ロードマップ

> Draft / 2026-11-08。以下は開発計画であり、記載のコマンドや機能が Mamela で実装済みとは限らない。
> Emela は v0.27.0 を基準にする。

## 進め方の方針

- 不確実性の大きい順に潰す。最大の不確実性は Emela → WASM → Go の経路（ステップ 0）。
- 決定性と人に優しい出力を、最初の PoC から完了条件に入れる。後付けにしない。
- 実機は、境界の外を壊さないことを確かめる guard を持つプロバイダーを通して、読み取りだけの段階から試す（[検証環境](lab.md)）。Proxmox の境界と手順は [mamela-proxmox](https://github.com/ieyoukan/mamela-proxmox) にある。

## 開発環境とバージョンの固定

使う版は、すべてリポジトリのファイルで決まる。CI も手元も同じファイルを読む。

| 対象 | 固定するファイル | 更新 |
|---|---|---|
| 開発ツール（Go・Node.js・emela・wasmtime） | `mise.toml` と `mise.lock`（プラットフォームごとの URL と checksum） | `mise lock --platform linux-x64,macos-arm64` |
| Go | `go.mod` の `toolchain` 行（mise を使わなくても、GOTOOLCHAIN がこの版を取ってくる） | 手で |
| Go のモジュール | `go.sum`（CI で `go mod verify` と `go mod tidy -diff`） | Dependabot |
| emela・wasmtime | `toolchain.lock`（アーカイブと実行ファイルの sha256） | 手で |
| Emela の依存 | 各 `Pome.lock`（取得時に sumdb、評価前に `emela pome verify`） | `emela pome` |
| GitHub Actions | `ci.yml` に commit SHA で固定 | Dependabot |

`mise.toml` の go は go.mod の toolchain 行と、emela と wasmtime は `toolchain.lock` と同じでなければならず、`mise.lock` の emela と wasmtime の checksum も `toolchain.lock` のアーカイブの sha256 と一致しなければならない（`TestMiseMatchesTheLocks`）。

- **`toolchain.lock`:** Mamela は、この lock と sha256 が一致する emela と wasmtime しか実行しない。版の表示ではなく実行ファイルそのものを照合するので、`.tools/bin` が書き換えられていても実行前に止まる（`MAM-EVAL-0001`）。lock は `mamela` のバイナリに埋め込まれる。
- **入れ方:** リポジトリでは `mise install` がツールを入れたあと、postinstall フックで `scripts/install-toolchain.sh` を実行し、`.tools/` に入れる（PATH にある固定版はダウンロードせずにコピーする）。mise を使わなければスクリプトを直接実行する。CI も `jdx/mise-action` で同じ手順を踏み、`MISE_LOCKED=1` で lock にないものを入れない。`go install` で入れた利用者は `mamela toolchain install` でユーザーのキャッシュに入れる（版ごとに別のディレクトリ）。どちらも同じ Go の実装で、アーカイブと実行ファイルの両方を lock と照合する。
- **見つけ方:** `mamela eval` と `plan` は、`--toolchain`、`MAMELA_TOOLCHAIN_DIR`、今いるディレクトリから上にたどった最初の `.tools/bin`、PATH（固定版と sha256 が一致するときだけ。mise で入れたもの）、ユーザーのインストールの順に探す。Pome キャッシュも、`--pome-cache`、`EMELA_POME_CACHE`、見つけた `.tools` の隣の `pomes`、`~/.emela/pomes` の順に決まる。`mamela toolchain` で、どれが選ばれたかと照合の結果を確かめられる。
- **emela・wasmtime の更新:** `toolchain.lock` の版、各プラットフォームの url、アーカイブの sha256（リリースページの digest）、中の実行ファイルの sha256（`bin`）を書き換え、`mise.toml` の版を合わせて `mise lock --platform linux-x64,macos-arm64` で `mise.lock` を作り直し、`mise install && mise run check` を通す。例の graph digest がテストに固定されているので、評価結果が変わればそこで分かる。
- **2026-11-10:** Wasmtime を 49.0.5 から 50.0.1 に上げた。全テストが通り、例の graph digest も変わらなかった。Node.js は 26（Active LTS）、CI のランナーは ubuntu-26.04 と macos-26（arm64）にした。開発ツールは mise に統一し、CI も `mise install` で入れる。

## ステップ 0: Emela / Wasmtime の PoC（ローカル） — 完了 (2026-11-08)

目的: Emela v0.27 で **副作用なしに、決定的に**構成を評価し、Go で読み取れることを実証する。結果と分かった制約は [emela-wasm.md](emela-wasm.md)。

- `scripts/install-toolchain.sh`: emela v0.27.0 と Wasmtime v49.0.5 を sha256 固定で `.tools/` に導入（のちに `toolchain.lock` へ移した。下の「開発環境」）。
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

> ADR 0006（2026-11-11）で、ステップ 2・3 と下の節に出てくる `mamela lab`（bootstrap、preflight、postflight、permissions、cleanup、tunnel）、`mamela destroy`・`reboot`、`internal/proxmox`、`internal/provider/pve`、組み込みの `VirtualMachine`、`examples/lab-vm`・`lab-l2`・`k3s`・`pve` は Mamela 本体から外した。同じ役割は、mamela-proxmox のプロバイダーの guard・明示操作・スクリプトが持つ（下の「Proxmox を本体から外す」）。ここは当時の記録である。

**L0 の道具は用意済み (2026-11-09):** `mamela lab bootstrap`（root 用スクリプトの生成のみ）、`mamela lab preflight` / `postflight`（読み取り専用）、`internal/proxmox`（GET しか送れない API クライアント、証明書の fingerprint 固定）。次は人が bootstrap を実行し、L0 を実機で回す。

**L0 を実機で通した (2026-11-09):** bootstrap の後、preflight（quorum、ノード、Token の権限が境界内、資源の余裕、予算）と postflight（管理外ゲストの不変）がすべて通った。Token の秘密ファイルが group / others から読める間は preflight が止まることも実機で確かめた。

**L1 の道具を実装した (2026-11-09):** `internal/provider/pve`（テンプレートなしで VM を作る Provider。手順は今は mamela-proxmox の `docs/lab.md`）、Proxmox クライアントの書き込み（GET / POST / PUT だけ。DELETE は送れない）、VMID の予約（`store` の `vmids` 表）、`mamela plan --site`（実機を観測して計画）、`mamela apply --site`（preflight → 再観測して実行 → postflight）、L1 用の宣言 `examples/lab-vm`。偽の Proxmox API に対して、応答の喪失・作成タスクの未完了・作成の失敗・予約した VMID の横取り・マーカーだけの VM・予算外の要求をテストした。安全装置（VMID の予約、作成中の VM を「まだない」と見ること、予約のない VM の拒否、応答喪失を結果不明とすること）は、外すとテストが落ちることを確認した。

**L1 を実機で実行した (2026-11-10):** 2 回目の実行で、lab ノードにテンプレートなしで VM を 1 台作成・起動でき、SSH でログインできた（Ubuntu 24.04、cloud-init のユーザーと鍵、DHCP、ディスク 10 GiB に拡張済み）。postflight で管理外のゲストが変わっていないことを確かめた。実機で分かったこと:

- 作成時にタグを付けると、ディスク複製のあとで 403 になり VM が消える（Proxmox はタグの権限を pool に入れる前に確かめる）。完成時に付けるように直した。
- 操作用 Token に `Pool.Audit` がないと `/cluster/resources` が pool を返さず、作った VM を自分のものと見分けられない（plan がまた作成を出した）。権限に加え、pool が分からないときは観測をエラーにし、足りない権限を preflight（LAB-0104）で検出するようにした。
- image の複製は lab ノードのディスクで遅い（1 回目は約 20 分）。待ち時間を延ばし、作成タスクを Proxmox のタスク一覧から追って、結果不明で止まる場面を減らした。

前提: 検証環境の一度だけの準備（pool・ロール・Token、cloud image 用ストレージの `import`）を人が実行済み。

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

**IaC としての検証の続き (2026-11-10):** 同じ VM で次を実機で確かめ、足りなかったものを実装した。

- ドリフトの修復: 手で止めた nginx は、既定の policy では plan に「起動（承認が必要）」が出て、承認後に起動した。`autoStartServices` では mamelet が 26 秒で自分で起動し直した。
- 止める・無効にする宣言: mamelet に一度きりの stop / disable を追加した。serve が古い版のままだと新しい操作を含む revision を署名前に拒否し（500）、apply は結果不明で止まった。直したあとは、一部済んでいた変更（無効化）を観測で確認し、残り（停止）を実行して完了した（reconcile が承認範囲内の残りを続けるようにした）。
- VM の大きさの変更: vCPU とメモリの変更を実装した（動いていれば再起動 1 回で反映）。1 vCPU / 1 GiB / 10 GiB から 2 vCPU / 1.5 GiB / 12 GiB にし、VM の中でも反映を確かめた。plan ではディスクの拡張を先にする。
- Control Plane に届かないとき: トンネルを切っても mamelet は前回の revision で nginx を 20 秒で起動し直した。10 分後、Control Plane の plan はノードのリソースを blocked（観測が古い）にした。
- 冪等性と再現性: 適用済みの plan をもう一度 apply すると「観測で済みと確認」だけになった。plan の再計算は、作った版と今の版が違うと合わない（MAM-REC-0001）ので、作った版を記録して理由を示すようにした。
- 後片付け: `mamela destroy` を実装し、mamela-l1（VMID 7000）と mamela-l2（VMID 7001）を、表示・名前の打ち直し・preflight・postflight つきで消した。アップロードした cloud image と seed は `mamela lab cleanup` のスクリプトを確認して root で消した。lab の VM、ディスク、アップロード、DB の予約と登録はすべてなくなった（pool・ロール・Token は次の検証のために残した）。

検証中に、管理外の VM 212 が root@pam によって削除された（Mamela の Token ではない）。postflight は各実行の前後だけを比べるので、実行と実行の間の変化は対象外である。

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

## プロバイダーの契約（段階 1、2026-11-10）

設計は [ADR 0002](adr/0002-provider-contract.md)。

- **マニフェスト:** Proxmox と mamelet のプロバイダーを、`internal/provider/spec` のマニフェスト（扱う種類、操作、必要な能力）で記述した。Proxmox のマニフェストは、操作ごとの呼び出しと権限カタログから導く。mamelet の能力の名前は、mamelet に与える操作の名前から導く。
- **強制:** 操作の実行中、Proxmox の client は、その操作が宣言した呼び出しとパラメータしか送らない（`proxmox.Restrict`）。宣言から 1 つ消すと、送る前に止まってテストが落ちることを確かめた。
- **分析:** 宣言のリソースごとに、至りうるすべての操作の要求（プロバイダー、能力、対象、条件）を出す（`spec.Providers.Analyze`）。これまでは種類ごとに代表のリソースを 1 つ使っていた。plan では、policy の判定と承認した変更集合で絞る。
- **出力:** 表示用の文章は CLI だけで組み立てる。`mamela check --json`（`mamela-check/v1`）は、診断と要求をデータのまま出す。
- **承認した 1 操作への結びつけ:** Proxmox の操作は、ノード・VMID・ストレージと、承認の意図を担う値（name・cores・memory・pool・tags・disk・size）に結びつく。観測中は変更の要求を送らない。値・ID・読み取り専用の検査は、それぞれ外すとテストが落ちることを確かめた。
- **部分的な成功:** 変更が効いたあとで失敗した操作は partly-done（MAM-REC-0006）になり、次の apply は観測してから続ける。
- **型付きの条件:** 能力は、付与の期間（once／standing）と型付きの条件を持つ。mamelet の install は、承認した変更集合と違う、削除を含む、別の origin、別のノード、別の操作、期限切れのどれでも条件が成り立たないことを、テストで確かめた。
- **plan と契約:** plan はプロバイダーの digest を持ち、契約が変われば apply しない（MAM-REC-0007）。

## プロバイダーの契約（段階 2a、2026-11-10）

外部の開発者が書くプロバイダーを、信頼しないコードとして動かす土台。書き方は [provider-authoring.md](provider-authoring.md)。

- **公開パッケージ:**
  - `provider/protocol`: マニフェスト、JSON-RPC、許可、カタログ。`internal/provider/spec`、`graph.Kind`、`policy.Op` は、この型の別名にした。
  - `provider/sdk`: Go の SDK。
  - `provider/providertest`: 適合テスト。
- **隔離（`internal/provider/host`）:**
  - WASM（Go の wasip1、WASI 0.3 component）は、固定版の wasmtime で、ディレクトリ・環境変数・ネットワークなしに動かす。import は実行前に許可リストで確かめる。
  - ネイティブは、`mamela provider trust` で信頼した実行ファイルだけを動かす。
- **許可（permit）:**
  - observe と lookup は、宣言した読み取りだけを通す。describe と diff は、要求を一切通さない。
  - apply は、承認した変更の要求（パラメータの名前と値まで一致するもの、各 1 回）・対象 ID・前提条件・期限に合う要求だけを通す。パラメータは、カタログが API の読む場所として書いた場所（クエリかフォーム）でしか送れない。
  - 結果が分からない変更や拒否のあとは、同じ apply の変更をすべて止める。リダイレクトには従わない。
  - 検査した実行ファイルのバイト列の写しを実行する。標準入力への書き込みにも期限が効く。
- **結果の判定:** host が要求の記録から、done・failed・partly-done・unknown を決める。プロバイダーの答えには頼らない。
- **確かめたこと:**
  - host: 違反するプロバイダー（`internal/provider/host/testdata/rogue`）の、それぞれの違反を拒否または検出する。
  - 適合テスト: 例のプロバイダー（`examples/kv`）をわざと 8 通りに壊し、すべて検出した。最初の版は lookup の誤りを見逃したので、適用の前と収束の後に lookup を確かめる検査を足した。
    - 宣言から外したものを観測で報告しない
    - 宣言から外したものを diff で変える
    - 再開しない
    - 宣言していないパラメータを送る
    - 失敗を握りつぶす
    - lookup が常に done と答える
    - lookup が常に partly-done と答える
    - lookup がラベルを見ない

## プロバイダーを plan・apply で使う（段階 2b と配布、2026-11-10）

- **import:** プロバイダーの Pome（Emela の型、WASM のプログラム、`mamela-provider.toml`）を、宣言の依存に足して import する。Mamela は、宣言の `Pome.lock`（Pome のキャッシュ）と `[replace]` から、プロバイダーを見つける（`internal/provider/external`）。
- **Graph:** `mamela-graph/v2` で、プロバイダーのリソースを運ぶ（`pome/mamela` v0.2.0 の `Mamela.provided`）。
- **plan:**
  - プロバイダーが観測と diff をする。Mamela は次のことを確かめる。
    - 宣言していないリソースを変えない
    - 前提条件が観測した版である
    - 1 つのリソースに同じ操作が 2 つない
  - カタログによる効果の分類とともに、スナップショットに記録する。
  - plan は、承認する要求をパラメータまで表示する。プログラムを SQLite に保存する。
- **apply:**
  - plan と同じプログラムを動かす。
  - 承認した変更と完全に同じ変更だけを、許可（SQLite に一度だけ記録）のもとで実行する。
  - site がなくても動く（組み込みの種類の変更がないとき）。
- **確かめたこと（`cmd/mamela` の通しのテスト）:**
  - 承認する前は何も変わらない。
  - 承認すると API にその要求だけが届く。
  - もう一度 plan すると in sync になる。
  - plan の後に対象が変わると、MAM-REC-0002 で止まり、新しい値を上書きしない。
- **実機の Proxmox で使った（2026-11-10）:** 外部の WASM のプロバイダー `examples/pve` を、検証環境で動かした。
  - **準備:**
    - preflight は通った。
    - 運用のトークンが見えるのは、lab の pool だけだった。
  - **作成:**
    - plan は、送る要求を 1 つ、パラメータまで表示した。
    - 承認して apply すると、lab の pool に空の VM ができた。
    - 最初の試みは、Proxmox の ACL が作成のタスクを拒否した（pool に入る前のタグの権限）。host はそれを partly-done と判定して止まり、観測で何も残っていないことを確かめた。所有の印をタグから説明欄に移して、作り直した。
  - **変更:** メモリの変更を、観測した config の digest を前提条件にして、承認したとおりに適用した。
  - **保持:** 宣言から消すと、retained になった（削除しない）。
  - **lab の外:** 毎回の postflight で、lab の外のゲストが変わっていないことを確かめた。
- **まだしていないこと（当時）:**
  - Proxmox と mamelet の、この契約への移行（組み込みのプロバイダーを、`builtin:proxmox` のカタログと外部のプロバイダーで置き換える）。ADR 0004 と 0006 で済んだ
  - `pome/mamela` v0.2.0 と、例の Pome の公開（タグの push）

## 軽量な Kubernetes（k3s、2026-11-10）

Ubuntu 24.04 の公式の apt には、kubelet・kubeadm・k3s のどれもない。そのため、パッケージと同じ考え方で、版と sha256 を宣言で固定する組み込みの kind `NodeK3s` を足した。

- **宣言:** 当時は `Mamela.k3s_server(graph, vm, version, sha256)` と `Mamela.k3s_agent(graph, vm, server, version, sha256)`。ADR 0007 で、別の Pome（`pome/k3s`）の `K3s.server(graph, node, version, sha256)` と `K3s.agent(graph, node, server, version, sha256)` に移った（プロバイダーのマシンの `Node` を取る）。例は `examples/k3s`（server 1 台と agent 2 台、traefik と metrics-server なし。ADR 0006 で mamela-proxmox に移した）。
- **クラスタ:**
  - 参加のトークンは、Control Plane がクラスタごとに作り、ノードの revision にだけ入れる。
  - agent の server のアドレスは、server の mamelet の報告から取る。報告するのは既定経路のインターフェースのアドレスだけで、k3s の範囲（10.42/16、10.43/16）は除く。plan に表示し、承認と違えば導入しない。
- **mamelet:**
  - 承認ごとに一度だけ、GitHub のリリースからバイナリを取得し、sha256 が一致したときだけ置く。
  - unit は Mamela が決めた中身で書く。手で変えられた unit は報告して、plan を止める。
  - 起動は、既存の `NodeService` の承認で行う。
- **確かめたこと:**
  - テスト: 承認の前は取得しない、別の sha256 なら置かない、同じ試行を繰り返さない、オフラインでは導入しない、承認から起動までの通し。
  - 検証環境の実機: 1 vCPU・1 GiB の VM 1 台に、3 回の承認（VM の作成、k3s の導入、サービスの有効化と起動）で k3s を入れた。ノードは `Ready` になり、`plan` は in sync になった。毎回の postflight で、lab の外は変わっていない。

## Proxmox を本体から外す（ADR 0006、2026-11-11）

設計は [ADR 0006](adr/0006-proxmox-out-of-core.md)。Mamela 本体のコードは、特定の基盤を知らなくなった。安全装置は、プロバイダーの契約の一部として、どのプロバイダーの作者も書ける。

- **guard:** マニフェストの `guard` と呼び出し `guard`。`mamela apply` は、プロバイダーごとに前後で呼ぶ。前の block があれば何も送らない。後の block は失敗にする。`mamela guard <pome-dir>` は前だけを実行する。plan を作ったときと契約が違えば呼ばない（MAM-REC-0007）。
- **明示操作:** 操作の `explicit`・`adopts`、呼び出し `prepare`、観測の `adoptable`。`mamela act [--confirm <name>] <op> <resource> <pome-dir>`（MAM-ACT-0100–0106、0201、0202）。意図と結果は SQLite の `explicit_operations`（migration 10）に記録する。削除が済めば作成の記録を消し、引き継ぎが済めば記録する。
- **スクリプト:** マニフェストの `scripts` と呼び出し `script`。`mamela provider script <pome-dir> <provider> [<script>]`。
- **遅い変更:** `Change.Late`。マシンの起動を、その実行のすべての作成の後にする。
- **site:** `internal/site`（`mamela-site/v1`、`mamela.site.toml`、雛形は `mamela.site.example.toml`、`--site` か `MAMELA_SITE`）。Control Plane、ゲストのログイン、ネットワークの規則、1 台あたりの予算だけ。基盤ごとの値は `mamela.providers.toml`（`guard_token_id`、`guard_token_file`、`[provider.<name>.guard]`）と宣言に置く。site のコードは MAM-SITE-0304・0305。
- **削除したもの:** `internal/proxmox`、`internal/provider/pve`、`internal/lab`、`mamela lab ...`・`destroy`・`reboot`、組み込みの `VirtualMachine`（graph は MAM-GRAPH-0005 とヒントで拒否する）、VM 用の policy のコード（[reason-codes.md](reason-codes.md) に廃止として残す）、`LAB-*` のコード。
- **controlplane bundle:** Proxmox のトークンの代わりに、ホスト用に書き換えた `mamela.providers.toml` と、その秘密（`/etc/mamela/secrets/<provider>-token`、`<provider>-guard-token`）を入れる。
- **Mamela の Pome 0.3.0:** `Vm`・`vm`・`node(vm)` をなくした。`package`・`service` は `Node` を取る。プロバイダーの Pome 向けに `network_spec(Network)` を足した。ADR 0007 で、`*_on` の別名と k3s の関数はなくし、k3s は `pome/k3s` に分けた。
- **例:** `examples/homelab` は例の EC2 プロバイダーのインスタンスと nginx になった。`examples/kv` に guard（`max_items`）と明示操作 `delete-item` を足した。Proxmox の例は mamela-proxmox に移した。
- **適合試験:** `providertest` に `GuardSettings`、`ScriptCredentials`、`Case.Explicit`、`Case.World` を足した（[provider-authoring.md](provider-authoring.md)）。
- **試験:** 偽のプロバイダー（`internal/provider/fake`）のマシンで、reconcile・plan・graph・node の試験を書き直した。

## 公開する Pome の形、依存、AWS（ADR 0007、2026-11-11）

設計は [ADR 0007](adr/0007-stable-pome-and-dependencies.md)。

- **Pome:** Mamela Pome 0.3.0 は、グラフ、`Node`、`Network`、`package`・`service`、`Ref`・`provided`・`provided_after`、`on_node` だけ。k3s は `pome/k3s`（`K3s.server`・`K3s.agent`）。
- **依存:** グラフ v2 の `after`（MAM-GRAPH-0103、0108）、待ち（MAM-PLAN-0113）、diff の `Dependencies`。作成が途中で止まった依存先も待つ。
- **AWS:** JSON の API（`header`・`json`）、profile の一時的な資格情報、1 つのプロバイダーの複数の API、カタログの値の許可リスト（`values`、違反は MAM-PRV-0008）、AWS のカタログ（ADR 0008 で mamela-aws に移った）。
- **実機での検証（github.com/ieyoukan/mamela-aws）:** 運用者の AWS ロール（MFA の assume-role）で、次を確かめた。
  - **作ったもの:** 専用の VPC、サブネット、インターネットゲートウェイ、ルートテーブル、セキュリティグループ 2 つ、EC2（t4g.micro、nginx のコンテナー、IAM ロールなし）、ECS のクラスター、タスク定義、Fargate Spot のサービス（タスク数 0）、ロググループ、タスクの実行ロール。
  - **作り方:** 依存の深さに合わせて、plan と apply を 6 回繰り返した。
  - **負荷試験:** 明示操作 `run-task` で locust を 1 回動かした。196 リクエストで失敗は 0。
  - **片付け:** 明示操作 `delete` で、すべてを消した。
  - **guard:** 前後のどちらでも block はなかった。境界の外の EC2 インスタンスとクラスターは変わらなかった。
  - **実機で見つけて直したこと:**
    - メンバーのない JSON の呼び出しの本文
    - 先頭が `/` の ID
    - IAM のタグに要る `iam:TagRole`。運用者のロールになく、プロバイダーはロールにタグを付けないことにした
    - インスタンスプロファイルを作れない運用者では、ECS on EC2 にできないこと。プロバイダーに、IAM ロールのないインスタンスを足した。ECS on EC2 は、試験の間だけ `/mamela/` に限った権限を足して確かめ、後で外した
    - RunInstances の固定のパラメータ

## カタログをプロバイダーの Pome に移す（ADR 0008、2026-11-11）

設計は [ADR 0008](adr/0008-catalogs-in-provider-pomes.md)。Mamela 本体は、AWS と Proxmox のカタログを持たなくなった。

- **カタログの形:** `mamela-catalog/v1` の JSON（`protocol.DecodeCatalog`）。
- **同梱:** プロバイダーの Pome の `mamela-provider.toml` の `[catalogs]` に並べる。Mamela は、プログラムと一緒に plan に保存する（migration 11 `plan_catalogs`）。
- **選び方:** 運用者は `catalog = "pome:<名前>"` で名指す。`builtin:` は ADR 0008 を示して拒む。
- **CLI:**
  - `mamela provider catalog [--json] <pome-dir> [<provider>]` で、同梱するカタログを読める。
  - `mamela provider inspect` は、プロバイダーの Pome のディレクトリも取る。
- **試験:** `providertest.Catalog` で、同梱するファイルで試験する。
- **移したもの:**
  - `provider/catalogs` の EC2・ECS・Logs・SSM・IAM は mamela-aws の `catalogs` に移した。
  - Proxmox は mamela-proxmox の `catalogs` に移した。
  - 例の EC2 のプロバイダーは `examples/ec2/pome/catalogs/ec2.json` を同梱する。

## provider kit、Pome の生成、Terraform からの乗り換え（ADR 0009、2026-11-12）

設計は [ADR 0009](adr/0009-provider-kit-and-imports.md)。

- **provider/kit:** 作者は種類ごとに、一覧（`List`）、差分（`Diff`）、効いているか（`Done`）を書く。プロトコルは kit が引き受ける。`examples/kv` は kit で書き直した。
- **spec の型:** manifest に `Kind.Spec`（`protocol.Field`）・`Kind.Doc`・`Kind.Imports` が増えた。kit は Go の構造体から書き出す（`kit.SpecOf`）。
- **`mamela provider pome`:** Pome の `src/lib.emel` を生成する（`internal/pomegen`）。`examples/kv/pome` は生成物で、`build.sh` が生成する。
- **`mamela provider new`:** そのままテストが通る雛形を書く（`internal/scaffold`）。生成直後に `go mod tidy`・`build.sh`・`go vet`・`go test` が通ることを試験する。
- **`mamela import terraform`:** `terraform show -json` の出力を読み（`internal/tfimport`）、プロバイダーの `import` で宣言に変え、Emela（`src/imported.emel`）に書き、引き継ぐ対象を記録する（migration 12 `adoptions`）。
  - 観測は、記録した物を `Adoptable` と報告する。plan は何も変えない（MAM-PLAN-0114）。
  - 引き継ぎは `mamela act --confirm` だけで行う。
- **試験:** `providertest.Case.Adopting` で、取り込み→引き継ぎを fault の注入込みで確かめる。kv-demo で、import→plan→adopt→plan の通しを確かめる。
- **残り:**
  - mamela-aws と mamela-proxmox を kit に移し、取り込みを足す。
  - OpenAPI などからカタログと呼び出しを起こす道具。

## 宣言のリポジトリで運用する（ADR 0010・0012、2026-11-12〜13）

設計は [ADR 0010](adr/0010-gitops-from-the-control-plane.md) と [ADR 0012](adr/0012-mamelacd.md)、使い方は [gitops.md](gitops.md)。

- **`mamelacd`（`cmd/mamelacd`、`internal/gitops`）:** GitHub を読みに行く別のプログラム。GitHub のトークンと自分の DB だけを持つ。
  - PR の plan をコメントする。
  - `/mamela approve <digest>` で承認する。
  - ブランチの先頭の宣言と同じ宣言の、承認された plan だけを apply する。古いマージは apply しない。
- **`mamela api`（`cmd/mamela/api.go`、`internal/api`）:** 本体の局所 API。Unix ソケットで plan・承認・apply だけを受け付ける。中身は CLI と同じ処理を同じ DB で行う。
- **記録:** mamelacd は自分の DB に、出した plan・処理済みのもの・先頭の状態を記録する。本体の DB の gitops の表は migration 16 で消した。
- **tarball:** `internal/tree` が、リンクとツリーの外を指すパスと特殊ファイルを拒む。展開するのは API（本体）である。
- **unit:** `mamela controlplane bundle` が `mamela-api.service` と `mamelacd.service` を入れる。mamelacd は専用のユーザーで動き、`/etc/mamela` と `/var/lib/mamela` を読めない。
- **試験:**
  - 偽の GitHub と kv で、`mamela api` と mamelacd の通しを確かめる。
  - 偽の API で、次を確かめる。
    - 先頭だけを apply する
    - 信頼できない先頭は plan しない
    - 待ち・拒否は plan し直す
    - 前提が違えば plan し直す
    - API が答えないときの扱い
  - API の拒否（リンク、ツリーの外、期限、未知の項目）を確かめる。

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
- 実機の基盤やパッケージを変えるテストは `MAMELA_LAB=1` の明示的な opt-in とし、CI の標準ジョブでは実行しない。guard を持つプロバイダーだけを通す（[lab.md](lab.md)）。

詳しい設計は [architecture.md](architecture.md)、検証環境は [lab.md](lab.md)。SQLite の根拠は [ADR 0001](adr/0001-sqlite.md)。
