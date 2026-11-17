# k3s を核から分ける（設計、2026-11-16）

状態: 案 A を採り、段 0〜4（挙動を変えない部分）を実装した（0.3.2、2026-11-17）。段 5〜6 は 0.4。決めてほしいことは最後にある。

## 問題

Mamela の核は、特定のインフラを知らないはずである（ADR 0006。Proxmox と AWS は、provider として外に出した）。ところが、**マシンの中のこと**（mamelet がすること）は、核に入っている。`NodePackage`（apt）、`NodeService`（systemd）、そして `NodeK3s`、`NodeK3sManifest`。k3s は、そのなかで最も大きい。

| 層 | k3s が入っている所 | 非テストの行（k3s に触れる行） |
|---|---|---|
| plan | `internal/plan/plan.go`（版上げの順序、Ready の確認、マニフェスト、ノードの削除） | 91 |
| mamelet | `internal/mamelet/k3s.go`（導入、観測、`kubectl`）、`agent.go` | 89 + 15 |
| 通信の形 | `internal/noderev/noderev.go`（revision の `K3s`、報告の `K3sState`） | 57 |
| provider/node | `node.go`、`manifest.go`（grant、能力の宣言） | 52 + 22 |
| 宣言 | `internal/graph/*`（種別、検証、正規形） | 70 |
| 観測 | `internal/state/snapshot.go` | 20 |
| 方針 | `internal/policy/policy.go`（操作ごとの判定の文言） | 13 |
| 保管 | `internal/store/k3s.go`（参加トークン） | 7 |
| 合計 | 17 ファイル | 約 450 |

困ること:

1. **ノードの種別を 1 つ足すたびに、核の 15〜20 ファイルに手が入る。** `NodeK3sManifest` は 21 ファイル、588 行。`NodeMamelet`（mamelet の更新）は、16 ファイルを書き換え、1 ファイルを足した。種別ごとに、宣言の検証、正規形、plan、方針、revision、報告、観測、grant、mamelet の動き、Pome がそろって要る。
2. **外の人が、自分のノードの種別を足せない。** docker、PostgreSQL、Nomad を足したい人は、核を直すことになる。provider（Proxmox、AWS）は外に出せたのに、マシンの中は出せない。
3. **k3s 特有の判断が、核の plan にある。** 「Ready を確認して、1 台ずつ、server が先」は k3s の運用の知識で、核が持つべきものではない。
4. **変更の影響が広い。** k3s の変更が、核の revision と報告の形、plan の digest を動かす。

## 核に残すもの（汎用の安全の仕組み）

分けるときも、次は核が持つ。拡張は、これを呼ぶだけで、迂回できない。

- 署名つき revision、一度だけの grant（承認した操作、条件、期限）、結果の報告
- 承認と判定（`Decide`）、削除はホストだけの承認（`Change.Deletes`）、保護されたノード
- 観測の鮮度、`Late` の操作、再起動の順序の枠
- mamelet が**コンパイル済みの固定のプログラムだけ**を実行すること（下の選択肢 B を採らない理由）
- mamelet 自身の更新（`NodeMamelet`。mamelet の話で、k3s の話ではない）

## 選択肢

**A. 拡張を、コンパイル時に組み込む（推奨）。** 拡張は Go のパッケージで、CP 側と mamelet 側の両方を持つ。核は、決まった口（インターフェース）だけを知る。`cmd/mamela` と `cmd/mamelet` が `import _ ".../ext/k3s"` で組み込む。mamelet の binary に入っていない拡張の revision は、断る。k3s は、まず `ext/k3s`（このリポジトリ）に、のちに別リポジトリ（`mamela-k3s`。mamela-proxmox、mamela-aws と並ぶ）に置く。

**B. mamelet が、実行時に拡張（WASM やプロセス）を読む。** 採らない。mamelet は root で動き、**固定のプログラムだけ**を実行するから、乗っ取りに強い。実行時に取り込んだコードを動かすと、その前提が崩れる。provider の WASM は、CP のホストで API を呼ぶだけで、マシンの中では動かないから、安全でいられる。

**C. 汎用の部品（ファイルを置く、ダウンロードする、コマンドを実行する）に分けて、k3s は Pome の合成にする。** 採らない。承認つきの任意コマンド実行（Ansible 化）になり、mamelet の「固定のプログラム」が保てない。順序と健全性の確認（Ready）も、部品では表せない。ただし、`NodeFile`（承認した内容のファイル）のような、安全に表せる部品は、拡張のなかで再利用できる。

## A の設計

拡張は、1 つの名前（`k3s`）と、いくつかの種別を持つ。

```text
CP 側
  Kinds()                    宣言の種別（NodeK3s ...）と、spec の検証、正規形
  Plan(kit, resource)        plan.Change を返す（診断、diff、操作、待ち）
  Decide(op, subject)        操作ごとの判定（承認が要るか、理由の文言）
  Revise(graph, node, store) ノードの revision に入れる、拡張の部分
  Grant(step, revision)      承認した操作の許可を revision に入れる
  Capabilities()             操作が要る能力（`mamela plan` が出す一覧）
mamelet 側
  Observe(system, payload)   観測（報告に入る）
  Act(system, payload, once) 許可された操作の実行（固定のプログラムだけ）
```

- **核の汎用の型:** revision は `Ext map[name]payload`、報告は `Ext map[name]payload`、観測は `Snapshot.Ext`。payload は、拡張が決める JSON（核は中身を見ない）。graph の資源は、拡張が登録した種別の文字列と、JSON のままの spec。検証と正規形は、拡張が行う。
- **plan への口（`kit`）:** 拡張は、核の `builder` の一部だけを、読み取りで使える。マシンの状態（`machine`、`nodeReady`、`awaitAgent`）、観測の取得、同じ graph の資源の一覧、`Decide`、`stale`。値を変えたり、承認を迂回したりする口は、ない。
- **一度だけの許可、承認、削除の判定、`Late`:** 核の仕組みを、そのまま使う。拡張は、`Operation{Deletes, Late}` を返すだけで、ホストの承認は核が強制する（`NodeK3s` のノード削除は、すでにそうなっている）。

## 段階

どの段も、それだけで出荷できる。k3s の宣言と観測から作る plan を、段の前に**ゴールデンテストで固定**し、段のあとも同じ digest になることで、回帰を防ぐ（[監査](../reviews/2026-11-17-iac-audit.md) の「核と拡張」の修正点 1〜6 を入れた並び）。

| 段 | 内容 | 状態 |
|---|---|---|
| 0 | ゴールデン（下） | 済（0.3.2） |
| 1 | 各パッケージの中で、k3s のコードを `k3s.go` に分ける（宣言の移動だけ） | 済（0.3.2） |
| 2 | plan の継ぎ目: `plan.Kit` と kind ごとの `Planner`、マシン再起動とサービス起動の関門 | 済（0.3.2） |
| 3 | graph の kind と policy の操作の登録制 | 済（0.3.2） |
| 4 | k3s を `internal/ext/k3s` へ。node provider と mamelet にも登録の口 | 済（0.3.2） |
| 5 | wire の汎用の `Ext`（revision、報告、観測）、未知のキーだけを `Refused`、CP の二重 decode と `accepts` | 0.4 |
| 6 | `ext/k3s` を別リポジトリ（`mamela-k3s`）へ | 0.4 以降 |

段 5 だけが digest と wire を変える。旧い mamelet は未知のメンバーを丸ごと断るので、CP を先に、mamelet をあとに（mamelet の更新の仕組みで）替え、旧い形も読める版を 1 つ挟む。

### 段 0: ゴールデン

`internal/plan/golden_test.go` と `internal/plan/testdata/golden/`（再生成は `go test ./internal/plan -run Golden -update`）。段 1〜4 の前に main の状態で書き、段 1〜4 で 1 バイトも変わっていない。

- plan: 37 の場面（導入、版上げの順序、Ready 待ち、manifest、ノードの削除、マシン再起動の順序、mamelet の更新、拒否・保護・警告・観測の問題）。graph・snapshot・policy を固定入力に、Canonical、digest、表示、使う権限（`PlannedRequirements`）。
- graph: 並べ替え・重複を含む宣言の正規形と digest、k3s の検証と decode の問題（json の文言が版や実行で揺れるもの、Go の型名が出るものは入れない）。
- Publish/Prune が作る revision の `Content()`（参加トークンはクラスタの名前に置き換える）。
- mamelet の 2 ラウンド（実行するプログラムと引数の列、Report の JSON）と、その報告からの観測。
- node provider の Manifest と `ProvidersDigest`、全 Op の Decide 表、mamelet の許可プログラム一覧。

### 段 2〜4 の口（実装）

- **graph**（`graph.Register`）: `KindDef`（Kind、Order、Decode、Validate、Canonical、Node、Fields、Describe、New）、複数の資源にまたがる検査（`Check`）、警告と警告コード。spec は `Resource.Ext` に入り、核は中を見ない。並びは固定（核の kind は 1、2、5、k3s は 3、4、provider の kind は 5 で NodeMamelet と名前で並ぶ。前からの並びで、digest に入る）。kind・並び・警告コードの重複は起動時に panic。
- **plan**（`plan.Register`）: kind ごとの `Planner`（Plan、Reported、Deleted）、マシン再起動の関門、サービス起動の関門、理由コード。拡張は `plan.Kit`（宣言の資源、観測の写し、policy、`NodeReady`、`AwaitAgent`、`Stale`、`Decide`、`WithOps`、マシンの削除・存在・再起動）だけを使う。保護ノードの拒否（`onNode`）と警告の付け方は核。理由コードは据え置き、核や他の拡張のものとの重複は panic。
- **policy**（`policy.RegisterOp`）: 操作と、その判定。`ApprovalRequired` か `Denied` だけ（`Auto` は panic。拡張は承認を迂回できない）。核の操作・コード、他の拡張のものとの重複も panic。
- **node provider**（`node.Register`）: Observe、Publish（足すだけ。宣言が外したものは取らない）、Protect、Prune、操作ごとの Grant、manifest の kind（graph の並びで並べる）。grant の operation・attempt・期限、登録の束縛、保護ノード、結果の待ちは核。
- **mamelet**（`mamelet.Register`）: Observe、Act、Results、実行するプログラム。1 ラウンドの順序は前と同じ（パッケージ、拡張の BeforeUpdate、mamelet の更新、拡張の AfterUpdate、サービス）。許可プログラムは核と拡張の和集合。
- `cmd/mamela` と `cmd/mamelet` が `_ ".../internal/ext/k3s"` で組み込む。`internal/ext/deps_test.go` が、核（`internal/...` と `provider/...` の ext 以外）が ext に依存しないこと、二つのプログラムが ext/k3s を組み込むことを `go list -deps` で確かめる。

移した量: `internal/ext/k3s` の非テストのコードは約 1,700 行（graph 360、plan 460、policy 30、node provider 300、mamelet 570。コメント込み）。核に足した口は約 700 行（`graph/kinds.go`、`plan/kit.go`、`provider/node/ext.go`、`mamelet/ext.go`、policy の登録）。

### 核に残った k3s の知識（段 5 で動かす）

- wire と保存の形: `noderev` の `Revision.K3s`・`Manifests`・`Report.K3s`・`K3sState`（検査の `Check` と `Content` を含む）、`state.Snapshot` の `K3s`・`K3sManifests`、`names` の k3s の版と manifest の名前の形、`store` の `k3s_tokens` 表と `K3sToken`。変えると revision・snapshot の digest と旧い mamelet との互換が変わるので、段 5 で `Ext` に載せ替える。`plan.Kit` が観測を写すときも、この二つの欄の名前を知っている。
- `mamelet.System` の `HTTP`・`K3sReleases`・`Addresses`（k3s のダウンロード先とアドレスの読み方。設定の型で、テストと cmd が使う）。
- 拡張の名前と版は、node provider の manifest（`ProvidersDigest`）にまだ入れていない。入れると全 plan の digest が変わるので、段 5 で入れる。

## 先にやること

mamelet の更新の仕組み（[mamelet の更新](../mamelet-update.md)、入った）。段 5 は、revision と報告の形を変えるので、全ノードの mamelet を新しい形に一斉に替える必要があり、手で替えるのは、検証環境の 3 台を超えると続かない。

## 決めてほしいこと

1. 拡張は、**コンパイル時に組み込む**（実行時のプラグインなし）でよいか。
2. `NodePackage` と `NodeService`（apt、systemd）は、核に残すか、同じ仕組みの標準の拡張（`ext/os`）にするか。推奨は、まず核に残し、段 5 のあとで判断する（0.3.2 では核に残した）。
3. k3s を別リポジトリに出す時期（段 6）。
