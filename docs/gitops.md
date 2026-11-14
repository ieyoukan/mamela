# 宣言のリポジトリで運用する（mamelacd、ADR 0010・0012）

宣言を GitHub のリポジトリに置き、PR で plan を読み、コメントで承認し、マージで反映する。設計は [ADR 0010](adr/0010-gitops-from-the-control-plane.md) と [ADR 0012](adr/0012-mamelacd.md)。

Control Plane のホストで、次の二つが動く。

- **mamelacd:** GitHub を見に行く。GitHub のトークンと自分の DB だけを持つ。
- **`mamela api`:** 本体の局所 API（Unix ソケット）。mamelacd に頼まれた plan・承認・apply を、CLI と同じ処理で行う。

GitHub や CI から Control Plane へ入る道は要らない。資格情報と状態は本体から動かず、mamelacd からも読めない。

## 用意

1. **宣言のリポジトリ:** 宣言の Pome を置く。非公開を勧める（宣言にはサイトの値が入る）。依存の Pome は、Control Plane のホストの Pome のキャッシュにあるもの、または `[replace]` でホストの絶対パスを指したものにする（評価は何も取りに行かない）。
2. **プログラム:** linux/amd64 の `mamela` と `mamelacd` を `/usr/local/bin` に置く（`GOOS=linux GOARCH=amd64 go build -o dist/ ./cmd/mamela ./cmd/mamelacd`）。
3. **unit とユーザー:** 次のものを入れる。
   - `mamela-api.service`（ユーザー `mamela-cp`）
   - `mamelacd.service`（ユーザー `mamelacd`）
   - グループ `mamela-api`（ソケットに届くのはこのグループだけ）

   新しいホストでは `mamela controlplane bundle` の install.sh が入れる。Control Plane がすでに動いているホストでは、`mamela controlplane gitops-units --out units` で unit と `install-gitops.sh` を書き、ホストで読んでから root で実行する。unit は `/etc/mamela/site.toml`、`/etc/mamela/providers.toml`、`/var/lib/mamela/state.db` を見る。ホストが別の場所に置いているなら、unit を直す。
4. **GitHub のトークン:** そのリポジトリだけの fine-grained token を作る。権限は次のものだけにする。
   - Contents: read
   - Pull requests: read and write
   - Commit statuses: read and write

   `/etc/mamelacd/github-token` に置く（所有者 `mamelacd`、0600）。
5. **設定:** `/etc/mamelacd/mamelacd.toml` に書く（所有者 `root`、グループ `mamelacd`、0640）。

   ```toml
   repo = "you/infra"              # 宣言のリポジトリ
   branch = "main"                 # この先頭の宣言を反映する
   dirs = ["lab/k3s", "lab/web"]   # 宣言（Pome のディレクトリ）
   token_file = "/etc/mamelacd/github-token"
   # api_url = "https://api.github.com"   # GitHub Enterprise など、別の API のとき
# sync = "push"                 # PR なしで、main への push を承認として同期する（下の「push で同期する」）
   authors = ["you"]               # この人たちの PR だけを plan する（fork の PR は扱わない）
   approvers = ["you"]             # 承認できる人
   # self_approval = false         # 作者が自分の PR を承認できないようにする
   approval_ttl = "24h"            # 承認の期限（7 日まで）
   interval = "1m"                 # GitHub を読む間隔
   mamela_socket = "/run/mamela/api.sock"
   state_db = "/var/lib/mamelacd/state.db"
   ```

6. **ツールチェーン:** `/etc/mamela/api.env` で指す（例: `MAMELA_TOOLCHAIN_DIR=/var/lib/mamela/src/.tools/bin`、`EMELA_POME_CACHE=/var/lib/mamela/src/.tools/pomes`）。評価するのは `mamela api` なので、mamelacd には要らない。
7. **起動:** `systemctl enable --now mamela-api mamelacd`。mamelacd は `mamelacd.toml` がなければ起動しない。起動時に API の版を確かめ、違えば止まる（`mamela` と `mamelacd` は同じ版にそろえる）。

## 使い方

1. ブランチで宣言を変え、PR を開く。
   - mamelacd が plan を PR にコメントする。変更・理由・送る要求をすべて書き、digest を付ける。
   - commit status `mamela/<dir>` も付ける。
   - push するたびに plan し直す。
2. 承認者が、コメントにある 1 行を PR に書く。

   ```
   /mamela approve sha256:...
   ```

   mamelacd がその PR に出した plan で、digest が完全に同じものだけが承認される。
3. PR をマージする。mamelacd は、ブランチの先頭の宣言を見る。
   - 先頭の宣言が承認した plan と同じなら、apply する。guard は前後に走る。
   - 違えば（main が先に進んでいたときなど）、先頭を plan してマージした PR にコメントする。その承認があれば、すぐに apply する。
   - 先頭でなくなった古いマージは、承認されていても apply しない（後のマージを戻さないため）。
4. 結果は PR のコメントと、先頭のコミットの commit status に出る。
   - apply の後は先頭を plan し直す。次の段階に変更があれば、新しい digest の承認を待つ。
   - 待ち・拒否のあいだは poll のたびに plan し直す（同じ状態は二度コメントしない）。

## push で同期する（`sync = "push"`、ADR 0014）

書く人が一人なら、PR もコメントも要らない。`mamelacd.toml` に `sync = "push"` を書くと、`main` の先頭のコミットの作者が `authors` にいれば、その先頭を plan し、変更があれば作者の名前で承認して、次の poll で apply する。結果は commit status（`mamela/<dir>`）に出る。

- **削除は承認しない:** 削除を含む plan は、status に `mamela approve <digest>` が出る。Control Plane のホストで承認すると、次の poll で apply する。
- **ブランチ保護:** PR 必須の保護は外す（force push と削除の禁止は残す）。ブランチへ書ける人が、承認する人になる。複数人で運用するなら `sync = "approval"`（既定）にする。
- **approvers は空でよい:** push では承認のコメントを読まない（PR の承認コメントは、これまでどおり動く）。

## しないこと

- **明示操作・取り込み:** 再起動・引き継ぎ・削除の明示操作（`mamela act --confirm`）と `mamela import terraform` はしない。`mamela api` にもその呼び出しはない。
- **削除の承認:** 宣言から消した物の削除（[ADR 0013](adr/0013-removal-deletes.md)）は、plan に載り、PR のコメントにもそう書かれるが、コメントでは承認できない。Control Plane のホストで `mamela approve <digest>` を実行する。承認のあとにマージされれば、mamelacd が apply する。
- **信頼できない先頭:** 信頼する作者の PR のマージでない先頭（直接の push、ほかの作者の PR）は plan しない。

## 前提と信頼の範囲（ADR 0012 の 6）

- **ブランチ保護:** 直接 push の禁止とレビュー必須は、GitHub のブランチ保護に任せている。mamelacd は、先頭が信頼する作者の PR のマージコミットであることしか確かめない。
- **古い PR のマージ:** マージ元の PR を直近 30 件の closed の PR から探すので、それより古いマージが先頭になっていると、その先頭は信頼されない扱いになり、plan しない。
- **mamelacd が乗っ取られると:** API の範囲内で、好きな宣言を plan し、承認者になりすまして承認し、apply できる。人の承認という関門は破られうる。ただし、物の削除を含む plan は承認できない（API が断る）ので、削除はできない。policy・guard・カタログ、資格情報と状態の秘匿、明示操作・取り込みをしないことは破られない。承認の身元を mamelacd を信じずに確かめるのは、ADR 0011 の署名（未実装）である。

## 試すとき

一度だけ GitHub を読むには、次を実行する。

```sh
mamela api --socket /tmp/mamela-api.sock --db ... --providers ... --site ... &
mamelacd once --config mamelacd.toml   # mamela_socket = "/tmp/mamela-api.sock"
```

テストは次の三つで行う。

- **`cmd/mamela/api_test.go`:** 偽の GitHub（`internal/gitops/fakegithub`）と例の kv で、PR → plan → 承認 → マージ → apply を、`mamela api` と mamelacd の通しで確かめる。
- **`internal/gitops`:** 偽の API で、先頭の扱いを確かめる。
- **`internal/api`:** API の拒否を確かめる。
