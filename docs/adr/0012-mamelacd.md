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
| `POST /v1/approve` | plan を digest で承認する（`by` は `github:<login>`、期限は 7 日まで） |
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
- **信頼できない先頭:** 信頼する作者の PR のマージでない先頭（直接の push、ほかの作者の PR）は plan しない（ADR 0010）。
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

- **mamelacd ができること:** API が受け付ける範囲で plan・承認・apply ができる。policy・guard・カタログの外には出られない。
- **mamelacd ができないこと:** 次のことは、乗っ取られてもできない。
  - 資格情報や状態を読む。
  - 明示操作・取り込みをする。
  - 承認なしで apply する。
- **承認者の身元:** 今は mamelacd が `github:<login>` として伝える。ADR 0011 の 10（物理キーの署名）を入れるときは、署名を API（本体）が確かめる。そうすれば、mamelacd を信じなくても、承認者の身元を確かめられる。

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
