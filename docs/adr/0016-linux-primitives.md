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
- kind の並び（digest に入る）は、核と k3s と provider のあとに 6〜9。一つの plan の操作は並びの順に走るので、アカウント、ソース、ダウンロード、ファイルの順にした（NodeUser 6、NodeAptRepo 7、NodeArtifact 8、NodeFile 9）。理由コードと操作の名前の範囲も、kind ごとに予約した（`internal/ext/linux/doc.go`）。

### 2. 任意のシェルはない

- mamelet が実行するのは、核の固定のプログラム（`dpkg-query`、`apt-get`、`systemctl`）と、拡張が登録した固定のプログラムだけ。ファイルは Go のコードで書き、`systemctl daemon-reload` と `try-restart` を使う。linux が足すプログラムは、NodeUser の `/usr/sbin/useradd` と `/usr/sbin/usermod` だけで、引数は mamelet が検査した値から決まった形で組み、名前は `--` のあとに置く（`/etc/passwd` を Go で書き換えると、`/etc/shadow` やロックとの整合を自分で持つことになる）。`authorized_keys` は Go のコードで書く。
- 宣言がコマンドや引数を書くことはない。値は、名前・path・mode の狭い形で検査し、graph と mamelet の両方で同じ関数を使う（`CheckPath`、`ParseMode`、`ValidAccount`、NodeUser の `CheckHome`、`ParseAuthorizedKey`）。mamelet は、署名つきの revision が何と言っても、書く前にもう一度確かめる。
- `NodeFile` は、カーネルとブートのもの、mamelet 自身、k3s のもの、ユーザーと sudoers、ほかの部品のもの（`mamela-*` の APT ソースと鍵、`authorized_keys`）を書かない。シンボリックリンクはたどらない。
- 部品の操作は、すべて承認が要る（拡張は `Auto` を返せない）。plan は、書く内容の全文と、変わる属性、作るディレクトリ、再起動するサービスを示す。

### 3. 能力の申告（capability negotiation）

linux は、wire の汎用の形（段 5）を使う最初の拡張である。revision の `ext.linux`、報告の `ext.linux`。

- mamelet は、受け取る拡張と、その部分の版を報告に書く（`extensions: {"linux": 1}`）。CP は、ノードの最新の報告が linux を挙げているときだけ、revision に `ext.linux` を入れる。0.9.x の mamelet（revision を、知らないメンバーを拒んで読む）は `ext` を受け取らず、そのまま動き続ける。
- そのノードの linux の資源は、plan で Blocked になる（MAM-PLAN-0122）。メッセージは、動いている mamelet の版と、更新の仕方（`Mamela.mamelet`、[mamelet の更新](../mamelet-update.md)）を言う。承認しても、grant は出さない（MAM-NODE-0104）。
- 二重の防御として、mamelet は、知らない拡張の部分と、拡張が読めない部分だけを断り（報告の `extRefused`）、残りの revision を続ける。

### 4. 更新は CP が先、mamelet があと

新しい CP は、旧い報告（`extensions` なし）も新しい報告も読む。旧い CP は、新しい mamelet の報告を、知らないメンバーとして拒む。だから CP を先に更新し、そのあと mamelet を（mamelet の更新の仕組みで）更新する。linux を受け取る mamelet は 0.10.0 から（`internal/mamelet/release.go`）。

### 5. 宣言から外したものは残す（この版の制限）

宣言から `NodeFile` を外しても、ファイルはノードに残る（`NodeArtifact` も同じ。MAM-PLAN-0313）。plan は、それを retained（MAM-PLAN-0310）で示し、apply のあと mamelet は観測しなくなる。消すのは手で。`NodeUser` も同じで、ユーザーはグループ・home・鍵とともに残る（MAM-PLAN-0311）。グループから外すこと、uid と home を変えることもしない（ユーザーのファイルの所有者が古いままになる）。ADR 0013 の削除（その宣言が作ったものだけ、ホストでの承認）は provider の資源のもので、ノードの中のものには、まだ広げない。誰が作ったファイルか（パッケージが置いた設定を上書きしたのか、新しく作ったのか）を記録してからでないと、消してよいかを決められないからである。

### 6. 秘密は範囲の外

ファイルの内容は、宣言、plan（PR のコメント）、Control Plane の DB、ノードの revision に平文で残る。秘密は書かない。秘密鍵やトークンに見える内容は、graph の警告にする（MAM-GRAPH-0203）。秘密の扱い（暗号化して保管し、ノードにだけ渡す）は、監査 A4 の続きとして別に決める。

### 7. アプリの導入は Pome の関数、Go の拡張は複数ノードの調整だけ

- アプリの導入（レシピ）は、部品を組み合わせた Emela の関数として Pome で配る。community-scripts のような目録を、宣言の言葉で作れる。レシピは desired state を作るだけで、何も実行しない。plan に出るのは部品であり、承認するのも部品である。
- Go の拡張を新しく書くのは、部品では表せない、ノードをまたぐ順序と健全性の確認が要るもの（k3s のように、server が先、Ready を確かめて 1 台ずつ）だけにする。

### 8. mamelet の側の安全（2026-11-18 の見直し）

mamelet は root で動き、観測する path はユーザーが変えられるディレクトリ（home）にもある。独立したレビューの指摘から、次を決めた。

- **開くもの:** os.Root は、名前の最後のリンクを（root の中なら）どんなフラグでもたどる。だから mamelet は、上のディレクトリを 1 つずつ（リンクでなく、見たものと同じであることを確かめて）開き、ファイルは通常のファイルであることを見てから、システムコール（`golang.org/x/sys/unix`）で `O_NOFOLLOW|O_NOCTTY|O_NONBLOCK` で開き、開いたものを fstat で確かめる。mode・所有者・大きさは開いたものから取る。見てから開くまでの間にユーザーがリンクに置き換えても、端末（制御端末になれば、ユーザーの SIGINT・SIGHUP が mamelet と apt に届く）やデバイス（開くだけで動く `/dev/watchdog`）を開かない。作るディレクトリの所有者と mode も、開いたものから変える。
- **ほかのユーザーの home:** `useradd --create-home` は、あるディレクトリを警告だけで home にする。mamelet は、作る home にすでに何かあれば作らず（plan も Blocked）、home がそのユーザーのもの、`~/.ssh` がそのユーザーか root のものでなければ、鍵を書かず chown もしない。graph は、宣言したユーザーの home の中のファイル、ほかの home の中の home を断る。
- **mamelet 自身:** 書かない場所に、mamelet と拡張が実行する固定のプログラム、動的リンカ・APT・dpkg の設定、すべてのサービスに効く systemd のもの（`service.d`、`system.conf`、generator、unit のディレクトリの `mamelet.service`）、誰でも書けるディレクトリを足した。ファイルは `mamelet` を再起動しない。
- **一度だけの許可と中断:** SIGTERM で中断した試みは、失敗として記録しない（許可が有効な間、次の周に続ける。どの試みも、中断のあとにやり直せるように作ってある）。書いたあとの `daemon-reload` と再起動が終わらなければ、mamelet はそれを状態ディレクトリに記録して報告し、plan はもう一度承認を求める（MAM-PLAN-0218）。許可なしに再起動をやり直すことはしない。
- **大きさ:** 1 つのノードの部品の数（ファイル 256、ユーザー 64、ソース 32、artifact 64）、報告する鍵の数（65）、linux の報告（512 KiB。超えれば、詳しいことを除いて理由だけを送る）に上限を置く。ノードの上のものが報告を Control Plane の上限（1 MiB）より大きくし、ノードが止まったように見えることがないように。artifact は、変わったとき（inode・大きさ・mtime・ctime）だけ計り直す。書く前とダウンロードの前に、ファイルシステムの空きを確かめる。

ここで足した報告と revision のメンバー（`exposed`、`pending`、`newHome`、`notOwned`、`more`、ユーザーの `home`、`error`）は、linux の部分の版（`Version` 1）を上げずに足した。linux を受け取る mamelet 0.10.0 は、まだリリースしていない（タグがない）からである。リリースのあとに足すものは版を上げる。

## 結果

- 良い: マシンの中の構成を宣言で書け、plan が何が変わるかを全文で言える。mamelet は固定のコードとプログラムのまま。旧い mamelet のノードは壊れず、plan が更新を求める。
- 悪い: 部品にないこと（任意のスクリプト、テンプレートの展開をノードで行うこと、ファイルの削除）はできない。内容の大きいもの（256 KiB 超）は NodeArtifact を待つ。秘密は置けない。
- 残り: k3s を汎用の形に載せ替える（段 5 の残り）、拡張の名前と版を node provider の manifest に入れる、ファイルの削除（作成の記録とともに）、秘密（A4）。
