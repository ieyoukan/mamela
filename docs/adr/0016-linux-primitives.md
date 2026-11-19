# ADR 0016 — マシンの中は、組み込みの拡張 linux の、安全な少数の部品で宣言する

- 状態: Accepted（2026-11-17）
- 日付: 2026-11-17
- 対象: ADR 0006（核は特定のインフラを知らない）、0013（宣言から消えたものの扱い）。[ノード拡張](../design/node-extensions.md)の案 A と段 5。監査 A4（秘密）は範囲の外

## 文脈

Mamela は、マシンを作り（provider）、そこに APT のパッケージと systemd のサービスを置ける（`NodePackage`、`NodeService`）。アプリを動かすには、それでは足りない。設定ファイル、サービスのユーザー、APT のソース、リリースのバイナリ。Ansible ならタスクで書くものを、宣言で書けない。

道は二つあった。

- **任意のコマンドを、承認つきで実行する。** 何でも書ける。だが mamelet は root で動き、**固定のプログラムだけ**を実行することで、乗っ取られても被害が限られている（[ノード拡張](../design/node-extensions.md)の選択肢 B・C を採らない理由）。承認する人が、シェルの一行が何をするかを読み切ることも期待できない。plan が「何が変わるか」を言えなくなる。
- **安全に表せる部品を、少数だけ持つ。** 内容と結果を plan に全文で出せて、mamelet が決まったコードで行えるもの。アプリの導入は、その部品を組み合わせた Emela の関数（Pome）にする。

アプリの導入の多くは、「ユーザーを作る、ソースを足す、パッケージを入れる、バイナリを置く、設定を書く、サービスを動かす」の組み合わせである（community-scripts の多くのスクリプトがそうである）。

## 決定

### 1. 部品は、組み込みの拡張 `linux` の kind

| kind | すること | 版 |
|---|---|---|
| `NodeFile` | 宣言したとおりの内容・mode・所有者のファイル。変わったら、挙げたサービスを再起動する | 0.4.0（この ADR とともに） |
| `NodeUser` | ユーザーと補助グループ、その SSH の鍵（許すこと、つまり sudo は後） | 0.4.0 |
| `NodeAptRepo` | APT のソースと、その鍵 | 0.4 の続き |
| `NodeArtifact` | sha256 で固定したダウンロード（リリースのバイナリ、`.tar.gz` の 1 つのメンバー）。mamelet が Go のコードで https からダウンロードし、置いたものを自分の状態ディレクトリに記録する | 0.4.0 |

- 拡張は、k3s と同じにコンパイル時に組み込む（`internal/ext/linux`、`cmd/mamela` と `cmd/mamelet` の blank import）。実行時に読み込むプラグインはない。
- `NodePackage` と `NodeService` は核に残す。linux は、パッケージの導入が待つもの（`PackageGate`、NodeAptRepo のため）と、サービスの起動が待つもの（`StartGate`、`restart` に挙げたファイル）で関わる。
- kind の並び（digest に入る）は、核と k3s と provider のあとに 6〜9。一つの plan の操作は並びの順に走るので、アカウント、ソース、ダウンロード、ファイルの順にした（NodeUser 6、NodeAptRepo 7、NodeArtifact 8、NodeFile 9）。理由コードの範囲も kind ごとに予約した（`internal/ext/linux/codes.go`。操作の名前は `doc.go`）。

### 2. 任意のシェルはない

- mamelet が実行するのは、核の固定のプログラム（`dpkg-query`、`apt-get`、`systemctl`）と、拡張が登録した固定のプログラムだけ。ファイルは Go のコードで書き、`systemctl daemon-reload` と `try-restart` を使う。linux が足すプログラムは、NodeUser の `/usr/sbin/useradd` と `/usr/sbin/usermod` だけで、引数は mamelet が検査した値から決まった形で組み、名前は `--` のあとに置く（`/etc/passwd` を Go で書き換えると、`/etc/shadow` やロックとの整合を自分で持つことになる）。`authorized_keys` は Go のコードで書く。
- 宣言がコマンドや引数を書くことはない。値は、名前・path・mode の狭い形で検査し、graph と mamelet の両方で同じ関数を使う（`CheckPath`、`ParseMode`、`ValidAccount`、NodeUser の `CheckHome`、`ParseAuthorizedKey`）。mamelet は、署名つきの revision が何と言っても、書く前にもう一度確かめる。
- `NodeFile` は、カーネルとブートのもの、mamelet 自身、k3s のもの、ユーザーと sudoers、ほかの部品のもの（APT のソースと鍵、`authorized_keys`）、root が名前で実行し黙って信頼するもの（§8）を書かない。シンボリックリンクはたどらない。
- 部品の操作は、すべて承認が要る（拡張は `Auto` を返せない）。plan は、書く内容の全文と、変わる属性、作るディレクトリ、再起動するサービスを示す。

### 3. 能力の申告（capability negotiation）

linux は、wire の汎用の形（段 5）を使う最初の拡張である。revision の `ext.linux`、報告の `ext.linux`。

- mamelet は、受け取る拡張と、その部分の版を報告に書く（`extensions: {"linux": 1}`）。CP は、ノードの最新の報告が linux をその版以上で挙げているときだけ、revision に `ext.linux` を入れる。判定は一つの関数（`noderev.Takes`）で、Publish、grant、`mamela serve` が配るときに同じものを使う。使う報告は、ノードのいまの登録（VMID と登録の時刻）のものだけ（`store.CurrentNodeReport`。登録し直した 0.9.x の mamelet が、前の mamelet の報告で `ext` を受け取らない）。0.9.x の mamelet（revision を、知らないメンバーを拒んで読む）は `ext` を受け取らず、そのまま動き続ける。
- そのノードの linux の資源は、plan で Blocked になる（MAM-PLAN-0122）。メッセージは、動いている mamelet の版と、更新の仕方（`Mamela.mamelet`、[mamelet の更新](../mamelet-update.md)）を言う。承認しても、grant は出さない（MAM-NODE-0104）。ただし、同じ宣言がそのノードの mamelet を更新するとき（`NodeMamelet` が同じ plan で change、または 1 台ずつの順番待ち）は、Blocked でなく Waiting（MAM-PLAN-0123）にする。それが文書の手順（更新と資源を一度に宣言する）で、plan に Blocked があると mamelacd の `sync = "push"` は承認せず、更新そのものが走らないからである。
- 二重の防御として、mamelet は、知らない拡張の部分と、拡張が読めない部分だけを断り（報告の `extRefused`）、残りの revision を続ける。

### 4. 更新は CP が先、mamelet があと

新しい CP は、旧い報告（`extensions` なし）も新しい報告も読む。旧い CP は、新しい mamelet の報告を、知らないメンバーとして拒む。だから CP を先に更新し、そのあと mamelet を（mamelet の更新の仕組みで）更新する。linux を受け取る mamelet は 0.10.0 から（`internal/mamelet/release.go`）。

**この更新は、そのままでは戻せない。** 0.10.0 の mamelet が一度報告するか、`ext` のある revision が DB に入ると、0.3.x の CP（DB の報告と revision を、知らないメンバーを拒んで読む）は、それを読めず、観測（Observe）が失敗し、すべての plan が止まる。だから CP を更新する**前に** `mamela backup` を取る。戻し方は [復旧](../recovery.md) の 8（更新前のバックアップを戻すか、最小の手で DB から読めないものを除く）。0.4.0 からの CP は、DB の報告と revision を、知らないメンバーを拒まずに読む（`internal/store/nodes.go`。届いた報告は今までどおり厳しく読む）ので、0.4.0 より後の版から 0.4.0 に戻すときは、これは起きない。

### 5. 宣言から外したものは残す（この版の制限）

宣言から `NodeFile` を外しても、ファイルはノードに残る（`NodeArtifact` も同じ。MAM-PLAN-0313）。plan は、それを retained（MAM-PLAN-0310）で示し、apply のあと mamelet は観測しなくなる。retained で示すのは、いま効いている宣言（最後に apply した plan の宣言、または承認した操作の宣言。revision の部分の `inForce`）が持つものだけで、ほかの宣言（PR）が plan しただけのものは示さない（Mamela が書いたかもしれないものだけが「残る」）。消すのは手で。`NodeUser` も同じで、ユーザーはグループ・home・鍵とともに残る（MAM-PLAN-0311）。グループから外すこと、uid と home を変えることもしない（ユーザーのファイルの所有者が古いままになる）。ADR 0013 の削除（その宣言が作ったものだけ、ホストでの承認）は provider の資源のもので、ノードの中のものには、まだ広げない。誰が作ったファイルか（パッケージが置いた設定を上書きしたのか、新しく作ったのか）を記録してからでないと、消してよいかを決められないからである。

### 5a. 観測の対象は宣言の和、許可は承認した値だけ、書く前に比べる（2026-11-18）

Control Plane は、承認していない宣言（PR、ブランチ）からも plan を作り、そのたびにノードの revision に観測の対象を足す（`node.Publish`）。mamelacd は PR と main を交互に plan する。

- **観測の対象は和にする。** revision の各項目が mamelet に観測させるもの（所有者・グループの有無、ユーザーの shell・uid・作る home・グループ・鍵、APT のソースの URI と suite）は、plan したすべての宣言の和（重複なし、整列）。上の宣言で置き換えると、交互の plan のたびに revision が変わり（serial が上がり、`node_revisions` に写しが溜まる）、plan が互いの報告待ちで揺れた。和なら、両方が一度ずつ plan したあと、revision は変わらない。観測は、どの対象について計算したかを言う（所有者は名前ごとの有無、APT のソースは URI と suite の組ごとの `where`）。plan は自分の宣言の対象の観測だけを使い、なければ報告を待つ（ほかの宣言の URI で計算した `Unread` で in sync と言わない）。
- **和は限られ、apply で戻る。** 1 つの一覧が 16 を超えるなら、plan した宣言のものだけにする。承認されていない宣言だけが足した項目（許可も `inForce` もない）が 1 種別 64 を超えるなら、その宣言にないものを除く。承認した plan の apply（`node.Prune`）は、対象をその宣言のものだけに戻す（ほかの宣言のものは、その次の plan で戻る）。
- **許可は自分で完結する。** grant（書く・入れる・足す・ユーザーの変更）は、mamelet がすることに要る値をすべて持ち、mamelet は観測の対象と比べない。だから、ほかの宣言の plan が観測の対象を変えても、apply が配っている grant は消えず、そのまま行われる（以前は、ユーザーの観測の対象を置き換えると grant も消え、apply は 10 分後に結果不明で終わった）。
- **書く前に比べる（compare and swap）。** grant は、承認した plan の観測が見た前の状態（ファイルの sha256 か、ないこと。ユーザーの有無・shell・`authorized_keys`。ソースと鍵の 3 つのファイル）を持つ。mamelet は、ノードがまだその状態か、すでに承認した後の状態のときだけ行い、そうでなければ理由を言って断る（「plan のあとで変わった」。次の plan が、いまの状態を示す）。承認したある状態への変更が、ほかの状態（人の手、別の apply）を上書きしない。ユーザーを作る grant も、`authorized_keys` が「ない」ことを前の状態に持つ。作るユーザーがすでにあれば、mamelet は、それがこの grant の作るものとまったく同じとき（中断した試みが作ったもの。宣言した uid、または UID_MIN より下か上かが `system` のとおり、主グループがユーザーの名前のもの、shell と home、home がそのユーザーのもの、grant が足すほかにグループがない、鍵がないか承認したもの）だけ続け、ほかは何も変えずに「plan のあとで変わった」と断る（plan のあとで人が `useradd` して鍵を置いたユーザーを乗っ取らない）。鍵の比較は、グループと shell を変える前にも行う（断るときに途中まで変えない。2026-11-19）。
- **apply は古い grant を取り下げる。** 承認した plan の apply（`node.Prune`）は、その宣言と値の違う grant（v2 を書く grant が残っているのに、宣言が v1 に戻った）を取り下げる。残る隙: 結果不明で終わった apply の grant は、期限（待ち時間＋`GrantGrace`、最大で約 25 分、承認の期限まで）まで有効で、そのあいだに宣言を戻しても、それが in sync の plan だけなら apply は走らず、取り下げられない。戻した宣言の plan を（変化がなくても）承認して apply すれば、そこで取り下がる（`mamela approve <digest>` と `mamela apply`）。mamelet が戻ったとき、ノードがまだ前の状態なら、compare and swap は通る（上書きするのは、承認した前の状態だけ）。

### 6. 秘密は範囲の外

ファイルの内容は、宣言、plan（PR のコメント）、Control Plane の DB、ノードの revision に平文で残る。秘密は書かない。秘密鍵やトークンに見える内容は、graph の警告にする（MAM-GRAPH-0203）。秘密の扱い（暗号化して保管し、ノードにだけ渡す）は、監査 A4 の続きとして別に決める。

### 7. アプリの導入は Pome の関数、Go の拡張は複数ノードの調整だけ

- アプリの導入（レシピ）は、部品を組み合わせた Emela の関数として Pome で配る。community-scripts のような目録を、宣言の言葉で作れる。レシピは desired state を作るだけで、何も実行しない。plan に出るのは部品であり、承認するのも部品である。
- Go の拡張を新しく書くのは、部品では表せない、ノードをまたぐ順序と健全性の確認が要るもの（k3s のように、server が先、Ready を確かめて 1 台ずつ）だけにする。

### 8. mamelet の側の安全（2026-11-18 の見直し）

mamelet は root で動き、観測する path はユーザーが変えられるディレクトリ（home）にもある。独立したレビューの指摘から、次を決めた。

- **開くもの:** os.Root は、名前の最後のリンクを（root の中なら）どんなフラグでもたどる。だから mamelet は、上のディレクトリを 1 つずつ（リンクでなく、見たものと同じであることを確かめて）開き、ファイルは通常のファイルであることを見てから、システムコール（`golang.org/x/sys/unix`）で `O_NOFOLLOW|O_NOCTTY|O_NONBLOCK` で開き、開いたものを fstat で確かめる。mode・所有者・大きさは開いたものから取る。見てから開くまでの間にユーザーがリンクに置き換えても、端末（制御端末になれば、ユーザーの SIGINT・SIGHUP が mamelet と apt に届く）やデバイス（開くだけで動く `/dev/watchdog`）を開かない。作るディレクトリの所有者と mode も、開いたものから変える。ディレクトリも同じで、システムコールで、ディレクトリとしてだけ（`O_DIRECTORY|O_NOFOLLOW|O_NONBLOCK|O_NOCTTY`）開き、見たもの（device と inode）と同じかを確かめ、その中ですること（見る、作る、消す、名前を変える、所有者と mode）は、すべてその descriptor から（`*at` のシステムコールで）行う（`dirFD`）。os.Root の `OpenRoot` は最後の要素を `O_DIRECTORY` なしで開くので、自分のディレクトリ（`~/.ssh`）を、mamelet が見てから開くまでの間に FIFO に置き換えるユーザーは、mamelet を open(2) で止め続けられた（SIGTERM でも終わらず、ノードのすべての許可が止まる。2026-11-19 の再レビュー）。`O_DIRECTORY` なら、FIFO・ソケット・デバイスは、開く前に `ENOTDIR` になる。
- **ほかのユーザーの home:** `useradd --create-home` は、あるディレクトリを警告だけで home にする。mamelet は、作る home にすでに何かあれば作らず（plan も Blocked）、home がそのユーザーのもの、`~/.ssh` がそのユーザーか root のものでなければ、鍵を書かず chown もしない。graph は、宣言したユーザーの home の中のファイル、ほかの home の中の home を断る。作る home は、いつも名前を `useradd` に渡す（鍵のある system ユーザーで home を宣言しないものも `/home/<name>`。ノードの `/etc/default/useradd` の既定に任せると、plan も mamelet も、どこに作られるかを確かめられなかった）。作る home の上のディレクトリを root 以外が変えられるときも、plan は Blocked にし、mamelet は作らない（見てから `useradd` までの間に、そのユーザーが自分のディレクトリかリンクを置ける。2026-11-19）。
- **mamelet 自身:** 書かない場所に、mamelet と拡張が実行する固定のプログラム、動的リンカ・APT・dpkg の設定、すべてのサービスに効く systemd のもの（`service.d`、`system.conf`、generator、unit のディレクトリの `mamelet.service`）、誰でも書けるディレクトリを足した。ファイルは `mamelet` を再起動しない。
- **root が名前で実行し、黙って信頼するもの（2026-11-19 の再レビュー）:** 固定のプログラムの path だけを断っても、PATH の前のディレクトリの同じ名前（新しい `/usr/sbin/systemctl` は、dpkg とパッケージのスクリプトが PATH で呼ぶ `/usr/bin/systemctl` の代わりに動く）、useradd が読むもの（`/etc/default/useradd` の `SKEL=/var/lib/mamelet` は、mamelet の鍵を新しいユーザーの home に写す）、ライブラリ、APT が信頼するものが残っていた。パッケージのプログラムのディレクトリ（`/usr/sbin`、`/usr/bin`、`/sbin`、`/bin`）は名前を数え上げずに丸ごと断る（パッケージのスクリプトが呼ぶものは数え切れない。自分のプログラムは `/usr/local/bin` か `/opt/<app>` に置く）。`/usr/local/sbin`・`/usr/local/bin` は、root が実行するプログラムの名前だけを断る。ライブラリは、multiarch のディレクトリと `/lib64` などを丸ごと、`/lib`・`/usr/lib`・`/usr/local/lib` の下は shared object を断る（`/usr/local/lib` は Debian の `ld.so.conf` にあり先に探される）。APT のソースと鍵は NodeAptRepo だけが書く（手で書いた `[trusted=yes]` のソースは Signed-By と origin の方針を迂回する）。pinning（`preferences`）も、mamelet の導入が入れる版と出どころを変えるので断る（警告にしなかったのは、plan が導入するパッケージの版の出どころを pinning から説明できないから）。`slice.d`・`system.slice`・`-.slice` は `service.d` と同じ理由で断る。`/etc/skel` は書ける（写したものは新しいユーザーのもので、root の権限にならない。`authorized_keys` はもともと断る）。どれも理由と代わりを言う（`internal/ext/linux/paths.go`）。
- **一度だけの許可と中断:** SIGTERM で中断した試みは、失敗として記録しない（許可が有効な間、次の周に続ける。どの試みも、中断のあとにやり直せるように作ってある）。書いたあとの `daemon-reload` と再起動が終わらなければ、mamelet はそれを状態ディレクトリに記録して報告し、plan はもう一度承認を求める（MAM-PLAN-0218）。許可なしに再起動をやり直すことはしない。
- **大きさ:** 1 つのノードの部品の数（ファイル 256、ユーザー 64、ソース 32、artifact 64）、報告する鍵の数（65）、linux の報告（512 KiB。超えれば、詳しいことを除いて理由だけを送る）に上限を置く。ノードの上のものが報告を Control Plane の上限（1 MiB）より大きくし、ノードが止まったように見えることがないように。artifact は、変わったとき（inode・大きさ・mtime・ctime）だけ計り直す。書く前とダウンロードの前に、ファイルシステムの空きを確かめる。

ここで足した報告と revision のメンバー（`exposed`、`pending`、`newHomes`、`notOwned`、`more`、ユーザーの `homes`、`error`）は、linux の部分の版（`Version` 1）を上げずに足した（作る home は 5a の観測の対象の和で、`homes` と、home ごとの `newHomes`。形は `internal/ext/linux/testdata/wire/v1.txt`）。linux を受け取る mamelet 0.10.0 は、まだリリースしていない（タグがない）からである。リリースのあとに足すものは版を上げる。

## 結果

- 良い: マシンの中の構成を宣言で書け、plan が何が変わるかを全文で言える。mamelet は固定のコードとプログラムのまま。旧い mamelet のノードは壊れず、plan が更新を求める。
- 悪い: 部品にないこと（任意のスクリプト、テンプレートの展開をノードで行うこと、ファイルの削除）はできない。内容の大きいもの（256 KiB 超）は NodeArtifact を待つ。秘密は置けない。
- 残り: k3s を汎用の形に載せ替える（段 5 の残り）、拡張の名前と版を node provider の manifest に入れる、ファイルの削除（作成の記録とともに）、秘密（A4）。
