# 利用者として書いて動かしたレビュー（2026-11-12）

最終確認: 2026-11-13。検証用 VM・ディスク・専用 seed とノード登録の後片付けは完了。
下記は調査時点の指摘。実装した対応は [修正記録](2026-11-13-operator-fixes.md) に分けた。

## 結論

宣言から消えたリソースを常に保持する現在の制約は、選べるライフサイクルに変えるべきである。
宣言に追従する `delete` と、実物を残す `retain` を用意し、削除の承認要否は別に設定する。
ただし現在の「未宣言」の判定は、同じ Control Plane が知る別構成のリソースも含む。
削除の判定をそのまま有効にするのでなく、先に構成の管理範囲と適用済みの状態を実装する必要がある。
具体案は [宣言に追従する削除](../design/declarative-removal.md)。設定名は設計案であり、まだ使えない。

宣言の書き心地はよい。型のある VM を作り、そのノードに package と service をつなげる部分は短く、
Emela の関数で役割をまとめられる。一方、初期導入、観測待ち、繰り返しの plan / approve / apply、
状態の場所の指定まで含めると、まだ自然に使えるところまでは届いていない。

## Terraform との比較

現時点で Terraform より優れている、とは評価しない。Mamela の強みになり得るのは、型のある宣言で
VM とノード内の package / service を結び、pull 型エージェントと権限制約を同じ仕組みで扱えること。
今回、その経路で VM 作成から nginx の応答まで動かせた。一方、導入と収束の操作には未完成の部分がある。

Terraform は state にある資源が宣言から消えた場合、apply で削除する。
保持したい場合は `removed` の `lifecycle.destroy = false` で実物を残して管理から外せる。
「削除しないこと」自体を Mamela の優位性にする理由はない。
参考: [Terraform のリソース管理](https://developer.hashicorp.com/terraform/language/resources)、
[removed の仕様](https://developer.hashicorp.com/terraform/language/block/removed)。

また、Terraform の plan は提案した変更を実行しない。
Mamela もこの境界を満たす必要がある。
参考: [terraform plan](https://developer.hashicorp.com/terraform/cli/commands/plan)。

## 確認の範囲

- 新しい一時ディレクトリで `emela new` から開始し、Mamela と mamela-proxmox の依存を設定した。
- 公開版の Mamela Pome v0.3.0 の取得は、タグが存在せず失敗した。以後はローカル checkout を
  `[replace]` で参照した。これは公開された導入手順の成功とは数えない。
- VM 1 台、nginx のパッケージとサービスを手書きした。評価を 2 回実行し、同じ graph digest を確認した。
- 既設の Control Plane と mamelet を SSH で確認した。Proxmox ホスト自身と、その上の CP の VM は別物だった。
- 実機操作は mamela-proxmox、既存の境界付き資格情報、guard、`MAMELA_LAB=1` を通した。
  作成した一時 VM だけを変更した。既存の CP と業務用のゲストは変更していない。
- 既存テンプレートから VM を作成・起動し、cloud-init、mamelet の登録、観測、承認済みの nginx 導入、
  HTTP 200、宣言した 4 リソースの in sync を確認した。
- `running: false` に相当する宣言に変え、plan / approve / apply を通して nginx を停止した。
  次に起動する宣言と `autoStartServices` を指定して plan だけを実行したところ、16 秒後に mamelet が
  drift repair として起動した。approve / apply は実行していない。HTTP 000 → 200 を確認した。
- 宣言を空にして再計画した結果、一時 VM は retained になり、削除されなかった。
- 各 apply の事後 guard は、境界外のゲストに変化がないことを報告した。
- 実機の CLI は `001f1a9e4742`。ローカルの調査開始時は `0f7c88a5cd15`。
  作業中に別作業の GitOps 実装 `8fbb13f` が入り、そのコードも追加確認した。実機の GitOps は実行していない。
- 実アドレス、ホスト名、既存ゲストの一覧、資格情報はこの文書に記載しない。

## 修正の優先順位

### P1: plan が実行許可を配信する

`cmd/mamela/plan.go` は `node.Publish` を呼ぶ。`internal/provider/node/node.go` は、宣言と
`autoStartServices` から `Service.Repair = [start]` を生成する。mamelet はこれを受け取ると
apply を待たずにサービスを起動できる。表示の「Nothing has been changed」と整合しない。
GitOps の PR の plan でも同じ処理が呼ばれるため、PR の評価と適用を区別する設計にも影響する。
上の一時 VM で、plan から 16 秒後の起動を実測した。

修正は、観測してほしい対象と、有効な Desired State / 実行許可を分離すること。
plan は観測を要求できても、新しい実行許可や有効な構成を公開しない。
apply が、保存した plan とポリシーに結び付いた構成を有効化する。
既存の修復権限を単にすべて消すだけでは、plan が既存の運用を変えてしまうので不十分である。

関連する実装不具合として、保護対象のノードでも、この経路からサービスを起動できた。
偽の OS と実際の mamelet で再現するテストを追加し、保護対象には新規・保留中の実行許可を渡さないよう修正した。
追記（2026-11-13）: plan は観測対象だけを追加し、既存の値と許可を保持するよう修正した。
自動修復の許可は独立した差分として表示し、apply で変更する。

### P1: 別構成のリソースを「削除された」と判定する

新しい構成の plan に、既存の別構成の VM が retained として並び、
「removed from the declaration」と表示された。`internal/plan/plan.go` は観測した Owned のリソースを
現在のグラフに含まれるかだけで判定している。適用済みの構成との差分ではない。

`stack_id` と安定した resource identity、適用済みの所属、共有依存を記録する必要がある。
削除対象は「この stack に所属していたものが、その stack の次の適用でなくなる場合」に限定する。
共有テンプレートは、ほかの stack の VM が参照している間は消せない。

表示は今回修正した。保持を削除に見える `-` で表示せず、`=` とし、
「この宣言にはない。別構成のものかもしれない」と説明する。管理範囲の分離そのものは未実装。

### P1: 変更の完了と、宣言全体の収束を区別する

今回の操作では、VM 作成後にノードの観測対象を配信する plan、報告後の package の plan、
その適用後の確認が必要だった。1 回の apply が成功しても、待機中のリソースが残り得る。

GitOps の `cliMamela.Plan` は StatusChange だけを数え、Engine は件数 0 を in sync と扱う。
そのため waiting / blocked だけの plan でも成功表示になり得る。
また、1 回の apply 後に適用済みと記録し、後続の依存段階を再計画する処理はない。
これはコード確認による指摘で、GitHub 上での再現試験は行っていない。

状態を `Converged / Waiting / ApprovalRequired / Blocked / Failed` に分ける。
apply 後は再観測し、残りの変更を表示する。新たな変更を古い承認で黙って実行せず、
承認範囲内の自動収束か、新しい承認が必要かを区別する。

### P1: ノード削除後の登録・権限を片付ける

`cmd/mamela/act.go` の削除成功時は `ForgetCreations` のみで、agent の登録、revision、report、
未使用の登録トークンを消さない。CP の認証は登録済みの証明書を確認するので、
実物の削除とノード権限の失効を結び付ける必要がある。

削除を表す契約も必要である。`lose-data` は効果の分類であって「対象が存在しなくなる」という意味ではない。
データを失う操作全般を、所有記録の削除条件にしてはいけない。
削除完了の再観測、所有記録の更新、ノード登録・許可の失効を回復可能な手順として記録する。
監査履歴は残し、同名で作り直しても古い証明書や許可が有効にならないようにする。

### P2: 導入と日々の操作を短くする

- **公開依存の整合:** 現在の CLI・Mamela Pome・プロバイダーを組み合わせて新規ディレクトリから
  install → eval → plan するリリース試験を設ける。未公開 v0.3.0 の代わりに v0.2.0 を使う案内は、
  現在の provider / Node API と互換ではない。
- **固定 IP の衝突:** 既存 VM と同じアドレスでも作成 plan が通った。その plan は適用せず、
  検証には予約範囲内の別アドレスを使った。少なくとも管理対象間の衝突を guard で拒否する必要がある。
  許可範囲内であることと、空いていることは別である。
- **接続設定:** 手動操作では CP に入り、DB、site、providers、toolchain、cache を指定した。
  名前付きの接続先と project 設定で一度だけ決めたい。GitOps は入口を改善するが、初期準備と診断には CLI が要る。
- **plan の表示:** 送信する HTTP 要求、権限一覧、内部の所有マーカーまで毎回展開される。
  既定は変更、対象、理由、承認要否に絞り、要求全文を詳細表示で確認できるようにしたい。
- **次のコマンド:** 表示される `mamela approve <digest>` は、指定した `--db` を引き継がない。
  同じ DB を使うコマンドを提示するか、project の設定に固定する必要がある。
- **plan による予約:** 試すだけの plan も作成 ID を一定時間予約する。書き直すと VMID が進んだ。
  同じ stack / resource の plan の更新では予約を引き継ぎ、破棄した plan の解放を明示したい。

## 実装する順序

1. plan と有効な revision の分離、保護対象の強制、収束状態の正しい表示。
2. stack ごとの所有と適用済み revision。削除成功時のノード権限の失効。
3. `delete / retain` と削除の承認設定。依存の逆順での削除と中断・再実行のテスト。
4. 公開版で成立する導入手順、接続設定、短い plan、収束までの案内。

「常に消さない」から「所有範囲の中で、選んだライフサイクルと承認方針に忠実に動く」へ移すのが、
IaC としても AI に任せるツールとしても一貫した方向である。

## このレビューで変更したものと検証

- 保護対象ノードには、package / service / k3s の実行許可を新規付与・引き継ぎしないよう修正。
  mamelet を使う回帰テストで、修正前に保護対象のサービスが起動することと、修正後に停止したままであることを確認した。
  この修正とテストは、並行作業のコミット `8fbb13f` に取り込まれた。
- 未宣言の保持を `=` で表示し、「削除された」と断定しない文言に変更。
- README の「評価経路だけが動く」という古い記述と、互換性のない公開 Pome の代用案内を修正。
- `go test ./...`、対象パッケージの回帰テスト、`scripts/check.sh` を実行して成功。
  最後のチェックには Go の race detector、vet、Emela の型検査・JS / WASM テスト、決定性の確認を含む。
- `git diff --check` は成功。変更した CLI を既設 Control Plane にデプロイしてはいない。
- ライフサイクル設定と stack の分離は設計案として記録した。実装済みとは扱わない。
