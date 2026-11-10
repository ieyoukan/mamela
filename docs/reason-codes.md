# 理由コード

Mamela が止まったり拒否したりしたときに表示するコード。一度決めたコードを別の意味に使い回さない。正は `internal/graph/problem.go`、`internal/evaluate/errors.go`、`internal/policy/policy.go`、`internal/plan/plan.go`、`internal/reconcile/reconcile.go`、`internal/provider/host/errors.go`。

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
| MAM-GRAPH-0003 | 定義されていないメンバーがある |
| MAM-GRAPH-0004 | 必要なメンバーがない、または `null` |
| MAM-GRAPH-0005 | 知らない kind |
| MAM-GRAPH-0006 | 文書が大きすぎる |
| MAM-GRAPH-0007 | 値の型が違う（整数でない数値など） |
| MAM-GRAPH-0008 | 同じメンバーが 2 回ある |
| MAM-GRAPH-0009 | 文書の後にデータが続いている |
| MAM-GRAPH-0010 | リソースが多すぎる |
| MAM-GRAPH-0011 | プロバイダーのリソース（`provider` を持つリソース）が `mamela-graph/v1` の文書にある（v2 が要る） |

## MAM-GRAPH — Graph の意味

| コード | 意味 |
|---|---|
| MAM-GRAPH-0101 | 名前がホスト名として正しくない |
| MAM-GRAPH-0102 | 同じリソースが 2 回宣言されている |
| MAM-GRAPH-0103 | 参照している node が Graph にない |
| MAM-GRAPH-0104 | 名前が `<node>/<package>` などの形と合わない |
| MAM-GRAPH-0105 | 値が範囲外 |
| MAM-GRAPH-0106 | 値が正しくない（パッケージ名、サービス名、イメージ名、`Present` 以外の状態） |

## MAM-POL — ポリシーの判定

| コード | 判定 | 意味 |
|---|---|---|
| MAM-POL-0101 | Auto | `autoStartServices` にあるサービスの起動（ドリフト修復） |
| MAM-POL-0102 | Auto | プロバイダーの変更で、host のカタログが分類した効果がすべて policy の `autoProviderEffects` にある |
| MAM-POL-0201 | ApprovalRequired | VM の作成 |
| MAM-POL-0202 | ApprovalRequired | VM の CPU / メモリの変更 |
| MAM-POL-0203 | ApprovalRequired | ディスクの拡張 |
| MAM-POL-0204 | ApprovalRequired | パッケージの導入（maintainer script が root で動く） |
| MAM-POL-0205 | ApprovalRequired | サービスの有効化 |
| MAM-POL-0206 | ApprovalRequired | サービスの起動（自動起動が許可されていない） |
| MAM-POL-0207 | ApprovalRequired | サービスの停止・無効化 |
| MAM-POL-0208 | ApprovalRequired | プロバイダーの変更に、host のカタログが知らない呼び出しの要求がある（要求をレビューする） |
| MAM-POL-0210 | ApprovalRequired | k3s（Kubernetes）の導入。GitHub から取得し sha256 で固定したバイナリを、root で動かす |
| MAM-POL-0209 | ApprovalRequired | プロバイダーの変更（効果は host のカタログで分類したもの） |
| MAM-POL-0301 | Denied | ディスクの縮小 |
| MAM-POL-0302 | Denied | イメージの変更（VM の置き換えになる） |
| MAM-POL-0303 | Denied | Mamela が作っていないリソース（Foreign）の変更 |
| MAM-POL-0304 | Denied | インストールすると他のパッケージが削除される（apt-get -s の変更集合に削除がある） |
| MAM-POL-0305 | Denied | 許可していない APT origin（policy の `allowedAptOrigins`。既定は `Ubuntu:`）からのパッケージ |
| MAM-POL-0306 | Denied | プロバイダーの変更に、host のカタログが「データを失いうる」とする要求がある |
| MAM-POL-0399 | Denied | ポリシーが知らない操作 |

## MAM-PLAN — 計画の状態

| コード | 意味 |
|---|---|
| MAM-PLAN-0101 | VM がまだないので待つ |
| MAM-PLAN-0102 | VM が Foreign なので、その上の設定はしない |
| MAM-PLAN-0103 | Agent からのパッケージの報告を待つ |
| MAM-PLAN-0104 | Agent からのサービスの報告を待つ |
| MAM-PLAN-0105 | mamelet が、インストールで何が変わるか（apt-get -s）を報告するのを待つ |
| MAM-PLAN-0106 | サービスの unit がまだない（それを入れるパッケージや k3s が入っていない）ので待つ |
| MAM-PLAN-0108 | mamelet からの k3s の報告を待つ |
| MAM-PLAN-0107 | プロバイダーがそのリソースを観測していない（Blocked。宣言の Pome で plan すると観測する） |
| MAM-PLAN-0201 | 観測が古いので判断しない（Blocked） |
| MAM-PLAN-0202 | mamelet がパッケージを観測・シミュレーションできない（Blocked。例: apt source にない） |
| MAM-PLAN-0204 | mamelet が k3s を観測できない（Blocked。例: unit が手で変えられた） |
| MAM-PLAN-0203 | プロバイダーの変更が、別の宣言に対して計算されたもの（Blocked。観測し直す） |
| MAM-PLAN-0301 | 宣言から消えたが、削除せず保持する（Retain） |

## MAM-REC — 実行

| コード | 意味 | 対処 |
|---|---|---|
| MAM-REC-0001 | 保存された Plan が入力から再計算した結果と一致しない | 適用しない。新しく plan する |
| MAM-REC-0002 | 実行直前の観測で、リソースが Plan の前提と違っていた | 新しく plan してレビューする |
| MAM-REC-0003 | 操作の結果が不明 | もう一度 apply する（観測してから確認か再試行をする） |
| MAM-REC-0004 | 操作が失敗した | 原因を直して apply する（失敗した操作は再試行される） |
| MAM-REC-0005 | 別の worker がこの操作を実行中 | 終わるのを待つ |
| MAM-REC-0006 | 操作の一部が効いたあとで失敗した（部分的な成功） | 原因を直して apply する（観測してから、残りを続ける） |
| MAM-REC-0007 | plan を作ったときとプロバイダーの契約が違う（マニフェスト、外部のプロバイダーのプログラム、API の接続先・資格情報・カタログ）。承認はその契約に対するもの | 新しく plan して、レビューと承認をやり直す |

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

## LAB — 検証環境

`mamela lab preflight` / `postflight` と `mamela apply` の前後の検査の結果。BLOCK が一つでもあれば実機テストをしない。

| コード | 重さ | 意味 |
|---|---|---|
| LAB-0001 | BLOCK | クラスタがクォーラムを満たしていない |
| LAB-0002 | warn | クォーラムに余裕がない（あと 1 台止まると失う） |
| LAB-0003 | BLOCK | 検証用ノードがオンラインでない |
| LAB-0101 | BLOCK | 操作用 Token が、Mamela のどの API 呼び出しも必要としない権限を持っている（権限カタログを超えている。bootstrap の再実行で外れる） |
| LAB-0102 | BLOCK | 操作用 Token から pool 外のゲストが見える（pool の所属は監査用 Token の一覧で判定する） |
| LAB-0104 | BLOCK | 操作用 Token に、権限カタログが必要とする権限が足りない（bootstrap を再実行する。`mamela lab permissions` で理由が分かる） |
| LAB-0201 | BLOCK | 予算上限の VM を作るとノードの空きメモリがしきい値を下回る |
| LAB-0202 | BLOCK | 予算上限のディスクを足すと thin pool の使用率が上限を超える |
| LAB-0203 | BLOCK | cloud image 用ストレージが content type `import` を受け付けない（bootstrap をもう一度実行する） |
| LAB-0301 | BLOCK | VMID 予約範囲に、pool の外のゲストがいる（他人のゲストか、作成タスクが終わっていない） |
| LAB-0302 | BLOCK | pool の中に、VMID 予約範囲外のゲストがいる |
| LAB-0303 | BLOCK | VM 数の予算を超える |
| LAB-0400 | ok | 管理外のゲストはすべて変わっていない |
| LAB-0401 | BLOCK | 管理外のゲストが消えた |
| LAB-0402 | BLOCK | 管理外のゲストの状態（running / stopped）が変わった |
| LAB-0403 | BLOCK | 管理外のゲストの設定 digest が変わった |
| LAB-0404 | BLOCK | 管理外のゲストが再起動した（uptime が減った） |
| LAB-0405 | warn | 管理外に新しいゲストが現れた（Mamela 以外が作った？）。予約範囲内なら、まだ pool に入っていない作成途中の VM の可能性がある |
