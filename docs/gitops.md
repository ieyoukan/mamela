# 宣言のリポジトリで運用する（mamelacd、ADR 0010・0012）

宣言を GitHub のリポジトリに置き、PR で plan を読み、コメントで承認し、マージで反映する。設計は [ADR 0010](adr/0010-gitops-from-the-control-plane.md) と [ADR 0012](adr/0012-mamelacd.md)。

Control Plane のホストで、次の二つが動く。

- **mamelacd:** GitHub を見に行く。GitHub のトークンと自分の DB だけを持つ。
- **`mamela api`:** 本体の局所 API（Unix ソケット）。mamelacd に頼まれた plan・承認・apply を、CLI と同じ処理で行う。

GitHub や CI から Control Plane へ入る道は要らない。資格情報と状態は本体から動かず、mamelacd からも読めない。

## 用意

1. **宣言のリポジトリ:** 宣言の Pome を置く。非公開を勧める（宣言にはサイトの値が入る）。依存の Pome は、Control Plane のホストの Pome のキャッシュにあるもの、または `[replace]` でホストの絶対パスを指したものにする（評価は何も取りに行かない）。
2. **プログラム:** linux/amd64 の `mamela` と `mamelacd` を `/usr/local/bin` に置く（`GOOS=linux GOARCH=amd64 go build ./cmd/mamela ./cmd/mamelacd`）。
3. **unit とユーザー:** `mamela controlplane bundle` の install.sh が、次のものを入れる。
   - `mamela-api.service`（ユーザー `mamela-cp`）
   - `mamelacd.service`（ユーザー `mamelacd`）
   - グループ `mamela-api`（ソケットに届くのはこのグループだけ）
4. **GitHub のトークン:** そのリポジトリだけの fine-grained token を作る。権限は次のものだけにする。
   - Contents: read
   - Pull requests: read and write
   - Commit statuses: read and write

   `/etc/mamelacd/github-token` に置く（`mamelacd` だけが読める 0600）。
5. **設定:** `/etc/mamelacd/mamelacd.toml` に書く（`mamelacd` が読める 0640）。

   ```toml
   repo = "you/infra"              # 宣言のリポジトリ
   branch = "main"                 # この先頭の宣言を反映する
   dirs = ["lab/k3s", "lab/web"]   # 宣言（Pome のディレクトリ）
   token_file = "/etc/mamelacd/github-token"
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

## しないこと

- **明示操作:** 削除・再起動・引き継ぎ（`mamela act --confirm`）はしない。宣言から消しても何も消えない。`mamela api` にもその呼び出しはない。
- **取り込み:** `mamela import terraform` はしない。
- **信頼できない先頭:** 信頼する作者の PR のマージでない先頭（直接の push、ほかの作者の PR）は plan しない。

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
