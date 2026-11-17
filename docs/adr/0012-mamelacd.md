# ADR 0012 — mamelacd: 同期を本体から分ける

- 状態: Accepted（2026-11-13）
- 日付: 2026-11-13
- 対象: ADR 0010（gitops）、0011（継続的な同期）、0003（Control Plane のホスト）

## 文脈

ADR 0010 の gitops は、`mamela gitops run` として本体の中で動いていた。一つのプロセスが、次の二つを両方持っていた。

- GitHub から来るもの（PR、コメント、tarball）を読む。
- インフラの資格情報と、Mamela の状態（SQLite）を持つ。

外から来るデータを扱う部分と、資格情報を持つ部分が分かれていない。「どんな AI でも安全に保守できる」（ADR 0011）を目指すなら、GitHub に触れる部分に資格情報を持たせたくない。

また、コミット前のレビューで、次の二つが見つかった。

- **古いマージを apply できる:** main が先に進んだ後でも、古いマージコミットの plan を承認すれば apply できた。後のマージの変更を戻してしまう。
- **重い:** マージ済みの PR を、poll のたびに最近の 30 件すべて評価し直していた。

## 決定

### 1. 三つのプログラムに分ける

Kubernetes の kubelet・kubectl・Argo CD に倣う。

| プログラム | 役割 | 持つもの |
|---|---|---|
| mamelet | 各マシンのエージェント（kubelet に当たる） | そのマシンの登録証明書 |
| mamela | 宣言・plan・承認・apply・明示操作・取り込み（kubectl に当たる）。Control Plane のホストでは `mamela serve`（mamelet 向け）と `mamela api`（mamelacd 向け）を動かす | インフラの資格情報、SQLite の状態 |
| mamelacd | Git の宣言に揃え続ける（Argo CD に当たる） | GitHub のトークン、自分の小さな DB |

### 2. 本体の局所 API（`mamela api`）

`mamela api --socket /run/mamela/api.sock` は、Unix ソケットで次のものだけを受け付ける（`mamela-api/v1`）。

| 呼び出し | すること |
|---|---|
| `POST /v1/trees` | コミットの tarball を受け取り、展開して digest で名付ける（`internal/tree` がリンク・ツリーの外・特殊ファイルを拒む） |
| `POST /v1/plan` | ツリーの中の宣言を plan して保存する |
| `POST /v1/revision` | 宣言を評価して digest を返す |
| `POST /v1/approve` | plan を digest で承認する（`by` は `<source>:<login>` の形で、mamelacd は `github:<login>` と伝える。期限は 7 日まで）。物の削除を含む plan は承認しない（MAM-API-0201、ADR 0013） |
| `POST /v1/approved` | plan の承認が有効かを返す |
| `POST /v1/apply` | 承認された plan を apply する |

- **CLI と同じ処理:** 中身は CLI と同じ処理である（同じ DB、同じ guard・policy・カタログ）。
- **一つずつ:** 要求は一つずつ処理する。
- **しないこと:** 明示操作（削除・再起動・引き継ぎ）、取り込み、状態や資格情報を返すことはしない。ネットワークにも出ない（tarball は mamelacd が送ってくる）。
- **届く人:** ソケットは mode 0660、グループ `mamela-api` である。届くのは mamelacd だけにする。

### 3. mamelacd

`mamelacd run --config /etc/mamelacd/mamelacd.toml`。

- **自分のユーザー:** 専用のユーザー `mamelacd`（グループ `mamela-api` に入る）で動く。
- **持つもの:** GitHub のトークンと、自分の DB（`/var/lib/mamelacd/state.db`）だけを持つ。DB に入るのは、出した plan、処理したコメント、ブランチの先頭の状態である。
- **読めないもの:** `/etc/mamela` と `/var/lib/mamela` は読めない。ファイルの権限で読めず、systemd の `InaccessiblePaths` でも塞ぐ。
- **起動時の確かめ:** API の版を確かめ、違えば止まる。

### 4. マージの後は、ブランチの先頭だけを apply する

- **先頭の宣言の plan だけ:** apply するのは、ブランチの先頭の宣言（ディレクトリごとの digest）と同じ宣言の、承認された plan だけである。PR の head で承認した plan でも、マージ後の先頭の宣言が同じなら apply する。
- **古いマージは apply しない:** 先頭でなくなったマージの plan は、承認されていても apply しない。
- **違えば先頭を plan する:** 先頭の宣言が承認したものと違えば、先頭を plan して、その先頭を作った PR にコメントする。承認されれば apply する。
- **信頼できない先頭:** 信頼する作者の PR のマージでない先頭（直接の push、ほかの作者の PR、マージ元が見つからない PR）は plan しない（ADR 0010。§6）。
- **apply の後:** apply の後は、先頭をもう一度 plan する。一つの段階の終わりは、収束ではないため。
  - 次の段階に変更があれば、新しい digest の承認を待つ。
  - 待ち（waiting）・拒否（blocked）なら、poll のたびに plan し直す。同じ状態は二度コメントしない。
- **実物が plan の前提と違ったとき（MAM-REC-0002）:** 何もせず、先頭を plan し直す。前の承認は使わない。
- **plan できないとき:** plan は実物を観測するので、しばらく失敗することがある（API が落ちているなど）。そのときは poll のたびに plan し直し、PR へのコメントは一度だけにする。評価できない宣言は、新しいコミットが来るまで plan しない。
- **API が答えないとき:** 宣言の問題としては報告しない。次の poll でやり直す。その間に書かれた承認のコメントも、処理済みにせず、次の poll で扱う。

### 5. 記録の置き場所

- **本体の DB:** GitHub のことは持たない。migration 16 で、gitops の表（migration 13・14）を消す。
- **mamelacd の DB:** 自分で持つ（`internal/gitops/state.go`）。

### 6. 信頼の範囲

- **mamelacd ができること:** API の範囲内で、任意のツリーの plan、任意の `by` での承認、apply ができる。本体は、承認の `by` を mamelacd が言うとおりに記録し、確かめられない。
- **人の承認という関門は、破られうる:** したがって mamelacd が乗っ取られると、乗っ取った者は、好きな宣言を plan し、承認者になりすまして承認し、apply できる。
- **破られないもの:** policy・guard・カタログは本体が強制するので、mamelacd からは破れない。Denied の変更（データを失いうる要求を含む）は承認があっても動かず、guard の block は何も送らせず、プロバイダーは許可にない要求を送れない。破られるのは、「承認が要る」と判定された変更を、人の承認なしに通せることである。
- **守れるもの:** 乗っ取られても、次のことはできない。
  - 資格情報や状態を読む（API は返さず、ファイルの権限と `InaccessiblePaths` で読めない）。
  - 明示操作（削除・再起動・引き継ぎ）や取り込みをする（API にその呼び出しがない）。
  - 物の削除を承認する（削除を含む plan は、API が承認を断る。Control Plane のホストでの `mamela approve` だけが承認でき、承認のあとの apply だけを mamelacd がする。ADR 0013）。
- **実機で確かめた（2026-11-14、Control Plane の VM で）:** ソケットは `mamela-api` グループだけが開ける。`/etc/mamela`・`/var/lib/mamela` は mamelacd の名前空間から見えない。書き込みは自分の状態と `/tmp` だけで、権限は無い。承認者でない人の承認は断られ、作者でない人の PR は plan されない。リンクやツリーの外へのパスを含む tarball は断られる。API が止まっているあいだの承認コメントは、API が戻ってから処理される。乗っ取られた mamelacd が、ソケットだけで、任意のツリーを plan し、他人の名前で承認できることも再現した。
- **承認の身元:** mamelacd を信じずに承認の身元を確かめられるのは、ADR 0011 の 10（物理キーの署名）だけである。署名は本体が確かめる。未実装なので、今の関門の強さは、mamelacd のホストの安全に頼っている。
- **ブランチ保護が前提:** mamelacd が確かめるのは、先頭が信頼する作者の PR のマージコミットであることだけで、その PR が誰にレビューされたかは見ない。直接 push の禁止とレビュー必須は、GitHub のブランチ保護（ruleset）に任せている。保護がなければ、レビューなしにマージされた宣言を、承認者の承認だけで apply することになる。
- **マージ元の PR の探索:** 先頭のマージ元は、直近 30 件の closed の PR（更新の新しい順）から、マージコミットが先頭と一致するものを探す。それより古い PR のマージが先頭だと、マージ元が見つからず、その先頭は信頼されない扱い（HeadUntrusted）になり、plan しない。新しい信頼できるマージが来れば、そちらが先頭になる。

### 7. 版と配布

mamelacd は、今は同じリポジトリ・同じモジュール（`cmd/mamelacd`）に置き、同じ版で配る。API（`mamela-api/v1`）が安定したら、別のリポジトリに分けられる。

## 検討したが採らなかったもの

- **別のリポジトリにする:** plan の JSON、承認の規則、DB の形は、まだ本体と一緒に変わる。今は版を合わせる手間が増えるだけである。
- **mamelacd が `mamela` の CLI を子プロセスで動かす:** mamelacd のユーザーが、資格情報と DB を読めなければならない。分ける意味がなくなる。
- **API を TCP（mTLS）で開く:** 同じホストでは要らない。mamelacd を別のホストに置く必要が出たら、同じ呼び出しで足す。
- **PR ごとにマージを apply する（ADR 0010 の当初の作り）:** 後のマージを戻しうる（上の文脈）。

## 結果

- **権限が分かれる:** GitHub に触れるプロセスは、資格情報も状態も持たない。資格情報を持つプロセスは、GitHub に触れない。
- **古いマージは apply されない:** main の先頭の宣言だけが apply される。
- **運用:** `mamela-api.service` と `mamelacd.service` の二つを有効にする（`mamela controlplane bundle` が入れる）。`mamela gitops` は無くなる。
- **ADR 0011 との関係:** ADR 0011 の同期（先頭の定期的な plan、手動同期、自動同期、写しの公開）は mamelacd の仕事になる。資格情報なしの確認（`mamela check --view`）は mamela に残る。

## 追記（0.3.2、2026-11-17）

監査（[2026-11-17-iac-audit](../reviews/2026-11-17-iac-audit.md) の A7・A8・B7・B11）で見つかったことの修正。決定の骨格（§1〜§7）は変わらない。

- **§4 の「plan できないとき」と評価の失敗を分ける:** API は、評価の失敗をすべて 422 で返す。宣言の誤りと、評価器が一時的に使えないこと（ツールチェーンの差し替え中、ビルドの時間切れ、ディスクが満杯）を、API が返す理由のコード・文言（`MAM-EVAL-0001`、`MAM-EVAL-0102`、`no space left on device`）で分ける。後者は head の状態 `eval-retry` にして、`retry` の間隔で評価し直す（`MAM-GIT-0201`）。宣言の誤りは、これまでどおり新しいコミットまで評価し直さないが、理由を commit status の description とログに出し、PR がなければコミットへのコメントにする（以前は PR 番号 0 へのコメントしかなかった）。API 側（`internal/api`）で状態コードを分ける案もあるが、API の版を変えずに mamelacd だけで直すために、文言で分けた。
- **§4 の「apply の後」を、失敗に強くする:** apply の結果（`outcome`）を記録した直後に、head の状態も記録する。結果のコメントや、次の段の plan が失敗しても、次の poll が続きから進む。GitHub が受けなかった結果のコメントは、本文ごと `state.db` に残し、次の poll で投稿する（apply はやり直さない）。
- **再試行の上限が poll をまたぐ:** 「同じ変更が戻ってくる」連続回数を、head のコミットごとに数える（stale の apply、または apply したのに同じ形の plan が出る apply。形は plan の本文とカウントの hash）。`retry_max + 1` に達したら、`sync = "push"` は自動で承認しない（head の状態 `stopped`、`MAM-GIT-0101`）。1 時間より間が空いた apply は、一続きとして数えない。ホストの承認と新しいコミットは、これまでどおり効く。
- **§6 の「マージ元の PR の探索」:** 直近 30 件の closed の PR にない新しい先頭は、1 度だけ 100 件ずつ 5 ページまでさかのぼる（承認方式のとき）。open の PR は全件、PR のコメントは全件（ページング）を読む。PR のコメントは、PR の `updated_at` が変わったときだけ読む（poll あたり 33 リクエストから、承認方式で 3 に）。
- **§6 の「承認の身元」:** login の文字列だけでなく、GitHub の数値 ID も照合する。設定は login のまま。初めて見たときに ID を固定して `state.db` に持ち、PR・コメント・コミットの作者の ID が違えば信頼しない（`MAM-GIT-0301`）。固定を動かすのは運用者で、`mamelacd repin`。Control Plane に渡す承認者の記録（`github:<login>`）、API、digest は変わらない。固定は初回の答えなので、最初から別の人が login を持っていた場合は防げない（運用者が最初に確かめる）。
- **§3 の可観測性:** 各 poll の終わりに heartbeat（時刻、結果、所要時間、次の予定、failing の回数）を `state.db` に記録し、`mamelacd status` で表示する。何も変わらないときも 10 分に 1 行 `alive` を出し、同じ失敗の繰り返しは 1 行にまとめる。GitHub のトークンの 401 は、「失効した／権限がない。トークンのファイルを更新して SIGHUP」と hint を出す。SIGHUP でトークンを読み直す（再起動不要）。GitHub のレート制限（403・429、Retry-After、X-RateLimit-Reset）は尊重して、その時刻まで要求を送らない。
- **互換:** `state.db` は migration 2 で、列と表を足す（既存の行は変わらない）。古い mamelacd は、新しい DB を「新しい版」として開かない（戻すには DB を作り直す）。API（`mamela-api/v1`）、plan の JSON、承認の digest、保存済みの行の意味は変えていない。

