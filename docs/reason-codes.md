# 理由コード

Mamela が止まったり拒否したりしたときに表示するコード。一度決めたコードを別の意味に使い回さない。正は `internal/graph/problem.go`・`internal/graph/warnings.go`、`internal/evaluate/errors.go`、`internal/policy/policy.go`、`internal/plan/plan.go`・`internal/plan/kit.go`、ノード拡張の `internal/ext/k3s`・`internal/ext/linux`（linux の kind ごとに予約したコードの範囲は `internal/ext/linux/doc.go`）、`internal/reconcile/reconcile.go`、`internal/provider/host/errors.go`、`internal/provider/host/upstream.go`（MAM-PRV-0012）、`provider/protocol/catalog.go`（MAM-PRV-0011）、`internal/provider/external/pin.go`（MAM-PRV-0009、0010）、`cmd/mamela/service.go`、`cmd/mamela/revoke.go`（MAM-APR）、`internal/store/migrate.go`（MAM-DB）と `internal/provider/external/runtime.go`（MAM-SITE）、`cmd/mamela/act.go`（MAM-ACT）、`internal/cp`・`cmd/mamela/agentcerts.go`・`cmd/mamela/newkeys.go`・`cmd/mamelet/main.go`（MAM-PKI）、`cmd/mamela/backup.go` と `internal/cp/server.go`（MAM-BAK）。

使わなくなったコードも、行を消さずに「廃止」と書いて残す。別の意味で使い直さないためである。

プロバイダーの guard の結果のコード（`mamela guard` と `mamela apply` の前後に表示する）は、プロバイダーが決める。Mamela のコードではない。Proxmox のプロバイダーの `PVE-*` は [mamela-proxmox の README](https://github.com/ieyoukan/mamela-proxmox) にある。例の kv プロバイダーの `KV-*` は `examples/kv/kvprovider` にある。以前の `LAB-*` は、Mamela 本体からなくなった（ADR 0006）。

## MAM-CLI — コマンド

| コード | 意味 | よくある原因と対処 |
|---|---|---|
| MAM-CLI-0001 | 上のどの分類にも入らない失敗（`mamela check --json` の診断） | メッセージに従う |

## MAM-EVAL — 評価

| コード | 意味 | よくある原因と対処 |
|---|---|---|
| MAM-EVAL-0001 | ツールチェーンが見つからない、または `toolchain.lock` の実行ファイルと一致しない | `mamela toolchain install`（リポジトリでは `scripts/install-toolchain.sh`）。`mamela toolchain` で、どれを使っているかを確かめる |
| MAM-EVAL-0101 | ビルドに失敗した | コンパイルエラー、Pome でない、キャッシュにない依存（ネットワークからは取らない） |
| MAM-EVAL-0102 | ビルドが時間内に終わらなかった | |
| MAM-EVAL-0103 | キャッシュにある依存が Pome.lock の hash と違う（ビルドしていない） | キャッシュが書き換わった・壊れた。該当する依存をキャッシュから消し、`emela pome install`（`scripts/install-toolchain.sh`）で取り直す |
| MAM-EVAL-0104 | `Pome.toml` の `[replace]` が、Pome のないディレクトリを指している（ビルドしていない） | パスは、その `Pome.toml` のあるディレクトリからの相対。例を単体でコピーすると届かなくなる。リポジトリ全体をコピーする、パスを直す、または `[replace]` を消して `[dependencies]` の公開版を使う（Pome キャッシュに `emela pome install` で入れておく） |
| MAM-EVAL-0201 | ビルド結果を検査できない | WASI 0.3 component 以外、または読めない形式 |
| MAM-EVAL-0202 | 許可されていない import がある（実行していない） | `Std.Env` / `Std.Random` / `Std.Clock` / `Std.FileSystem` / `Std.Http` を使っている |
| MAM-EVAL-0301 | 宣言が error で終わった | 宣言の中の `fail` |
| MAM-EVAL-0302 | 宣言が defect で終わった | ゼロ除算などのバグ |
| MAM-EVAL-0303 | 時間内に終わらなかった | 終わらない再帰 |
| MAM-EVAL-0304 | メモリ上限を超えた | 際限なく大きくなるデータ |
| MAM-EVAL-0305 | stdout に書きすぎた | Graph を 2 回以上書いている、診断を stdout に書いている |
| MAM-EVAL-0306 | Wasmtime が異常終了した | stderr の詳細を見る |
| MAM-EVAL-0401 | 2 回の評価で結果が違った | ツールチェーンか Mamela のバグ |

## MAM-GRAPH — Graph の形式

| コード | 意味 |
|---|---|
| MAM-GRAPH-0001 | JSON として正しくない（途中で切れた、Graph 以外を出力した、不正な UTF-8） |
| MAM-GRAPH-0002 | `schema` が対応していない値 |
| MAM-GRAPH-0003 | 定義されていないメンバーがある。組み込みの kind のリソースに `after` を書いた（`after` を持てるのはプロバイダーのリソースだけ。マシンの上のものは、もともとマシンを待つ） |
| MAM-GRAPH-0004 | 必要なメンバーがない、または `null` |
| MAM-GRAPH-0005 | 知らない kind。組み込みの kind をプロバイダーのリソースとして書いた。組み込みの `VirtualMachine`（ADR 0006 でなくなった）。マシンはプロバイダーの Pome で宣言する（Proxmox なら `github.com/ieyoukan/mamela-proxmox/pome`）。Mamela の Pome は 0.3.0 を使う |
| MAM-GRAPH-0006 | 文書が大きすぎる |
| MAM-GRAPH-0007 | 値の型が違う（整数でない数値、文字列のところの数値など）。省略できるメンバー（k3s の `server`、mamelet の `version`）に `null` を書いた（書かずに省略する）。プロバイダーの `spec` の `null`、深さ 32 を超える入れ子、2^53 を超える整数 |
| MAM-GRAPH-0008 | 同じメンバーが 2 回ある |
| MAM-GRAPH-0009 | 文書の後にデータが続いている |
| MAM-GRAPH-0010 | リソースが多すぎる |
| MAM-GRAPH-0011 | プロバイダーのリソース（`provider` を持つリソース）が `mamela-graph/v1` の文書にある（v2 が要る） |

## MAM-GRAPH — Graph の意味

| コード | 意味 |
|---|---|
| MAM-GRAPH-0101 | 名前が正しくない（プロバイダーのリソースの kind・provider・name の形） |
| MAM-GRAPH-0102 | 同じリソースが 2 回宣言されている |
| MAM-GRAPH-0103 | 参照している node が Graph にない（プロバイダーのリソースとして宣言されていない）。k3s の agent の server が、k3s の server として宣言されていない。`after` の参照（`"Kind name"`）が、同じ Graph のプロバイダーのリソースではない（宣言していない、組み込みの kind のリソース、書き間違い）。**プロバイダーが動いたあと（plan の観測の前）**: 同じ名前のマシンが 2 台ある（どのリソースか示す）、node が machine（マニフェストで `node: true` の kind）ではないリソースを指している。同じ名前でも、machine と disk のように kind が違えば、machine だけが node になる |
| MAM-GRAPH-0104 | 名前が `<node>/<package>` などの形と合わない |
| MAM-GRAPH-0105 | 廃止（ADR 0006）。値が範囲外（組み込みの VirtualMachine の vCPU・メモリ・ディスク）。マシンの大きさの上限は site の予算（MAM-SITE-0305）が決める |
| MAM-GRAPH-0106 | 値が正しくない（パッケージ名、サービス名、`Present` 以外の状態、k3s の版・sha256・役割、k3s の server の空の `server`、マシンの `spec.network`）。`spec.network` が、`mode`・`address`・`gateway`・`dns` だけを持つオブジェクトでない（文字列・配列・未知のメンバー・型違い）ものも、これで拒否する |
| MAM-GRAPH-0107 | 同じ固定アドレスが複数のマシンに宣言されている（マシンの `spec.network.address`） |
| MAM-GRAPH-0108 | `after` の依存が循環している（自分自身を待つものも含む。ADR 0007）。循環に入るリソースごとに、そのリソースから一周する経路（`Thing a -> Thing b -> Thing a`）を示して報告する。経路の中の依存を 1 つ外す |
| MAM-GRAPH-0109 | 文字列が、見たとおりのものとして表示されない文字を含む。C0・DEL・C1 の制御文字（k3s の manifest の内容と provider の `spec` の文字列は、改行 `\n` とタブ `\t` だけ許す。復帰 `\r`、ESC、ベルを含む）、bidi 制御（U+202A–202E、U+2066–2069、U+200E/F、U+061C）、ゼロ幅文字（U+200B–200D、U+2060、U+FEFF）、U+2028/2029、タグ文字、UTF-8 でないバイト。plan の表示を偽装できるので（k3s は manifest を cluster-admin で適用する）、何がどこ（行・列、`spec` では JSON pointer）にあるかを示して拒否する。manifest は LF の改行で書き、YAML に必要な文字はエスケープ（`"\u202e"`）で書く |
| MAM-GRAPH-0110 | 同じものへの宣言が矛盾している。同じ node の同じ unit（`nginx` と `nginx.service` は同じ）を、`enabled`・`running` を変えて 2 回宣言した。k3s を宣言した（`NodeK3s`）node で、k3s の service を `running=false` にした。同じ node の 2 つの `NodeAptRepo` が、同じ URI と suite を持つ（apt は 1 つのソースに 2 つの鍵を認めない）。宣言を 1 つにするか、同じにする |

## MAM-GRAPH — 警告（拒否ではない）

宣言として正しいが、意図とは違いそうなもの。plan の、そのリソースの `warnings` に出る。plan の状態も操作も変えない。

| コード | 意味 |
|---|---|
| MAM-GRAPH-0201 | k3s の agent の版が、その server の版より新しい（Kubernetes は API server より新しい kubelet を支えない）。server の版を先に宣言する（版上げは server が先） |
| MAM-GRAPH-0202 | k3s を宣言したのに、その service（`NodeService`、名前は `k3s`）を宣言していない。バイナリは入るが、起動も維持もされず、plan は in sync になる。`Mamela.service(graph, node, "k3s", enabled: true, running: true)` を足す |
| MAM-GRAPH-0203 | `NodeFile` の内容が秘密に見える（PEM・OpenSSH・PGP の秘密鍵、GitHub・Slack のトークン、AWS のアクセスキー）。宣言、plan（PR のコメント）、Control Plane の DB、ノードの revision に平文で残る。秘密は宣言に書かない（秘密の扱いは監査 A4 で後に入る）。`password=` のような語だけでは警告しない |
| MAM-GRAPH-0204 | `NodeFile` の `restart` に、その node で宣言していないサービスがある。動いていれば再起動されるが、名前の誤りかもしれない。`Mamela.service` で宣言するか、名前を確かめる |
| MAM-GRAPH-0205〜0219 | 予約（linux 拡張の警告。NodeUser、NodeAptRepo、NodeArtifact が使う） |

## MAM-POL — ポリシーの判定

| コード | 判定 | 意味 |
|---|---|---|
| MAM-POL-0101 | Auto | `autoStartServices` にあるサービスの起動（ドリフト修復） |
| MAM-POL-0102 | Auto | プロバイダーの変更で、host のカタログが分類した効果がすべて policy の `autoProviderEffects` にある |
| MAM-POL-0103 | Auto | plan に表示したサービスの自動修復設定を apply で有効化・解除する。plan 自体は許可を配らない |
| MAM-POL-0201 | — | 廃止（ADR 0006）。VM の作成（組み込みの VirtualMachine）。今はプロバイダーの変更（MAM-POL-0102、0208、0209） |
| MAM-POL-0202 | — | 廃止（ADR 0006）。VM の CPU / メモリの変更 |
| MAM-POL-0203 | — | 廃止（ADR 0006）。ディスクの拡張 |
| MAM-POL-0204 | ApprovalRequired | パッケージの導入（maintainer script が root で動く） |
| MAM-POL-0205 | ApprovalRequired | サービスの有効化 |
| MAM-POL-0206 | ApprovalRequired | サービスの起動（自動起動が許可されていない） |
| MAM-POL-0207 | ApprovalRequired | サービスの停止・無効化 |
| MAM-POL-0208 | ApprovalRequired | プロバイダーの変更に、host のカタログが知らない呼び出しの要求がある（要求をレビューする） |
| MAM-POL-0210 | ApprovalRequired | k3s（Kubernetes）の導入。GitHub から取得し sha256 で固定したバイナリを、root で動かす |
| MAM-POL-0212 | ApprovalRequired | 動いている k3s の版上げ。バイナリ（sha256 で固定）を置き換え、そのノードの k3s を再起動する。server の再起動中は API が使えず、agent の再起動中はそのノードが Ready でない |
| MAM-POL-0213 | ApprovalRequired | k3s の自動適用ディレクトリへのマニフェストの書き込み。k3s は cluster-admin の権限で適用し、変わるたびにまた適用する。plan に全文が出る |
| MAM-POL-0214 | ApprovalRequired | Kubernetes のノードを、クラスタから消す（Ready なら先に drain する）。宣言に無いノードだけ。削除なので、承認は Control Plane のホストだけが行う（`mamela approve`） |
| MAM-POL-0215 | ApprovalRequired | mamelet 自身を、宣言した sha256 の binary（Control Plane が配る）に置き換えて再起動する。前の binary は残し、新しいものが報告しなければ戻る。ノードは 1 台ずつ |
| MAM-POL-0220 | ApprovalRequired | `linux.write-file`（`NodeFile`）。root としてファイルを、plan のとおりの内容・mode・owner で書き、挙げたサービスを（動いていれば）再起動する。plan に全文が出る |
| MAM-POL-0221〜0222 | — | 予約（linux 拡張の操作。NodeUser） |
| MAM-POL-0223 | ApprovalRequired | `linux.add-apt-repo`（`NodeAptRepo`）。root として APT のソースとその鍵（sha256 で固定）を置き、`apt-get update` を走らせる。そのソースのパッケージは、システムのパッケージを置き換えうる（apt は版の高いものを入れる）し、maintainer script は root で動く。plan にソースのファイルの全文が出る。そのパッケージを入れるには、origin を policy の `allowedAptOrigins` と mamelet の `allowedOrigins` に足す |
| MAM-POL-0224〜0226 | — | 予約（linux 拡張の操作。NodeAptRepo 0224、NodeArtifact 0225〜0226） |
| MAM-POL-0211 | — | 廃止（ADR 0006）。VM の起動。今はプロバイダーの変更（起動は `late` の変更。provider-authoring.md） |
| MAM-POL-0209 | ApprovalRequired | プロバイダーの変更（効果は host のカタログで分類したもの） |
| MAM-POL-0310 | ApprovalRequired | 宣言から消えた、その宣言が作った物の削除（ADR 0013）。承認は Control Plane のホストの `mamela approve` だけ。`mamela api`（GitHub 側）は断る（MAM-API-0201） |
| MAM-POL-0311 | Denied | 削除の要求に、host のカタログが知らない呼び出しがある。分類できない呼び出しでは削除しない |
| MAM-POL-0301 | — | 廃止（ADR 0006）。ディスクの縮小。今はプロバイダーが unsupported とした操作（MAM-POL-0309） |
| MAM-POL-0302 | — | 廃止（ADR 0006）。イメージの変更。今は MAM-POL-0309 |
| MAM-POL-0303 | Denied | Mamela が作っていないリソース（Foreign）の変更。プロバイダーが引き継ぎ（adopt）の明示操作を持つなら、`mamela act` で引き継げる |
| MAM-POL-0304 | Denied | インストールすると他のパッケージが削除される（apt-get -s の変更集合に削除がある） |
| MAM-POL-0305 | Denied | 許可していない APT origin（policy の `allowedAptOrigins`。既定は `Ubuntu:`）からのパッケージ |
| MAM-POL-0306 | Denied | プロバイダーの変更に、host のカタログが「データを失いうる」とする要求がある。データを失う操作（削除など）は、plan ではなくプロバイダーの明示操作（`mamela act`）でだけ行う |
| MAM-POL-0308 | Denied | Control Plane が自分の動くホスト（site の `controlplane.host`）を変える操作（ADR 0003）。`mamela act` も同じ理由で拒む |
| MAM-POL-0309 | Denied | プロバイダーが実行しない変更（マニフェストで unsupported とした操作。ディスクの縮小、イメージやネットワークの変更など）。差分は表示し、理由を示す |
| MAM-POL-0307 | — | 廃止（ADR 0006）。VM のネットワークの変更。今は MAM-POL-0309 |
| MAM-POL-0399 | Denied | ポリシーが知らない操作 |

## MAM-PLAN — 計画の状態

| コード | 意味 |
|---|---|
| MAM-PLAN-0101 | マシンがまだないので待つ |
| MAM-PLAN-0102 | マシンが Foreign なので、その上の設定はしない。プロバイダーが引き継ぎの明示操作を持つなら `mamela act` |
| MAM-PLAN-0103 | Agent からのパッケージの報告を待つ |
| MAM-PLAN-0104 | Agent からのサービスの報告を待つ |
| MAM-PLAN-0105 | mamelet が、インストールで何が変わるか（apt-get -s）を報告するのを待つ |
| MAM-PLAN-0106 | サービスの unit がまだない（それを入れるパッケージや k3s が入っていない）ので待つ |
| MAM-PLAN-0108 | mamelet からの k3s の報告を待つ |
| MAM-PLAN-0109 | k3s の agent が、server のアドレスの報告を待つ |
| MAM-PLAN-0110 | マシンが初回起動に十分な時間（15 分）を超えて動いているのに、mamelet が、宣言したものを何も報告していない（何か 1 つでも報告していれば、新しく宣言したものは次の poll を待つだけで、これにはならない）。起動で止まっている（緊急モードなど）か、Control Plane に届かない。コンソールを確かめ、プロバイダーの明示操作で再起動する（`mamela act`。Proxmox なら `reboot-vm`、応答しなければ `reset-vm`） |
| MAM-PLAN-0111 | マシンが止まっているので、その上のリソースは起動を待つ |
| MAM-PLAN-0112 | k3s の agent のサービスは、server の k3s が動くまで起動しない（agent の起動は server を待ち、mamelet を止めてしまう） |
| MAM-PLAN-0113 | プロバイダーのリソースが、`after` の依存先を待つ（ADR 0007）。依存先がまだない、Mamela のものと確かめられない（Foreign）、または作成が途中で止まっている（もう一度 apply すると終わる）。プロバイダーには、このリソースの diff を求めない。依存先を作る plan を apply してから、もう一度 plan する。依存先が Foreign のままなら待ち続けるので、依存先の理由（MAM-POL-0303 など）を見る |
| MAM-PLAN-0114 | プロバイダーのリソースが存在し、引き継ぐ対象（`adoptable`）である。`mamela import terraform` で取り込んだもの、またはプロバイダーが以前 Mamela の作ったものと認めたもの。人が引き継ぐまで、Mamela は何も変えない（ADR 0009） | プロバイダーの引き継ぎ操作を `mamela act --confirm <name> <adopt-op> "<ID>" <pome-dir>` で行う。そのあと plan すると、宣言との差分が出る |
| MAM-PLAN-0115 | k3s の版上げは、全 Kubernetes ノードが Ready のあいだだけ進む。Ready でないノードがある、または server がノードを報告していない（server の k3s を調べる） |
| MAM-PLAN-0116 | k3s の版上げの順番待ち。server が先、agent は名前の順に 1 台ずつ（agent は server より新しい版にしない） |
| MAM-PLAN-0117 | mamelet からの k3s のマニフェストの報告を待つ |
| MAM-PLAN-0118 | k3s のノードのマシンを再起動する変更（仕様の変更での再起動など）は、クラスタのマシンを 1 台ずつ、agent が先（agent は名前の順）、server が最後に行う。先のマシンの再起動が済むまで待つ。全 Kubernetes ノードが Ready でなければ MAM-PLAN-0115 |
| MAM-PLAN-0119 | ノードの mamelet が、どの版で動いているかを報告していない。更新の仕組みより前の mamelet は報告しない。一度だけ手で置き換える（[mamelet の更新](mamelet-update.md)） |
| MAM-PLAN-0120 | mamelet の更新は 1 台ずつ、名前の順。前のノードが宣言した版で動くまで待つ。前のノードの更新が失敗したなら、それが先に直す |
| MAM-PLAN-0122 | ノードの mamelet が、その拡張（`linux` など、汎用の通信の形の拡張）を受け取らない（Blocked）。報告の `extensions` に拡張がない、または古い版だけ（mamelet が拡張より古い。`linux` は 0.10.0 から）か、revision の拡張の部分を断った（報告の `extRefused`。理由が出る）。Control Plane は、受け取らないノードの revision に拡張の部分を入れない（古い mamelet が revision 全体を断らないように）ので、承認しても何も起きない。先に mamelet を更新する（[mamelet の更新](mamelet-update.md)。Control Plane が先、mamelet があと） |
| MAM-PLAN-0140 | mamelet からのファイル（`NodeFile`）の報告を待つ。宣言を変えた直後に、所有者・グループがあるかの報告を待つときもこれ |
| MAM-PLAN-0141 | `NodeFile` の owner または group が、そのノードにない（Blocked）。それを作るパッケージを入れるか、ある owner（root など）にする。NodeUser が入ると、宣言で作るユーザーは MAM-PLAN-0143 の待ちになる |
| MAM-PLAN-0142 | サービスの起動が、そのサービスを `restart` に挙げた `NodeFile` が書かれるのを待つ（設定を置いてから起動する） |
| MAM-PLAN-0143 | `NodeFile` の owner または group を、同じ宣言の別のリソース（NodeUser）が作るので、それを待つ（NodeUser とともに使われる） |
| MAM-PLAN-0144〜0149 | 予約（linux 拡張。NodeFile 0144、NodeUser 0145〜0149） |
| MAM-PLAN-0150 | mamelet からの APT のソース（`NodeAptRepo`）の報告を待つ |
| MAM-PLAN-0151 | パッケージ（`NodePackage`）の導入が、同じノードの `NodeAptRepo` が in sync になる（ファイルが揃い、apt がそれを読む）のを待つ。パッケージがそのソースから来るかもしれないため。ソースを足す plan を apply すると、次の plan が導入を示す |
| MAM-PLAN-0152 | `NodeAptRepo` のファイルは揃っているが、apt がまだそのソースを読んでいない（`/var/lib/apt/lists` にリリースファイルがない）。apt-get update が失敗したか、まだ走っていない。承認した plan をもう一度 apply するか、この plan を承認すると、update をやり直す。失敗の理由は前の試みの結果にある |
| MAM-PLAN-0153 | ノードのほかのソースのファイルが、`NodeAptRepo` と同じ URI と suite を持つ（Blocked）。apt は 1 つのソースに 2 つの鍵（`Signed-By`）を認めず、すべてのソースを読まなくなる。そのファイルを手で消すか、宣言しない |
| MAM-PLAN-0154〜0159 | 予約（linux 拡張。NodeAptRepo 0154、NodeArtifact 0155〜0159） |
| MAM-PLAN-0121 | k3s のバイナリは宣言した sha256 のもので、動いてもいるのに、Kubernetes のノードが報告する版が、宣言の `version` と違う（kubelet が `v1.37.2` と、k3s の revision なしで報告しても、`v1.37.2+k3s1` とは同じ版として扱う）。再起動した直後で、ノードがまだ新しい版を報告していないだけなら、少し待って plan し直すと消える。消えないなら、宣言の version と sha256 が別のリリースのもの（version だけ上げて sha256 を忘れた、など）。version のバイナリの sha256（リリースの `sha256sum-<arch>.txt`）に直す。報告が古い、k3s が止まっている、ノードの名前が宣言と違うときは、判断しない |
| MAM-PLAN-0130 | ノードの mamelet が、Control Plane の revision を拒否している（Blocked）。理由は mamelet の報告のとおり。未知のメンバー（mamelet が Control Plane より古い。[mamelet の更新](mamelet-update.md)の限界）、serial の巻き戻し（Control Plane の DB を古い写しから戻した。最新の DB に戻す）、時計のずれ（ノードの時計を直す）、署名や宛先の不一致（ノードの設定）、revision の内容が不正（宣言の名前や値）。許可は revision で届くので、拒否されているあいだは、承認しても何も届かない。同じノードで同期している（in sync）ものは、そのまま。Control Plane のログにも、拒否の始まりと終わりが出る |
| MAM-PLAN-0107 | プロバイダーがそのリソースを観測していない（Blocked。宣言の Pome で plan すると観測する） |
| MAM-PLAN-0201 | 観測が古いので判断しない（Blocked） |
| MAM-PLAN-0202 | mamelet がパッケージを観測・シミュレーションできない（Blocked。例: apt source にない） |
| MAM-PLAN-0204 | mamelet が k3s を観測できない（Blocked。例: unit が手で変えられた） |
| MAM-PLAN-0210 | mamelet は、その path にファイルを書かない（Blocked）。path がシンボリックリンク・ディレクトリ・デバイス、または上のディレクトリがリンクかディレクトリでない。Mamela はリンクをたどらない。本当の path を宣言するか、手で置き換える |
| MAM-PLAN-0211 | mamelet がファイルを観測できない、またはその報告を Control Plane が読めない（Blocked。mamelet が path を断ったときもこれ） |
| MAM-PLAN-0212〜0213 | 予約（linux 拡張の観測の問題。NodeUser、NodeArtifact） |
| MAM-PLAN-0214 | mamelet は、`NodeAptRepo` のソースか鍵の path に書かない（Blocked）。path かその上のディレクトリがシンボリックリンク、または path がディレクトリ。`mamela-*` のソースと鍵は Mamela のもので、手で置き換える |
| MAM-PLAN-0215 | mamelet が `NodeAptRepo` を観測できない、またはその報告を Control Plane が読めない（Blocked。mamelet がソースの値を断ったときもこれ） |
| MAM-PLAN-0216〜0219 | 予約（linux 拡張の観測の問題。NodeUser、NodeArtifact） |
| MAM-PLAN-0203 | プロバイダーの変更が、別の宣言に対して計算されたもの（Blocked。観測し直す） |
| MAM-PLAN-0301 | 現在の宣言にないため保持する（理由を記録していない既定。0302〜0306 が詳しい） |
| MAM-PLAN-0302 | 現在の宣言にないが、別の宣言（stack）が作った物なので保持する（ADR 0013）。作った宣言から消す |
| MAM-PLAN-0303 | 現在の宣言にないが、どの宣言が作ったかを記録する前に作った物なので保持する。`mamela claim <dir> --confirm` でその宣言の物にするか、プロバイダーの明示操作（`mamela act`）で消す |
| MAM-PLAN-0304 | 現在の宣言にないが、プロバイダーにその種類を削除する操作（`removes`）がないので保持する |
| MAM-PLAN-0305 | 現在の宣言にないが、policy の `removal = "retain"` なので保持する |
| MAM-PLAN-0306 | 現在の宣言にないが、プロバイダーが削除を用意できなかったので保持する |
| MAM-PLAN-0310 | 宣言から外れた `NodeFile`。ファイルはノードにそのまま残る（Mamela はまだファイルを消さない）。plan を apply すると、mamelet はそれを観測しなくなる。消すなら手で |
| MAM-PLAN-0311 | 予約（linux 拡張の、宣言から外れたものの保持） |
| MAM-PLAN-0312 | 宣言から外れた `NodeAptRepo`。ソースと鍵はノードにそのまま残り、apt はそれを読み続ける（Mamela はまだソースを消さない）。plan を apply すると、mamelet はそれを観測しなくなる。消すなら手で（2 つのファイルを消して apt-get update） |
| MAM-PLAN-0313〜0319 | 予約（linux 拡張の、宣言から外れたものの保持） |
| MAM-PLAN-0307 | 現在の宣言にあるが、Mamela が別の宣言（stack）のために作った物なので、何も変えない（Blocked。その上のパッケージ・サービス・k3s は待つ）。同じ名前のリソースが 2 つの宣言にある。宣言を 1 か所にする（作った宣言から消すと、プロバイダーがその kind を削除できるなら、それは削除の plan になる）か、こちらの名前を変える |
| MAM-PLAN-0401 | 警告。動いている k3s の版から 7 マイナー版以上戻す宣言（`v1.37.2` から `v1.30.0` など）。Kubernetes は版を戻すことを支えず、新しい版が保存したものを古い版が読めないことがある。打ち間違いなら version と sha256 を直す。本当に戻すなら、その版のときに取ったスナップショットから復旧する（Mamela はクラスタを巻き戻さない）。plan の状態も操作も変えない |

## MAM-REC — 実行

| コード | 意味 | 対処 |
|---|---|---|
| MAM-REC-0001 | 保存された Plan が入力から再計算した結果と一致しない | 適用しない。新しく plan する |
| MAM-REC-0002 | 実行直前の観測で、リソースが Plan の前提と違っていた | 新しく plan してレビューする |
| MAM-REC-0003 | 操作の結果が不明 | もう一度 apply する（観測してから確認か再試行をする） |
| MAM-REC-0004 | 操作が失敗した | 原因を直して apply する（失敗した操作は再試行される） |
| MAM-REC-0005 | 別の worker がこの操作を実行中 | 終わるのを待つ |
| MAM-REC-0006 | 操作の一部が効いたあとで失敗した（部分的な成功） | 原因を直して apply する（観測してから、残りを続ける） |
| MAM-API-0201 | `mamela api` が、物の削除を含む plan の承認を断った（GitHub の側から削除は承認できない）。HTTP 403 | Control Plane のホストで `mamela approve <digest>` を実行する |
| MAM-REC-0007 | plan を作ったときとプロバイダーの契約が違う（マニフェスト、外部のプロバイダーのプログラム、API の接続先・資格情報・カタログ）。承認はその契約に対するもの。このとき apply は guard も呼ばない | 新しく plan して、レビューと承認をやり直す |
| MAM-REC-0008 | 削除の直前に世界を観測し直せなかったので、何も消さずに止まった。それより前の操作の結果は残る | プロバイダーが答えるようになってから、もう一度 apply する（観測してから、承認どおりのものだけを消す） |
| MAM-REC-0009 | この DB に対する別の apply が動いている（CLI の `mamela apply` と `mamela api` など）。apply は 1 つの DB につき 1 つずつ、前の apply のあとの世界を観測して行う。何も実行していない | 終わるのを待って、もう一度 apply する |
| MAM-REC-0010 | plan を作ったときの policy が、いま有効な policy と違う（policy のファイルを変えた、保護するノードを足した、plan のときと違う `--policy` を付けた）。承認はそのときの policy の下での判断なので、何も実行していない | 新しく plan して、レビューと承認をやり直す。plan に `--policy` を付けたなら、apply にも同じファイルを付ける |
| MAM-REC-0011 | apply の最中に、承認が取り消された（`mamela revoke`）。まだ始めていない、承認が要る操作は実行していない。済んだ操作は戻らない | まだ要るなら承認し直す（`mamela approve`）。そうでなければ新しく plan する |
| MAM-REC-0101 | 適用後の再観測でも宣言全体は未収束 | 次の観測を待って再計画する。次の変更にはその計画のポリシー判定・承認が必要 |
| MAM-REC-0102 | 適用後の再観測に失敗した。実行した操作の結果は記録済みで、収束したかは分からない | 新しく plan して確かめる |

## MAM-APR — 承認

| コード | 意味 | 対処 |
|---|---|---|
| MAM-APR-0001 | `mamela revoke` で取り消す承認がない（承認していない、期限が切れた、すでに取り消した）。何も変えていない | 何もしなくてよい。承認が要るなら `mamela approve` |
| MAM-APR-0002 | その plan はすでに承認されている。承認の記録は書き換えない（承認者も期限もそのまま）ので、この要求は何も変えなかった。要求は event に残る。終了コードは 0 | 承認者や期限を変えたいなら、`mamela revoke` してから承認し直す |

## MAM-DB — 状態の DB（SQLite）

| コード | 意味 | 対処 |
|---|---|---|
| MAM-DB-0001 | DB のスキーマが、この Mamela が知っているより新しい（新しい版の Mamela が上げた）。開かずに止まる | DB を上げた版か、それより新しい Mamela を使う。この版に戻すなら、すべての Mamela のプログラムを止め、DB（と `-wal`・`-shm`）を別の場所に置き、上げる前に自動で作られた `<db>.pre-migrate-<この版のスキーマ>` を DB の場所に戻す。退避のあとに書いた状態は戻らない |
| MAM-DB-0002 | migration の前に DB の退避を作れなかったので、DB を変えずに止まった | DB のあるディレクトリを書き込めるようにする（退避は DB の隣に `<db>.pre-migrate-<旧版>` として作る。権限 0600）。空き容量も確かめる |

## MAM-PRV — 外部のプロバイダー

| コード | 意味 | 対処 |
|---|---|---|
| MAM-PRV-0001 | プロバイダーのプログラムとして実行できない。または、許していない import がある（実行していない） | ファイル・ソケット・HTTP を開くコードを除く（例: `net/http` や `os.ReadFile` を使うパッケージを import しない）。API への要求は `host/request` で送る |
| MAM-PRV-0002 | ネイティブのプロバイダーで、その実行ファイル・source・版を信頼する記録がない | WASM 版を使う。作者とその build を信頼できるときだけ `mamela provider trust` |
| MAM-PRV-0003 | プロバイダーがプロトコルを破った。またはマニフェストが不正 | プロバイダーの不具合。stdout にはプロトコルのメッセージだけを書く（診断は stderr） |
| MAM-PRV-0004 | プロバイダーが、許されていない要求を求めた（送っていない）。observe 中の変更、diff 中の要求、許可にない呼び出し・パラメータ・対象・値・前提条件、期限切れ | プロバイダーの不具合か、宣言と許可の不一致。マニフェストに呼び出しとパラメータを宣言し、承認した対象と値だけを送る |
| MAM-PRV-0005 | 許可が変更と一致しない、期限が切れた、または使用済み | 新しく plan して承認する |
| MAM-PRV-0006 | プロバイダーのプログラムが止まった、または時間内に答えなかった | stderr の詳細を見る |
| MAM-PRV-0007 | プロバイダーが、頼まれたことをできなかったと答えた | メッセージに従う |
| MAM-PRV-0008 | プロバイダーの変更が、運用者の選んだカタログが許さない値を送る（カタログの `values`。たとえば IAM の path・信頼ポリシー・付けるポリシー）。plan の時点で止め、apply でも送る前に確かめる | プロバイダーの不具合か、宣言が許されない値を求めている。カタログを広げるのは、プロバイダーの作者がカタログを直し、運用者がそれを読んで（`mamela provider catalog`）選び直すときだけ |
| MAM-PRV-0009 | 宣言のツリーの Pome のプログラム（`program_sha256`）またはカタログ（`catalog_sha256`）が、運用者が `mamela.providers.toml` で固定したものと違う。何も起動せず、API に何も送らない。plan と apply の両方で確かめる（ADR 0015） | 宣言の Pome が変わった（版上げ、`[replace]`、`Pome.lock`）。意図した版上げなら、新しいプログラムとカタログを読み（`mamela provider catalog`、`mamela provider inspect`）、`mamela provider pin <宣言のディレクトリ> <provider>` が出す値を設定に書く。意図していなければ、宣言のツリーを誰が変えたかを調べる |
| MAM-PRV-0010 | プロバイダーが固定されていない（`program_sha256`、`pome:` のカタログの `catalog_sha256` がない）。警告（`mamela plan` が stderr に出す。従来どおり動く）。`strict_pins = true` のときは、設定の読み込みで拒否する | 宣言のツリーを書ける人が、運用者の資格情報で動くプログラムとカタログを選べる状態。`mamela provider pin` が出す値を設定に書き、すべて固定したら `strict_pins = true` にする |
| MAM-PRV-0011 | カタログが、データを変えうる呼び出しを `read` としている（`PUT`・`PATCH`・`DELETE`、または `fixed` のない `POST`）。そのカタログは読み込まない。どのカタログのどの項目かをメッセージが言う | カタログの作者が、その呼び出しの効果を `change`・`restart`・`lose-data` に直す。RPC 型の API の読み取りなら、呼び出しを区別する `fixed`（`Action=...`、`X-Amz-Target`）を書く |
| MAM-PRV-0012 | provider の API への要求が失敗した（拒否された、名前が引けない、時間切れ、TLS の証明書、固定した指紋と違う、切られた）。メッセージは種類だけを言い、接続先（URL、host、port、path）は出さない（plan のエラーは PR コメントに載る）。接続先は Control Plane のログにある | API が動いているか、`api_url`・`tls_fingerprint` が合っているかを確かめる。メッセージの直し方に従う |

## MAM-SITE — site の規則

site（`mamela.site.toml`、`mamela-site/v1`）の規則に合わない変更。どのプロバイダーのマシンにも効く。

| コード | 意味 | 対処 |
|---|---|---|
| MAM-SITE-0304 | 作るマシンのネットワーク（seed の `network`）を site が許さない。範囲外の固定アドレス、許されていない DHCP。plan の時点で止め（何も保存しない）、apply でも seed を作る前にもう一度確かめる | site の `network.static_first`–`static_last` の中のアドレスを宣言する。`network.dhcp = true` なら `Dhcp` でもよい |
| MAM-SITE-0305 | プロバイダーの変更が、site の 1 台あたりの予算（vCPU、メモリ、ディスク）を超える。どのパラメータが大きさを決めるかは、運用者の選んだカタログ（`measures`）が決める。plan の時点で止め、apply でも送る前に確かめる | 宣言の大きさを予算の中にする。予算（`[budget]`）を広げるなら、理由を書いて人がレビューする |

以前の LAB-0304・LAB-0305 と同じ番号にしてある。

## MAM-GIT — mamelacd（GitHub との同期、ADR 0012）

mamelacd（`internal/gitops`）が、GitHub や自分の判断について出すコード。commit status・ログ・PR やコミットのコメント・`mamelacd status` に出る。

| コード | 意味 | 対処 |
|---|---|---|
| MAM-GIT-0001 | GitHub がトークンを断った（401）。失効したか、このリポジトリへの権限がない | 新しいトークンを作り（Contents: read、Pull requests: read and write、Commit statuses: read and write）、`token_file` を書き換えて、`systemctl kill -s HUP mamelacd`（または再起動）。401 のときは、ファイルが変わっていれば自動で読み直す（docs/gitops.md） |
| MAM-GIT-0002 | GitHub が、このトークンにその操作を許さない（403 で、レート制限ではない）。コミットへのコメントには Contents: write が要り、ないときは 1 度だけログに出して、コメントを付けない | トークンの権限とリポジトリを確かめる。コミットへのコメントは任意で、権限を足さなくてもよい（理由は status とログにある） |
| MAM-GIT-0003 | GitHub のレート制限（403 または 429、Retry-After、X-RateLimit-Reset）。その時刻まで何も送らず、poll の間隔が延びる | 待つ。何も失われない。繰り返すなら、PR の数と `active_interval` を見直す |
| MAM-GIT-0004 | GitHub が 5xx を返した | GitHub 側の問題。次の poll でやり直す |
| MAM-GIT-0101 | 同じ変更が戻ってくる（apply が stale になり続ける、または apply したのに同じ plan がまた出る）回数が `retry_max + 1` に達したので、`sync = "push"` が自動では承認しない（head の状態 `stopped`） | 世界を変えているものか、変更が観測に出ない理由を調べる（`mamela plan`、Control Plane のホストの記録）。新しいコミットを push するか、ホストで `mamela approve <digest>`（次の poll で apply される） |
| MAM-GIT-0201 | 評価器が一時的に使えない（ツールチェーンの差し替え中 MAM-EVAL-0001、ビルドの時間切れ MAM-EVAL-0102、ディスクが満杯）ので、宣言のせいにせず、`retry` の間隔で評価し直す（head の状態 `eval-retry`） | 評価器の側（ホストのツールチェーン、ディスク）を直す。直れば自動で進む |
| MAM-GIT-0301 | 設定の login で動いたのが、固定した GitHub アカウント（数値 ID）と違う。改名で空いた login を別の人が取った、またはアカウントを作り直した。そのアカウントの PR・承認・push は信頼しない | 本人でなければ何もしなくてよい（信頼されていない）。本人のアカウントが変わったなら `mamelacd repin --config <toml> --login <login>` |
| MAM-GIT-0302 | 設定の authors・approvers の login が GitHub にない。何についても信頼されない | 設定の login を直す |
| MAM-GIT-0501 | `mamelacd status` が、最後の poll の終わりが `interval` の 3 倍より古い（レート制限の待ちを除く）ことを見つけた、または heartbeat がない。止まっている疑い | `systemctl status mamelacd`、`journalctl -u mamelacd -n 100`。API（`mamela api`）が答えているか確かめる |

## MAM-ACT — 明示操作（`mamela act`）

プロバイダーの明示操作（削除、再起動、引き継ぎ。ADR 0006）。plan には入らず、人が名前を打ち直したときだけ行う。

| コード | 意味 | 対処 |
|---|---|---|
| MAM-ACT-0100 | その名前のプロバイダーのリソースがない（宣言にも、Mamela が作って残っているものにもない）。または、その名前が複数のリソースを指す | 名前を確かめる。複数あれば ID（`"ProxmoxVM web"` のような `<kind> <name>`）で指す |
| MAM-ACT-0101 | その操作は、そのプロバイダーのその kind の明示操作ではない | メッセージに出る、その kind の明示操作から選ぶ |
| MAM-ACT-0102 | 対象が存在しない | 何もしなくてよい |
| MAM-ACT-0103 | 引き継ぎの対象が、もう Mamela のもの | 何もしなくてよい |
| MAM-ACT-0104 | プロバイダーが、対象を引き継げる（`adoptable`）と報告していない。Mamela は、プロバイダーが Mamela の作ったものと認めたものか、人が取り込んだもの（`mamela import terraform`）しか引き継がない | 対象が本当に Mamela の作ったものか確かめる。Terraform の管理下にあるなら `mamela import terraform` で取り込む。そうでなければ Foreign のまま |
| MAM-ACT-0105 | Mamela に、対象を作った記録がない（Foreign、または作成が終わっていない）。Mamela は自分のものにしか明示操作をしない | 作成が途中なら、plan・apply で仕上げる |
| MAM-ACT-0106 | `--confirm` の値が対象の名前と違う。何も変えていない | 名前を正しく打ち直す |
| MAM-ACT-0201 | 操作の結果や完了後の状態を確かめられなかった | 表示された操作 ID を `mamela act --recover <operation-id>` に渡して再観測する。変更要求は再送しない |
| MAM-ACT-0202 | 操作が失敗した（または一部だけ効いた） | メッセージの原因を直してから、もう一度実行する |
| MAM-ACT-0204 | 復旧対象の操作・資源・プロバイダー契約が一致しない、または旧形式の記録に復旧用の identity がない | 元の契約を使う。旧形式は実物と監査記録を確認する |
| MAM-ACT-0205 | 明示操作の途中で所有記録やノードの登録が変わったため、新しい登録を保持した | 同名資源の再作成・再登録を確認する。古い操作で新しい登録を消さない |

## MAM-PKI — 証明書と鍵（mamelet の登録、Control Plane の identity）

CP と mamelet の mTLS の証明書、CP の CA と署名鍵。手順は [recovery.md](recovery.md)。

| コード | 意味 | 対処 |
|---|---|---|
| MAM-PKI-0101 | mamelet の証明書が 30 日以内に切れる（更新されていない）。CP のログ（そのノードが来たとき、6 時間に 1 回、起動時と毎日）、`mamela plan` の警告、`mamela controlplane agents` | 0.3.2 以降の mamelet は、0.3.2 以降の CP から自分で更新する。mamelet を更新するか（[mamelet-update.md](mamelet-update.md)）、切れる前後に `mamela controlplane reenroll --node <名前>` |
| MAM-PKI-0102 | mamelet の証明書が切れた。CP は握手で断る（CP のログ。TLS の失敗に足す）。ノードの mamelet のログにも出る | 再登録する（`reenroll`、recovery.md の 6.3） |
| MAM-PKI-0103 | CP のサーバー証明書が 30 日以内に切れるので、動いたまま出し直した（情報）。出し直せないときは、そのエラーと、鍵のディレクトリへの書き込み権限の確認 | 何もしない。出し直せないときは `controlplane.state_dir` の権限 |
| MAM-PKI-0104 | mamelet が証明書を更新できない（ノードのログ）。CP が古くて要求を知らない（404）か、CP が断った | CP を更新する（先に CP、次に mamelet）。CP のログに理由がある。1 時間ごとに再試行する |
| MAM-PKI-0105 | 更新の要求を CP が断った。証明書の残りがまだ寿命の半分より多い（409） | 何もしない（mamelet は残り 1/3 になってから頼む） |
| MAM-PKI-0106 | CP の CA が 1 年以内に切れる（毎日のログ）。mamelet は CA を 1 つだけ固定していて、重ねて信頼できない | recovery.md の 6.4（全ノードの再登録）を、時間を決めて行う |
| MAM-PKI-0201 | **起動を拒否した。** DB に登録済みの mamelet があるのに、CA か revision の署名鍵のファイルがない（`serve`・`apply`・`api`・`backup`）。新しく作ると、固定している全 mamelet が止まる | 鍵を戻す（`mamela restore`、またはバックアップの `cp/`）。本当に失ったなら `mamela controlplane new-keys --confirm`（DB は残る。全ノードの再登録） |
| MAM-PKI-0203 | 鍵はあるが DB が新しい（なかったので、起動時に作った）。登録済みの mamelet がなく、全 mamelet が断られる（`serve` の起動時のログ） | DB を戻す（`mamela restore`）。なければ、ノードごとに `reenroll --node <名前> --vmid <vmid>` |
| MAM-PKI-0301 | `reenroll` の対象のノードが DB にない、または VMID が登録と違う | `mamela controlplane agents` で名前を確かめる。DB を失ったノードは `--vmid` を付ける |
| MAM-PKI-0302 | `new-keys` を断った。鍵のディレクトリに、CA か署名鍵のファイルが一部ある | 残すなら戻す。替えるなら、古い CA・署名鍵のファイルを自分で移してから |

## MAM-BAK — バックアップと復元

`mamela backup`、`mamela restore`、古い DB の検出。手順は [recovery.md](recovery.md)。

| コード | 意味 | 対処 |
|---|---|---|
| MAM-BAK-0001 | `backup` の出力先がすでにある（上書きしない） | 新しい名前にする |
| MAM-BAK-0002 | バックアップできない（DB がない、鍵がまだない）、または、バックアップの archive が復元できない（欠け、書き換え、知らないファイル、manifest と違う sha256） | archive は `mamela backup` が書いたままのものを使う。転送で壊れたなら、取り直す・前のものを使う |
| MAM-BAK-0003 | `restore` の先に、DB か鍵がすでにある。何も変えていない | 何もないホストで戻す。または自分で移す。`--replace` なら `名前.before-restore-<時刻>` に移して残す |
| MAM-BAK-0004 | バックアップの DB を、この Mamela は開けない（新しい版の schema など） | バックアップを作った版以上の Mamela で戻す |
| MAM-BAK-0101 | **DB が mamelet より古い。** ノードが受け入れた revision の serial が、この DB のものより大きい（古いバックアップから戻した）。mamelet は小さい serial の revision を断る（CP のログ。6 時間に 1 回） | より新しいバックアップを戻す。なければ、そのノードの `state.json` の `lastSerial` を 0 にする（recovery.md の 5） |

## MAM-NODE — ノードの identity と配信

| コード | 意味 | 対処 |
|---|---|---|
| MAM-NODE-0101 | 同名ノードのマシン ID が変わった | 旧登録の失効を確認し、現ノードで再計画する |
| MAM-NODE-0102 | `controlplane.move_to` があるが、plan は接続先を変えない | 新しい CP の準備後、`mamela controlplane redirect` を実行する |
| MAM-NODE-0103 | 宣言が固定した mamelet の sha256 を、この Control Plane は配っていない（`controlplane.mamelet_binary` の sha256 が違う）。plan の警告で、digest には入らない | その binary を `controlplane.mamelet_binary` に置いて `mamela serve` を再起動するか、配っている版を宣言に書く |
| MAM-NODE-0201 | CP が、ノードの `wait` の要求を断った。同じノードの待ちが 2 つあるところに、新しい要求が来て、古いほうを閉じた（409）。mamelet は通常の間隔に戻るだけ | 何もしない。繰り返すなら、同じ名前の mamelet が 2 つ動いていないか |
| MAM-NODE-0202 | CP が、登録・更新・報告の要求を断った。本文が大きすぎる（413。登録と更新は 16 KiB、報告は 1 MiB）、または 10 秒以内に届かなかった（408） | mamelet 以外が送っていないか。ネットワークの遅延 |

| MAM-NODE-0104 | 拡張の操作の許可を、その拡張を受け取らない mamelet のノードに出さなかった（plan のあとに mamelet が古いものに戻ったなど）。plan は MAM-PLAN-0122 で止める | mamelet を更新して、もう一度 plan する |
| MAM-NODE-0120 | 承認が切れたので、ノードに許可を出さなかった。apply が長く（イメージの取り込みなどで）、承認の期限を過ぎてからノードの操作に着いた | 承認し直して、もう一度 apply する |

`mamela act` は、ほかに MAM-POL-0308（Control Plane のホスト）と、guard の block（何も送らない）で止まる。
