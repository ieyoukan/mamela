# 理由コード

Mamela が止まったり拒否したりしたときに表示するコード。一度決めたコードを別の意味に使い回さない。正は `internal/graph/problem.go`、`internal/evaluate/errors.go`、`internal/policy/policy.go`、`internal/plan/plan.go`、`internal/reconcile/reconcile.go`。

## MAM-EVAL — 評価

| コード | 意味 | よくある原因と対処 |
|---|---|---|
| MAM-EVAL-0001 | ツールチェーンが見つからない、またはバージョンが違う | `scripts/install-toolchain.sh` を実行する |
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
| MAM-POL-0201 | ApprovalRequired | VM の作成 |
| MAM-POL-0202 | ApprovalRequired | VM の CPU / メモリの変更 |
| MAM-POL-0203 | ApprovalRequired | ディスクの拡張 |
| MAM-POL-0204 | ApprovalRequired | パッケージの導入（maintainer script が root で動く） |
| MAM-POL-0205 | ApprovalRequired | サービスの有効化 |
| MAM-POL-0206 | ApprovalRequired | サービスの起動（自動起動が許可されていない） |
| MAM-POL-0207 | ApprovalRequired | サービスの停止・無効化 |
| MAM-POL-0301 | Denied | ディスクの縮小 |
| MAM-POL-0302 | Denied | イメージの変更（VM の置き換えになる） |
| MAM-POL-0303 | Denied | Mamela が作っていないリソース（Foreign）の変更 |
| MAM-POL-0399 | Denied | ポリシーが知らない操作 |

## MAM-PLAN — 計画の状態

| コード | 意味 |
|---|---|
| MAM-PLAN-0101 | VM がまだないので待つ |
| MAM-PLAN-0102 | VM が Foreign なので、その上の設定はしない |
| MAM-PLAN-0103 | Agent からのパッケージの報告を待つ |
| MAM-PLAN-0104 | Agent からのサービスの報告を待つ |
| MAM-PLAN-0201 | 観測が古いので判断しない（Blocked） |
| MAM-PLAN-0301 | 宣言から消えたが、削除せず保持する（Retain） |

## MAM-REC — 実行

| コード | 意味 | 対処 |
|---|---|---|
| MAM-REC-0001 | 保存された Plan が入力から再計算した結果と一致しない | 適用しない。新しく plan する |
| MAM-REC-0002 | 実行直前の観測で、リソースが Plan の前提と違っていた | 新しく plan してレビューする |
| MAM-REC-0003 | 操作の結果が不明 | もう一度 apply する（観測してから確認か再試行をする） |
| MAM-REC-0004 | 操作が失敗した | 原因を直して apply する（失敗した操作は再試行される） |
| MAM-REC-0005 | 別の worker がこの操作を実行中 | 終わるのを待つ |

## LAB — 検証環境

`mamela lab preflight` / `postflight` と `mamela apply` の前後の検査の結果。BLOCK が一つでもあれば実機テストをしない。

| コード | 重さ | 意味 |
|---|---|---|
| LAB-0001 | BLOCK | クラスタがクォーラムを満たしていない |
| LAB-0002 | warn | クォーラムに余裕がない（あと 1 台止まると失う） |
| LAB-0003 | BLOCK | 検証用ノードがオンラインでない |
| LAB-0101 | BLOCK | 操作用 Token が境界を超える権限を持っている |
| LAB-0102 | BLOCK | 操作用 Token から pool 外のゲストが見える（pool の所属は監査用 Token の一覧で判定する） |
| LAB-0104 | BLOCK | 操作用 Token に、bootstrap が付けるはずの権限が足りない（bootstrap を再実行する） |
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
