# 宣言のリポジトリで運用する（gitops、ADR 0010）

宣言を GitHub のリポジトリに置き、PR で plan を読み、コメントで承認し、マージで反映する。Control Plane が GitHub を見に行く。GitHub や CI から Control Plane へ入る道は要らず、資格情報も状態も Control Plane から動かない。設計は [ADR 0010](adr/0010-gitops-from-the-control-plane.md)。

## 用意

1. **宣言のリポジトリ:** 宣言の Pome を置く。非公開を勧める（宣言にはサイトの値が入る）。依存の Pome は、Control Plane のホストの Pome のキャッシュにあるもの、または `[replace]` でホストの絶対パスを指したものにする（評価は何も取りに行かない）。
2. **GitHub のトークン:** そのリポジトリだけの fine-grained token を作る。権限は次のものだけにする。
   - Contents: read
   - Pull requests: read and write
   - Commit statuses: read and write

   Control Plane のホストの `/etc/mamela/secrets/github-token` に置く（`mamela-cp` だけが読める 0600）。
3. **設定:** `/etc/mamela/gitops.toml` に書く。

   ```toml
   repo = "you/infra"              # 宣言のリポジトリ
   branch = "main"                 # ここへのマージを反映する
   dirs = ["lab/k3s", "lab/web"]   # 宣言（Pome のディレクトリ）
   token_file = "/etc/mamela/secrets/github-token"
   authors = ["you"]               # この人たちの PR だけを plan する（fork の PR は扱わない）
   approvers = ["you"]             # 承認できる人
   # self_approval = false         # 作者が自分の PR を承認できないようにする
   approval_ttl = "24h"            # 承認の期限
   interval = "1m"                 # GitHub を読む間隔
   work_dir = "/var/lib/mamela/gitops"
   ```

4. **ツールチェーン:** `/etc/mamela/gitops.env` で指す（例: `MAMELA_TOOLCHAIN_DIR=/var/lib/mamela/src/.tools/bin`、`EMELA_POME_CACHE=/var/lib/mamela/src/.tools/pomes`）。
5. **起動:** `systemctl enable --now mamela-gitops`。unit は `mamela controlplane bundle` が入れる。`gitops.toml` がなければ起動しない。

## 使い方

1. ブランチで宣言を変え、PR を開く。
   - Mamela が plan を PR にコメントする。変更・理由・送る要求をすべて書き、digest を付ける。
   - commit status `mamela/<dir>` も付ける。
   - push するたびに plan し直す。
2. 承認者が、コメントにある 1 行を PR に書く。

   ```
   /mamela approve sha256:...
   ```

   Mamela が出した plan で、digest が完全に同じものだけが承認される。
3. PR をマージする。
   - マージした宣言が承認した plan と同じなら、apply する。guard は前後に走る。
   - 違えば（main が先に進んでいたときなど）、マージを plan し直してコメントする。その承認があれば、すぐに apply する。
4. 結果は PR のコメントと、マージコミットの commit status に出る。

## しないこと

- **明示操作:** 削除・再起動・引き継ぎ（`mamela act --confirm`）はしない。宣言から消しても何も消えない。
- **取り込み:** `mamela import terraform` はしない。
- **PR を経ない push:** 設定したブランチへの直接の push は plan しない。

## 試すとき

一度だけ GitHub を読むには、次を実行する。

```sh
mamela gitops once --config gitops.toml --db ... --providers ... --site ...
```

テストは、偽の GitHub（`internal/gitops/fakegithub`）と例の kv で、PR → plan → 承認 → マージ → apply を通す（`cmd/mamela/gitops_test.go`）。
