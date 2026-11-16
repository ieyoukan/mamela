# 理由コード

Mamela が止まったり拒否したりしたときに表示するコード。一度決めたコードを別の意味に使い回さない。正は `internal/graph/problem.go`、`internal/evaluate/errors.go`、`internal/policy/policy.go`、`internal/plan/plan.go`、`internal/reconcile/reconcile.go`、`internal/provider/host/errors.go`、`internal/provider/host/upstream.go`（MAM-PRV-0012）、`provider/protocol/catalog.go`（MAM-PRV-0011）、`internal/provider/external/pin.go`（MAM-PRV-0009、0010）、`cmd/mamela/service.go` と `internal/provider/external/runtime.go`（MAM-SITE）、`cmd/mamela/act.go`（MAM-ACT）。

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
| MAM-GRAPH-0007 | 値の型が違う（整数でない数値など） |
| MAM-GRAPH-0008 | 同じメンバーが 2 回ある |
| MAM-GRAPH-0009 | 文書の後にデータが続いている |
| MAM-GRAPH-0010 | リソースが多すぎる |
| MAM-GRAPH-0011 | プロバイダーのリソース（`provider` を持つリソース）が `mamela-graph/v1` の文書にある（v2 が要る） |

## MAM-GRAPH — Graph の意味

| コード | 意味 |
|---|---|
| MAM-GRAPH-0101 | 名前が正しくない（プロバイダーのリソースの kind・provider・name の形） |
| MAM-GRAPH-0102 | 同じリソースが 2 回宣言されている |
| MAM-GRAPH-0103 | 参照している node が Graph にない（プロバイダーのマシンとして宣言されていない）、同じ名前のリソースが 2 つある。k3s の agent の server が、k3s の server として宣言されていない。`after` の参照（`"Kind name"`）が、同じ Graph のプロバイダーのリソースではない（宣言していない、組み込みの kind のリソース、書き間違い） |
| MAM-GRAPH-0104 | 名前が `<node>/<package>` などの形と合わない |
| MAM-GRAPH-0105 | 廃止（ADR 0006）。値が範囲外（組み込みの VirtualMachine の vCPU・メモリ・ディスク）。マシンの大きさの上限は site の予算（MAM-SITE-0305）が決める |
| MAM-GRAPH-0106 | 値が正しくない（パッケージ名、サービス名、`Present` 以外の状態、k3s の版・sha256・役割、マシンの `spec.network`） |
| MAM-GRAPH-0107 | 同じ固定アドレスが複数のマシンに宣言されている（マシンの `spec.network.address`） |
| MAM-GRAPH-0108 | `after` の依存が循環している（自分自身を待つものも含む。ADR 0007）。循環に入るリソースごとに報告する。循環の中の依存を 1 つ外す |

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
| MAM-PLAN-0118 | k3s のノードのマシンを再起動する変更（仕様の変更での再起動など）は、クラスタのマシンを 1 台ずつ、名前の順（agent が server より先）に行う。先のマシンの再起動が済むまで待つ。全 Kubernetes ノードが Ready でなければ MAM-PLAN-0115 |
| MAM-PLAN-0119 | ノードの mamelet が、どの版で動いているかを報告していない。更新の仕組みより前の mamelet は報告しない。一度だけ手で置き換える（[mamelet の更新](mamelet-update.md)） |
| MAM-PLAN-0120 | mamelet の更新は 1 台ずつ、名前の順。前のノードが宣言した版で動くまで待つ。前のノードの更新が失敗したなら、それが先に直す |
| MAM-PLAN-0107 | プロバイダーがそのリソースを観測していない（Blocked。宣言の Pome で plan すると観測する） |
| MAM-PLAN-0201 | 観測が古いので判断しない（Blocked） |
| MAM-PLAN-0202 | mamelet がパッケージを観測・シミュレーションできない（Blocked。例: apt source にない） |
| MAM-PLAN-0204 | mamelet が k3s を観測できない（Blocked。例: unit が手で変えられた） |
| MAM-PLAN-0203 | プロバイダーの変更が、別の宣言に対して計算されたもの（Blocked。観測し直す） |
| MAM-PLAN-0301 | 現在の宣言にないため保持する（理由を記録していない既定。0302〜0306 が詳しい） |
| MAM-PLAN-0302 | 現在の宣言にないが、別の宣言（stack）が作った物なので保持する（ADR 0013）。作った宣言から消す |
| MAM-PLAN-0303 | 現在の宣言にないが、どの宣言が作ったかを記録する前に作った物なので保持する。`mamela claim <dir> --confirm` でその宣言の物にするか、プロバイダーの明示操作（`mamela act`）で消す |
| MAM-PLAN-0304 | 現在の宣言にないが、プロバイダーにその種類を削除する操作（`removes`）がないので保持する |
| MAM-PLAN-0305 | 現在の宣言にないが、policy の `removal = "retain"` なので保持する |
| MAM-PLAN-0306 | 現在の宣言にないが、プロバイダーが削除を用意できなかったので保持する |

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
| MAM-REC-0101 | 適用後の再観測でも宣言全体は未収束 | 次の観測を待って再計画する。次の変更にはその計画のポリシー判定・承認が必要 |
| MAM-REC-0102 | 適用後の再観測に失敗した。実行した操作の結果は記録済みで、収束したかは分からない | 新しく plan して確かめる |

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

## MAM-NODE — ノードの identity と配信

| コード | 意味 | 対処 |
|---|---|---|
| MAM-NODE-0101 | 同名ノードのマシン ID が変わった | 旧登録の失効を確認し、現ノードで再計画する |
| MAM-NODE-0102 | `controlplane.move_to` があるが、plan は接続先を変えない | 新しい CP の準備後、`mamela controlplane redirect` を実行する |
| MAM-NODE-0103 | 宣言が固定した mamelet の sha256 を、この Control Plane は配っていない（`controlplane.mamelet_binary` の sha256 が違う）。plan の警告で、digest には入らない | その binary を `controlplane.mamelet_binary` に置いて `mamela serve` を再起動するか、配っている版を宣言に書く |

`mamela act` は、ほかに MAM-POL-0308（Control Plane のホスト）と、guard の block（何も送らない）で止まる。
