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

   PR のない先頭（`sync = "push"` の直接の push）の評価失敗や停止の理由は、commit status の description に出る。それに加えてコミットへのコメントも付けたいときは Contents: write が要る（任意）。ただし Contents: write は、そのトークンでブランチへ書けることでもある（`mamelacd` が乗っ取られたときの範囲が広がる）ので、勧めない。権限がなければ、`MAM-GIT-0002` を 1 度だけログに出し、コメントは付けない（理由は status とログにある）。

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
   # resync = "10m"                # 収束済みの宣言を plan し直して、ずれを探す間隔（既定 10m、"0" で止める）
   # active_interval = "10s"       # 何かが進んでいるあいだ（待ちがある宣言）の poll の間隔（既定 10s、5s 以上、interval 以下）
   # retry = "5m"                  # apply が失敗したあと、同じ先頭をやり直すまでの間隔（既定 5m、"0" でやり直さない）。評価器が一時的に使えないときの再評価の間隔にも使う
   # retry_max = 3                 # やり直す回数の上限（既定 3、0 から 20）。同じ変更が戻ってくる（stale、または apply して plan がまだ同じ）回数の上限にも使う（retry_max + 1 回で止まる）
   # self_heal = true              # sync = "push" のとき、ずれを戻す plan を作者の名前で承認する
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

## ずれを見つける（`resync`、`self_heal`）

収束済みの先頭は、新しいコミットがない限り plan し直さない。それだけだと、あとから世界が変わっても（サービスを手で止めた、バイナリを替えた、VM が落ちた）、commit status は「in sync」のままになる。`resync`（既定 10 分）ごとに、収束済みの宣言を plan し直す。plan は観測するだけで、何も変えない。

- **ずれがあれば:** commit status に変更の数が出て、ログに `drifted` と出る。そのずれを戻す plan は、`mamela approve <digest>`（ホスト）で承認する。
- **`self_heal = true`（`sync = "push"` のとき）:** ずれを戻す plan を、先頭の作者の名前で承認して、次の poll で apply する。削除を含む plan と、blocked の plan は承認しない（ホストの承認が要る）。
- **`self_heal = false`（既定）:** ずれを戻す plan は push では承認しない。これは、ずれを見つけた直後だけでなく、そのあとの waiting・blocked・plan できなかった・失敗の状態を経ても、戻し終わる（in sync になる）まで変わらない。ホストの `mamela approve` で承認した戻しの、次の段も同じである。新しいコミットで始まる変更の段は、これまでどおり push が承認する。
- 宣言のとおりに戻すだけで、宣言にないものは消さない。

## 速さ（段をつなげる、待ちを短くする、やり直す）

1 つの変更は、いくつかの段（VM、k3s の導入、サービスの起動、版上げの 1 台ごと）に分かれ、段ごとに plan、承認、apply がある。待つのは、承認の人（人の承認が要るとき）と、世界の変化（VM の起動、mamelet の報告）だけにする。

- **段をつなげる（`sync = "push"`）:** 1 回の poll の中で、plan → push による承認 → apply → 次の段の plan → … と続ける（1 宣言につき 1 回の poll で最大 12 段）。次の poll を待たない。削除を含む plan、blocked の plan、ずれ（`self_heal` なし）の plan は承認しないので、そこで止まる。
- **待ちがあるときは短く poll する（`active_interval`）:** 宣言が「依存するものを待っている」（`waiting`、plan できなかった）あいだ、または段が 12 を超えて続くあいだは、次の poll を `active_interval`（既定 10 秒）で行う。何もないときは `interval`（既定 1 分）。
- **GitHub への読み取りは少ない:** 何も変わっていない poll で、承認方式は 3 リクエスト（open と closed の PR の一覧、ブランチの先頭）、`sync = "push"` は 4 リクエスト（先頭のコミットの作者を足す）である（PR が closed の信頼 PR 30 件のときの実測は、33 から 3 になった）。PR のコメントは、その PR の `updated_at` が、前にコメントを読んだときと変わったときだけ読む（`updated_at` は PR へのコメントでも動く）。`active_interval` の 10 秒で回り続けても、1 時間に 1,500 前後で、GitHub の 5,000 の枠に収まる。open の PR は全件（100 件ずつ、10 ページまで）、コメントは全件（100 件ずつ、50 ページまで）を読む。
- **GitHub のレート制限:** 403 または 429 で、Retry-After か X-RateLimit-Reset があれば、その時刻まで GitHub に何も送らず、1 行（`MAM-GIT-0003`、いつ再開するか）をログに出す。poll の間隔はその時刻まで延びる。残りが 0 と分かった時点（`X-RateLimit-Remaining: 0`）でも、次の要求は送らない。待っているあいだも何も失われない。`mamelacd status` の `next` に、延びた予定が出る。
- **失敗はやり直す（`retry`、`retry_max`）:** apply が失敗すると、`retry`（既定 5 分）あとに、同じ先頭を plan し直し、承認（`sync = "push"` ならその push）して apply する。最大 `retry_max` 回（既定 3）。apply は世界を見直して続きから進むので、繰り返してよい。commit status に `(retry 1/3 in 5m0s)`、上限を超えると `(gave up after 4 tries)` と出る。新しいコミットは数え直す。`sync = "approval"` では、やり直しの plan にも承認のコメントが要る。
- **同じ変更が戻ってくるときは止まる（`MAM-GIT-0101`）:** `sync = "push"` が承認した plan の apply が stale（世界が plan の前提と違った）で終わり続ける、または apply したのに同じ変更の plan がまた出る、という繰り返しが `retry_max + 1` 回続くと、それ以上自動では承認しない。commit status、ログ、コメント（マージ元の PR があればそこ、なければコミット）に、何が・なぜ・どうするかを出す。1 時間より間が空いた apply は、繰り返しに数えない（ときどきの修復は止めない）。新しいコミットを push するか、ホストで `mamela approve <digest>` する（それは次の poll で apply される）と動く。`sync = "approval"` では人が承認するので、この上限はない。
- **評価器が一時的に使えないとき（`MAM-GIT-0201`）:** API が評価を断った理由が、ツールチェーンの差し替え中（`MAM-EVAL-0001`）、ビルドの時間切れ（`MAM-EVAL-0102`）、ディスクが満杯のときは、宣言のせいではないので、その先頭は `retry` の間隔（`retry = "0"` なら `interval`）で評価し直す。commit status は `not evaluated for now (...): trying again in 5m0s` と出る（`pending`）。評価は何も変えないので、回数の上限はない（ログと `mamelacd status` に出続ける）。それ以外の理由（ビルドの失敗、宣言の `fail` など）は宣言の誤りで、新しいコミットが来るまで評価し直さない。理由は commit status の description、ログ、コメントに出る（`sync = "push"` の先頭は PR がないので、コミットへのコメント）。

mamelet は、次の poll を待たずに、自分の revision が新しくなった（承認した操作が許可された）ときに、すぐ動く。Control Plane の `GET /v1/nodes/<node>/wait?after=<serial>&seconds=<n>` が、新しい revision が出るか、最大 30 秒で返る。古い Control Plane は、このリクエストを知らない（404）ので、mamelet は今までどおり `intervalSeconds`（既定 30 秒）ごとに動く。

## 動いているか見る（`mamelacd status`、ログ、トークンの入れ替え）

mamelacd は、正常に待っているあいだは何も出さない。それがハングと区別できるように、次のものを持つ。

- **heartbeat:** poll のたびに、始まりと終わり（時刻、結果、所要時間、次の予定）を `state.db` に記録する。
- **`mamelacd status --config /etc/mamelacd/mamelacd.toml`:** それを読んで、最後の poll、次の poll、プロセス（pid、版、稼働時間）、各 dir の head の状態と意味、固定したアカウントを表示する。`mamelacd` のユーザーで実行する（`sudo -u mamelacd mamelacd status --config ...`。DB のファイルの所有者を変えないため）。`state_db` がなければ作らず、そう言う。
  - 最後の poll の終わりが `interval` の 3 倍より古く、次の予定（レート制限の待ちなど）も過ぎていれば、`PROBLEM: no poll finished for ...: mamelacd may be stopped or stuck (MAM-GIT-0501)` と出る。見る先は `systemctl status mamelacd` と `journalctl -u mamelacd -n 100`。poll が走っているあいだは `running since ...` と出る（長い apply かもしれない。3 倍を超えれば注意が出る）。
  - poll が失敗し続けていれば、何回目か、いつから、理由が出る。固定した ID と違うアカウントが使われたときも出る。
  - `PROBLEM` が 1 つでもあれば、終了コードは 1。監視から呼べる。
- **ログ:** 何も変わらなくても、10 分に 1 行、`mamelacd: alive: ...`（何回 poll したか、最後の所要時間、次の予定、各 dir の状態）を出す。同じ理由で失敗し続ける poll は、最初の 1 回だけ全文を出し、10 分ごとに「still failing the same way, N poll(s) since ...」と 1 行にまとめ、直ったとき 1 回だけ「polls work again」と出す。
- **起動の行:** 起動時に、見る先（`mamelacd status --config ...`）を 1 行で言う。

### GitHub のトークンが失効したとき・入れ替えるとき

トークンが 401 になると、`MAM-GIT-0001: GitHub GET ... answered 401: the token was revoked or has expired, or it has no access to <repo>. Make a new token (...), write it to <token_file>, then send mamelacd a SIGHUP ... or restart it` と出る（その poll は止まり、同じ理由の繰り返しは上のとおり 1 行にまとまる）。入れ替えの手順は、再起動なしで次のとおり。

1. GitHub で、そのリポジトリだけの新しい fine-grained token を作る（権限は「用意」の 4）。
2. `/etc/mamelacd/github-token` を新しいトークンで書き換える（所有者 `mamelacd`、0600）。
3. 次のどれかで、mamelacd に読み直させる。
   - 何もしない: 401 のときは、トークンのファイルを読み直し、変わっていればそのまま使う（次の poll で自動）。
   - `systemctl kill -s HUP mamelacd`（SIGHUP）: すぐ読み直して、すぐ poll する。トークンがまだ有効なうちに入れ替えるときは、これを使う。
   - `systemctl restart mamelacd`。
4. 古いトークンを GitHub で失効させる。

ファイルが読めない・空のときは、使っているトークンのまま、ログに理由を出す。

## しないこと

- **明示操作・取り込み:** 再起動・引き継ぎ・削除の明示操作（`mamela act --confirm`）と `mamela import terraform` はしない。`mamela api` にもその呼び出しはない。
- **削除の承認:** 宣言から消した物の削除（[ADR 0013](adr/0013-removal-deletes.md)）は、plan に載り、PR のコメントにもそう書かれるが、コメントでは承認できない。Control Plane のホストで `mamela approve <digest>` を実行する。承認のあとにマージされれば、mamelacd が apply する。
- **信頼できない先頭:** 信頼する作者の PR のマージでない先頭（直接の push、ほかの作者の PR）は plan しない。

## 前提と信頼の範囲（ADR 0012 の 6）

- **ブランチ保護:** 直接 push の禁止とレビュー必須は、GitHub のブランチ保護に任せている。mamelacd は、先頭が信頼する作者の PR のマージコミットであることしか確かめない。
- **古い PR のマージ:** マージ元の PR は、まず直近 30 件の closed の PR から探す。そこになく、先頭が宣言にとって新しいとき（承認方式で、1 度だけ）は、100 件ずつ 5 ページ（500 件）まで、さかのぼって探す。それより古いマージが先頭になっていると、その先頭は信頼されない扱いになり、plan しない。
- **承認者・作者は login と GitHub の数値 ID（`MAM-GIT-0301`）:** 設定は login のままだが、初めて見たときに、各 login の数値 ID を GitHub に聞いて `state.db` に固定する。以後、PR・承認コメント・コミットの作者の ID が、固定した ID と違えば、同じ login でも信頼しない。改名で空いた旧 login を別の人が取っても、承認権限は得られない。違うアカウントが使ったことは、1 度だけログに出し、`mamelacd status` に残る。アカウントを作り直した、などで固定を動かすときは `mamelacd repin --config <toml> --login <login>`（次の poll で、その時点の ID を固定し直す。稼働中の mamelacd は再起動しなくてよい）。固定するのは初回の問い合わせの答えなので、`mamelacd` を初めて動かす前に、login が本人のものか確かめる（ログに `pinned @alice to GitHub account id N` と出る）。数値 ID は、承認者を記録する `github:<login>`（Control Plane の記録）には影響しない。固定は mamelacd の `state.db` にあるので、それを失えば（[recovery.md](recovery.md) のとおり、バックアップには含まれない）、次の poll の初回の問い合わせで作り直される。そのときも、先に login が本人のものか確かめる。
- **mamelacd が乗っ取られると:** API の範囲内で、好きな宣言を plan し、承認者になりすまして承認し、apply できる。人の承認という関門は破られうる。ただし、物の削除を含む plan は承認できない（API が断る）ので、削除はできない。policy・guard・カタログ、資格情報と状態の秘匿、明示操作・取り込みをしないことは破られない。承認の身元を mamelacd を信じずに確かめるのは、ADR 0011 の署名（未実装）である。

## 試すとき

一度だけ GitHub を読むには、次を実行する。

```sh
mamela api --socket /tmp/mamela-api.sock --db ... --providers ... --site ... &
mamelacd once --config mamelacd.toml   # mamela_socket = "/tmp/mamela-api.sock"
```

テストは次の三つで行う。

- **`cmd/mamela/api_test.go`:** 偽の GitHub（`internal/gitops/fakegithub`）と例の kv で、PR → plan → 承認 → マージ → apply を、`mamela api` と mamelacd の通しで確かめる。
- **`internal/gitops`:** 偽の API で、先頭の扱いを確かめる。偽の GitHub（`fakegithub`）は、`per_page` とページング（Link）、更新時刻（`updated_at`、Date）、アカウントの数値 ID と改名、レート制限（403 と X-RateLimit-*、Retry-After）、5xx、リクエストの記録を模す。模さない偽物は、これらの欠陥（監査 A7）をすり抜けた。
- **`cmd/mamelacd`:** `status`、`repin`、SIGHUP によるトークンの読み直しを、実際のソケットと偽の GitHub で確かめる。
- **`internal/api`:** API の拒否を確かめる。
