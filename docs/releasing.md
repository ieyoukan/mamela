# Pome の公開（版、タグ、利用側の更新）

このリポジトリが公開する Pome は 2 つ。`pome/mamela`（`github.com/ieyoukan/mamela/pome/mamela`、モジュール `Mamela`）と `pome/k3s`（`K3s`）。利用側（宣言のリポジトリ、プロバイダーの Pome）は、Emela の proxy から**版のタグ**（`pome/<名前>/vX.Y.Z`）で取得する。タグが指す内容は、取得した側の `Pome.lock` の hash と sumdb に残るので、**公開した版の内容は変えられない**。直すときは新しい版を出す。

[`scripts/check-pome-versions.sh`](../scripts/check-pome-versions.sh) が、この文書の規則のうち機械で見られるものを `scripts/check.sh` と CI で検査する（ネットワークは使わず、clone のタグと git のオブジェクトを読む。CI の checkout は `fetch-depth: 0`。shallow clone は断る）。ローカルでタグが古いときは `git fetch origin --tags`。

## 規則

- **内容を変える PR は、同じ PR で `Pome.toml` の `version` を上げる。** 内容は、Pome のディレクトリのファイルのうち `test/` と `Pome.lock` を除くすべて（ソース、`[dependencies]` を含む `Pome.toml`）。タグのある版と内容が違うと検査が落ちる。`test/` と `Pome.lock` だけの変更は、版を上げなくてよい（使う側はそれをビルドしない）。
- **まだタグのない版は、何度変えてもよい。** 次の版を、複数の PR で育てられる。最初の PR が版を上げ、あとの PR は上がった版のまま内容を変える。タグを付けた時点で、その版は固定される。
- **`Pome.toml` の `version` は、公開済みのどのタグより古くならない。** 古い main から出した PR は、`git rebase origin/main` する。
- **タグは `pome/<名前>/vX.Y.Z`、注釈付き、マージコミットに付ける。** そのタグの `Pome.toml` の `version` は X.Y.Z と同じでなければならない。軽量タグ、版と食い違うタグ、`vMAJOR.MINOR.PATCH` でないタグは検査が落ちる。
- **examples の `[dependencies]` は、`[replace]` が指す checkout の `version` と同じにする。** examples は checkout の Pome をビルドするので、公開版にない API を使っても CI は通ってしまう。要求する版を checkout の版に結ぶと、API を足した版を出すとき、同じ PR で examples の要求も上がる。`internal/evaluate/testdata/` の壊れた Pome は対象外。
- **公開する Pome が、このリポジトリの別の Pome に依存するときは、公開済みの版を書く**（`[replace]` なし）。その版のタグがなければ検査が落ちる。`Pome.lock` に残る commit とタグの commit が違うとき（取得したあとにタグを動かしたとき）も落ちる。
- **版の付け方（0.x のあいだ）:** 互換のない変更（API の削除・変更、Graph の形の変更）は minor、追加と修正は patch。0.3.0 から 0.3.1（`Mamela.mamelet` の追加）、K3s の 0.1.0 から 0.1.1（`K3s.manifest` の追加）がその例。

## 手順（Mamela Pome を 0.3.2 にする場合）

1. **PR で版を上げる。** 変更と同じ PR で次を行う。
   - `pome/mamela/Pome.toml` の `version = "0.3.2"`。
   - examples の `Pome.toml`（`examples/homelab`、`examples/kv-demo`、`examples/kv/pome`、`examples/ec2/pome`）の `[dependencies]` の `github.com/ieyoukan/mamela/pome/mamela` を `"v0.3.2"` に。`[replace]` に載る依存は `Pome.lock` に書かれないので、lock は変わらない。
   - README の版の記述（`Try the current implementation`）と、新しい kind を出すなら [graph-schema.md](graph-schema.md) の互換表。
   - `scripts/check.sh` を通す。
2. **PR をマージする。** マージコミットにする（タグを付ける先になる）。
3. **マージコミットに注釈付きタグを付けて push する。** 公開はこの push で起きる。

   ```sh
   git fetch origin && git checkout main && git merge --ff-only origin/main
   scripts/check-pome-versions.sh        # main の内容とまだ付けていないタグ: 通ること
   git tag -a pome/mamela/v0.3.2 -m "Mamela Pome 0.3.2" <マージコミット>
   git push origin pome/mamela/v0.3.2
   ```

   **付けたタグは動かさない。** 付け替えも、削除して付け直すこともしない（取得済みの `Pome.lock` と sumdb が食い違い、`scripts/check-pome-versions.sh` も `pome/k3s` の lock の commit との食い違いで落ちる）。誤った版は、新しい版（0.3.3）で直す。
4. **取れることを確かめる。** 空の `EMELA_POME_CACHE` で、その版を依存に書いた作業用の Pome に `emela pome install` を実行する（proxy と sumdb を通って取れること）。

## k3s の Pome が Mamela の Pome に依存する

`pome/k3s` の `[dependencies]` は、Mamela の Pome の**公開済みの版**を名指す（`[replace]` なし。いま `v0.3.0`）。Emela は、依存の中で要求された版のうち**最も高いもの**を選ぶ（k3s 0.1.1 が `v0.3.0` を要求し、宣言が `v0.3.1` を要求すると `v0.3.1` でビルドされる）。だから k3s の要求は、K3s が動く**最低の版**を意味する。

- **上げるとき:** (a) K3s が Mamela の Pome の新しい API を使うようになった、(b) Mamela の Pome が互換のない変更（minor）をして、K3s がそれに合わせた。
- **上げないとき:** 上のどちらでもないとき。上げると、K3s を使う宣言はすべて新しい Mamela の Pome を引き込む。それが新しい kind を出すなら、古い Mamela のバイナリでは MAM-GRAPH-0005 になる（下の「バイナリとの互換」）。
- **順序:** Mamela の Pome を公開（タグの push まで）してから、k3s の PR を出す。同じ PR では出せない（k3s の依存は、タグのある版でなければならない）。k3s の PR で次を行う。
  1. `pome/k3s/Pome.toml` の依存を、公開した版に上げる。
  2. `cd pome/k3s && emela pome install` で `Pome.lock` を書き直す（`scripts/install-toolchain.sh` は、lock が変わったら書き直して止まる。変更を commit する）。
  3. k3s の `version` を上げる（依存の変更は内容の変更）。
  4. マージして、`pome/k3s/vX.Y.Z` を同じ手順で付ける。

## 公開したあと、利用側が依存を上げる

宣言のリポジトリ（非公開でよい）が新しい版を使うには、その版が **Control Plane のホストの Pome キャッシュに先に入っている**必要がある。評価は何も取りに行かない（`EMELA_POMEPROXY=off`。[gitops.md](gitops.md) の用意 6 の `EMELA_POME_CACHE`）。入っていないまま PR を出すと（または `sync = "push"` で push すると）、mamelacd の plan が失敗する（MAM-EVAL-0101、`a dependency in Pome.lock is not in the Pome cache`）。何も apply されないが、先へ進めない。

順序は次のとおり。

1. **バイナリ:** 新しい Pome が新しい kind や field を出すなら、先に Control Plane の `mamela`（と `mamelacd`）を、それを受け付ける版にする（下の「バイナリとの互換」）。
2. **Pome キャッシュ:** Control Plane のホストで、運用の `EMELA_POME_CACHE`（`/etc/mamela/api.env` で指しているもの。`mamela toolchain` で確かめられる）に対して、新しい `Pome.toml` と `Pome.lock` のある宣言のディレクトリから `emela pome install` を実行する。ネットワークを使い、proxy と sumdb を通って取る。使う emela は固定版（`toolchain.lock`）で、`mamela api` を動かすユーザーが読めるようにする。
3. **宣言のリポジトリ:** 新しい版に `[dependencies]` を上げ、`emela pome install` で `Pome.lock` を更新して、まとめて PR にする。mamelacd が plan をコメントする（キャッシュにあるので成功する）。plan を読んで承認し、マージする。

キャッシュに古い版を残してよい。古い宣言（まだ上げていない別の宣言のリポジトリ）がそれを使う。

## バイナリとの互換

新しい kind（や field）を出す Pome の版を、それを知らない古い Mamela のバイナリが読むと、評価の結果の検査で MAM-GRAPH-0005（知らない kind）または MAM-GRAPH-0003（知らない member）になる。Pome の版と、要るバイナリの対応は [graph-schema.md の「Pome の版と Mamela のバイナリ」](graph-schema.md#pome-の版と-mamela-のバイナリ) の表にある。新しい kind や field を足す PR は、その表に行を足す。

## この検査が見ないこと

- examples が、公開版の API だけを使っているか。examples は checkout をビルドするので、要求する版と checkout の版が同じであることだけを見ている。
- タグが、マージコミットに付いているか。
- 公開（proxy が取れること）。手順 4 で確かめる。
- 宣言のリポジトリや Control Plane のキャッシュの状態。
