# Control Plane の復旧・バックアップ・証明書の寿命

> 2026-11-17（0.3.2）。この文書は公開リポジトリにあるため、ホスト名・アドレス・トークンは書かない。パスは Control Plane のホストの既定（[lab.md](lab.md) の 5。`controlplane bundle` の install.sh が置く場所）。

Control Plane（CP）のホストを失ったとき、何を失い、何を戻せば何が動くか。mamelet の証明書がいつ切れ、切れたらどうするか。理由コードは [reason-codes.md](reason-codes.md) の `MAM-PKI`・`MAM-BAK`。

## 1. CP を成り立たせているもの

| もの | 場所（ホストの既定） | 失うと | `mamela backup` |
|---|---|---|---|
| 状態の DB | `/var/lib/mamela/state.db` | Mamela が作った物の記録が消え、**全部が Foreign になる**（plan は何も変えない）。登録済みの mamelet の記録が消え、全 mamelet が 403 になる。承認・revision の履歴・操作の記録も消える | 含む |
| 鍵のディレクトリ | `/var/lib/mamela/cp/`（`controlplane.state_dir`）: `ca.pem` `ca-key.pem` `revision-key.pem` `server.pem` `server-key.pem` | mamelet は CA と revision の署名鍵を**固定**しているので、**全 mamelet が止まる**。新しく作ると、全ノードの再登録と設定の書き換えが要る（4、6.4） | 含む |
| site・providers・トークン | `/etc/mamela/site.toml`、`providers.toml`、`secrets/` | 設定の書き直し。プロバイダーのトークンは、プロバイダーで発行し直せる | **含めない**（下） |
| mamelacd | `/etc/mamelacd/`（設定、GitHub のトークン）、`/var/lib/mamelacd/state.db` | ブランチの先頭を最初から評価し直す。Mamela の状態は壊れない。トークンは GitHub で発行し直す | **含めない**（下） |
| mamelet のバイナリ | `/var/lib/mamela/mamelet-linux-amd64` | `scripts/build-mamelet.sh` で作り直せる | 含めない |
| 宣言 | Git のリポジトリ | — | — |

**`mamela backup` に入れるのは、この Mamela にしかなく、作り直せないものだけ**（DB と鍵）。

- **providers・トークンを入れない理由:** 第三者のシステムの資格情報で、CA の秘密鍵と同じファイルに入れると、バックアップが盗まれたときの被害が広がる。発行し直せる。置き場所は運用者のもの（`controlplane bundle` で作った一式、または手元の設定）。
- **mamelacd を入れない理由:** 別のプログラム・別のユーザーで、信頼の境界が違う（ADR 0012。`mamela` のユーザーは `/var/lib/mamelacd`〔0700、所有者 mamelacd〕を読めず、mamelacd も `/etc/mamela` と `/var/lib/mamela` を読めない）。持つのは GitHub から作り直せる状態と、GitHub のトークンだけで、失っても CP の状態は壊れない。設定（`mamelacd.toml`。秘密は入っていない）とトークンは、運用者が別に控える。

## 2. バックアップ

~~~bash
# CP のホストで。鍵と DB の所有者で実行する（ホストでは mamela-cp）
sudo -u mamela-cp mamela backup --site /etc/mamela/site.toml --db /var/lib/mamela/state.db --out <新しい名前>.backup
~~~

- **中身:** `manifest.json`（各ファイルの大きさと sha256、作った時刻、Mamela の版、登録済み mamelet の数）、`state.db`（`VACUUM INTO` の一貫したコピー。`mamela serve` が動いていてよい）、`cp/` の鍵 5 つ。mode 0600。**既存のファイルには書かない**（MAM-BAK-0001）。
- **鍵が DB と合わないときは作らない:** DB に登録済みの mamelet があるのに CA・署名鍵がなければ、MAM-PKI-0201 で止まる（戻すと、止まる状態をそのまま戻すことになる）。
- **秘密である:** CA の秘密鍵が入っている。DB にも、宣言のマニフェストや k3s の join token が平文で入りうる（A4。秘密の参照は 0.4）。ホストの外に、**暗号化して**置く（Mamela は暗号化しない。`age`・`gpg` などで包む）。ホストの中に置くなら、ホストを失うときに一緒に失う。
- **頻度:** 鍵は変わらないが、DB は apply や登録のたびに変わる。**DB が古いと、mamelet が先に進んでいる**（5）。apply のあと、と定期（毎日など）に取る。取った日付を名前に入れる。
- **試す:** 別のホスト（または空のディレクトリ）で `mamela restore` を試し、起動までを確かめる。取れていても、戻せるとは限らない。

## 3. CP のホストを失ったときの復旧

順序が大事である。**鍵が先、DB が次、起動が最後。**

1. **新しいホストに CP を置く**（[lab.md](lab.md) の 5。`mamela` と、ユーザー `mamela-cp`、site・providers・トークン）。**mamelet がつなぐアドレスを同じにする**（`controlplane.url`。mamelet の `config.json` の `server` に固定されている。アドレスが変わるなら、各ノードの `server` を直す。旧 CP がないと `redirect` は使えない）。
2. **`mamela serve` を起動しない。**
3. 戻す（鍵のディレクトリと DB が空のホストで。上書きはしない。MAM-BAK-0003）:
   ~~~bash
   sudo -u mamela-cp mamela restore --in <backup> --site /etc/mamela/site.toml --db /var/lib/mamela/state.db
   ~~~
   まず archive を端から端まで検査する（欠け・書き換え・知らないファイル・新しすぎる DB は、何も書かずに MAM-BAK-0002・0004）。そのあと鍵、最後に DB を書く（途中で止まっても、「DB だけあって鍵がない」にはならない）。置いてあるものを置き換えるなら `--replace`（`名前.before-restore-<時刻>` に**移すだけ**で、消さない。`-wal`・`-shm` も一緒に移す）。
4. `mamela serve` を起動する。サーバー証明書のホストが違えば、起動時に CA から出し直す（mamelet には何も変わらない）。
5. **確かめる**（この順に）:
   1. ログに **MAM-PKI-0201・0203・BAK-0101 がない**。
   2. `mamela controlplane agents` に、全ノードがあり、`ended` がない。
   3. 各ノードの mamelet が報告している（`mamela plan <宣言>` が「報告を待つ」〔MAM-PLAN-0103〕のままにならず、Mamela の物が Foreign と出ない）。
6. バックアップの**あとに**あったこと（apply、登録、承認）は、DB にない。何があったかは、プロバイダー側の実物と、PR のコメント・commit status で見る。plan し直す。

## 4. 一部だけ戻した・失ったとき

| 状態 | 何が起きるか | どう分かるか | 直す |
|---|---|---|---|
| **DB だけ戻した。鍵がない** | `serve`・`apply`・`api` は**起動を拒否**する。以前は、新しい CA と署名鍵を黙って作って起動し、全 mamelet が止まった | **MAM-PKI-0201**（足りないファイルを名指す） | 鍵を戻す（`mamela restore`、またはバックアップの `cp/` のファイルを置く）。本当に失ったなら、`mamela controlplane new-keys --confirm` で新しく作る（DB は残る）。全ノードの再登録が要る（6.4） |
| **鍵はある。DB がない・新しい** | 起動はするが、登録済みの mamelet が 1 つもない。全 mamelet が 403。Mamela の物は全部 Foreign（plan は何も変えない） | **MAM-PKI-0203**（起動時） | DB を戻す。なければ、ノードごとに `mamela controlplane reenroll --node <名前> --vmid <config.json の vmid>` で再登録する。Foreign になった物は、プロバイダーの引き継ぎ（adopt）の明示操作で自分の物にする（[lab.md](lab.md) の 3.4） |
| **古い DB を戻した** | mamelet は、受け入れた serial より小さい revision を**拒否する**（ロールバックの検知）。plan は待ちのまま | **MAM-BAK-0101**（ログ。mamelet の報告から） | 5 |
| **どちらも失った** | 新しい CA で最初から。登録も作成の記録もない | — | 6.4 の手順で全ノードを登録し直し、Foreign は引き継ぐ |

## 5. DB が mamelet より古いとき

revision の serial は、ノードごとに増える。mamelet は、受け入れた最大の serial（`<stateDir>/state.json` の `lastSerial`）より小さい serial の revision を拒否する（`rolled back`）。古い DB を戻すと、DB のほうが小さい。CP は、mamelet の報告（`serial`、`refused`）か、`wait` の `after` からこれを見つけて、ノードごとに **MAM-BAK-0101** をログに出す（6 時間に 1 回）。

1. **より新しいバックアップを戻す。** これが正しい直し方である。
2. なければ、**そのノードの mamelet に serial を忘れさせる**。ノードで root:
   ~~~bash
   systemctl stop mamelet
   # <stateDir>/state.json の "lastSerial" を 0 にする（stateDir は /etc/mamelet/config.json。Mamela が作ったノードは /var/lib/mamelet）
   systemctl start mamelet
   ~~~
   - **`attempts` は消さない。** 一度きりの操作（パッケージの導入など）を実行済みと覚えているのはそこで、消すと、revision に残った承認済みの操作がもう一度実行されうる。
   - **限界:** ロールバックの検知は、古い revision の再送（承認が取り消された許可の再実行）を防ぐ仕組みである。止めている間は、その守りがない。人がそのノードで行う作業で、期限（revision は取るたびに 24 時間）が守りになる。

## 6. 証明書

### 6.1 寿命と更新

| 証明書 | 寿命 | 更新 |
|---|---|---|
| mamelet のクライアント証明書 | 1 年 | **mamelet が残り 1/3（約 8 か月後）で自分で更新する**（0.3.2 から）。失敗したら 1 時間ごとにやり直す |
| CP のサーバー証明書 | 1 年 | **CP が動いたまま**、接続のたびに確かめ、30 日前になったら CA から出し直して保存する（MAM-PKI-0103）。再起動は要らない |
| CA | 10 年 | **しない**（6.4）。残り 1 年から、CP が毎日ログに警告する（MAM-PKI-0106） |
| revision の署名鍵（Ed25519） | 期限なし | **しない**（6.4） |

**mamelet の更新（`POST /v1/nodes/<node>/renew`）:**

- mamelet が、いまの証明書（mTLS）で認証して、**いまの鍵で署名した証明書要求**を送る。CP は、残りが寿命の半分より少ない証明書だけを更新し（MAM-PKI-0105。証明書を集められないように）、同じ登録の新しい証明書を返す。mamelet は `cert.pem` を原子的に置き換えて（fsync して rename）、すぐに使う。
- **鍵は変えない。** `cert.pem` と `key.pem` が、停電のあとでも必ず対になる（置き換えるのは 1 つのファイルだけ）。鍵を替えるのは再登録（6.3）である。
- **登録の同一性は変わらない。** DB の `agents.cert_serial` は、登録のときの証明書の serial のまま**更新しない**。更新した証明書は、CA が付ける Subject の `serialNumber` にそれを持つ。理由は 3 つある。(1) plan・承認・明示操作は、この serial に結び付けている（`AgentRegistrations`、`explicit_operations`）。更新で変えると、更新をまたいだ承認が `MAM-NODE-0101` で無効になる。(2) 応答が届かなかった（CP が書いたあとに通信が切れた）mamelet が締め出されない。古い証明書は、期限まで使える。(3) 古い DB を戻しても（5）、更新した証明書が通る。**再登録**は新しい登録になり、前の証明書は（更新したものも）すべて使えなくなる。
- **更新できるのは、有効な証明書だけ。** 切れた証明書は TLS で断られる（6.3）。

**終わりの見方:**

- `mamela controlplane agents`: 登録済みのノードと、証明書の終わり、`ok`・`ends soon`（30 日以内）・`ended`。
- `mamela plan`: 終わりが近い・終わった証明書があれば警告を出す（MAM-PKI-0101・0102。plan の digest には入らない）。
- CP のログ: 起動時と 1 日に 1 回の確認、そのノードが来たとき（6 時間に 1 回）、切れた証明書の TLS の失敗（MAM-PKI-0102）。
- mamelet のログ（ノード）: 更新できないとき（MAM-PKI-0104）、切れたとき（0102）。
- 終わりは、登録と更新のときに、DB の event に記録する。記録のない古い登録は、「登録から 1 年」と**見積もる**（一覧に `(estimated)`）。

### 6.2 版の組み合わせ

| CP | mamelet | 起きること |
|---|---|---|
| 0.3.2 以降 | 古い（0.3.1 以前） | **何も変わらない。** 登録のときの証明書で動き続ける。更新はされない。終わりの 30 日前から、CP のログと plan が警告する。それまでに mamelet を更新する（[mamelet-update.md](mamelet-update.md)）か、切れたら再登録する |
| 古い | 0.3.2 以降 | 更新の要求は 404 になり、**何もしない**（証明書はそのまま）。1 時間ごとに試し、ログに MAM-PKI-0104 を 1 度だけ出す |
| 0.3.2 以降 | 0.3.2 以降 | 更新される |

- **先に CP、そのあと mamelet**（[mamelet-update.md](mamelet-update.md) と同じ順序）。
- **更新のあとで、0.3.2 より古い CP に戻さない。** 更新した証明書は、古い CP には serial が DB と違って見え、断られる。最初の更新は登録から約 8 か月後である。
- revision・report の形は変えていない。古い mamelet が未知のメンバーで revision を断ることも、CP が報告を断ることも、この変更では起きない。

### 6.3 期限が切れた mamelet を救う

切れた証明書では、TLS の握手が終わらない（mamelet のログは `remote error: tls: expired certificate`、CP のログは `TLS handshake error`。CP は MAM-PKI-0102 を足す）。自分では登録し直せない（トークンは使うと消える）。**そのノードの鍵を再び登録する**。

1. CP のホストで（`mamela-cp` の権限で。DB を書く）:
   ~~~bash
   mamela controlplane agents --db /var/lib/mamela/state.db                      # 切れているノードを探す
   mamela controlplane reenroll --db /var/lib/mamela/state.db --node <名前>      # 一度きりのトークン（既定 1 時間、24 時間まで）
   ~~~
2. そのノードで root（`/etc/mamelet/config.json` の `enrollTokenFile`、`stateDir`）:
   ~~~bash
   systemctl stop mamelet
   # トークンと改行を enrollTokenFile に書く（mode 0600。Mamela が作ったノードは /etc/mamelet/enroll-token）
   rm <stateDir>/cert.pem          # key.pem は、登録のときに新しいものに替わる
   systemctl start mamelet         # 登録して、トークンのファイルを消して、動き続ける
   ~~~
3. `mamela controlplane agents` で終わりが延びたこと、mamelet が報告していることを確かめる。

- **再登録は新しい登録**である。前の証明書は使えなくなり、そのノードについて作った plan と承認は無効になる（MAM-NODE-0101）。plan し直して承認する。
- **一度に 1 台ずつ。** トークンは、そのノードの名前と VMID にだけ効く。
- **CP のホストを失って DB もない**ときは、`--vmid <config.json の vmid>` を付ける（DB が知らないノードにも、この 1 台分のトークンを出す）。

### 6.4 CA と revision の署名鍵（0.3.2 では回転しない）

**してないこと:** CA の更新、revision 署名鍵の回転。mamelet は `config.json` の `ca` と `revisionKey` を**1 つだけ**固定していて、重ねて信頼する仕組み（複数の CA）がない。0.4 に回す。

**限界:**

- CA は最初の `serve` から **10 年**で終わる。終わると全ノードが止まる。`openssl x509 -enddate -noout -in /var/lib/mamela/cp/ca.pem` で見られる。残り 1 年から CP のログに警告が出る（MAM-PKI-0106）。
- 新しい CA に替えると、**全ノードが一斉に新しい CA を知らなければならず、移行の間に両方が通ることはない。** CP を新しい CA で起動した瞬間から、各ノードは、手で直されるまでつながらない。mamelet は、取得済みの revision の期限（24 時間）の間は、決まった修復だけを続ける。
- revision の署名鍵が漏れたら、攻撃者が全ノードに revision を署名できる。替えるのは同じ手順になる（`new-keys` は CA と一緒に作り直す）。期限はないので、漏れない限り替える必要はない。

**手順（全ノード。時間を決めて行う）:**

1. 古い CA・鍵のファイルを控える（`mamela backup`）。`mamela controlplane agents` でノードの一覧を取る。
2. `mamela serve` を止め、`ca.pem` `ca-key.pem` `revision-key.pem`（と `server.pem` `server-key.pem`）を鍵のディレクトリの外に**移す**（消さない。全ノードを直し終わるまで取っておく）。
3. `mamela controlplane new-keys --confirm`（DB は残る）。新しい CA・署名鍵・サーバー証明書を作り、各ノードに入れる `ca` と `revisionKey` を表示する（あとで `mamela controlplane trust` でも出る）。
4. `mamela serve` を起動する。
5. **ノードごとに:** `mamela controlplane reenroll --node <名前>` のトークンを取り、ノードで `config.json` の `ca` と `revisionKey` を 3 の値に替え、6.3 の手順（トークン、`cert.pem` を消す、再起動）を行う。
6. 全ノードが報告したら、2 で移したファイルを、バックアップの外で安全に処分する。

DB が残っているので、作成の記録、承認、revision の serial は続く。mamelet の `lastSerial` を替える必要はない。

## 7. 警告とコード

[reason-codes.md](reason-codes.md) の `MAM-PKI`（証明書と鍵）、`MAM-BAK`（バックアップと復元、古い DB）、`MAM-NODE-0201`・`0202`（CP が mamelet に返す拒否）。
