# k3s を核から分ける（設計、2026-11-16）

状態: 提案。決めてほしいことは最後にある。

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

どの段も、それだけで出荷できる。k3s の宣言と観測から作る plan を、段の前に**ゴールデンテストで固定**し、段のあとも同じ digest になることで、回帰を防ぐ。

0. **ゴールデン:** k3s の宣言と観測（導入、版上げ、順序、Ready、マニフェスト、ノードの削除）の plan の JSON を、テストに固定する。
1. **パッケージの移動:** k3s のコード（plan、mamelet、graph、provider/node）を、`internal/ext/k3s` へ。核は、薄いフックで呼ぶ。通信の形は、まだ変えない。機械的で、digest は変わらない。
2. **汎用の `Ext` の形:** revision、報告、観測に `Ext` を足し、k3s を載せ替える。形が変わるので、**CP を先に、mamelet をあとに**更新する（mamelet の更新の仕組みが、ここで要る）。旧い形も読める版を 1 つ挟む。
3. **種別の登録制:** `graph` が、拡張の種別を登録で知る。`graph.go` から k3s の型が消える。
4. **別のリポジトリへ:** `ext/k3s` を `mamela-k3s` へ。Pome（`pome/k3s`）は、すでに別である。

見積もり: 段 0〜1 は 1〜2 日分、段 2〜3 が本体（数日）、段 4 は移すだけ。

## 先にやること

mamelet の更新の仕組み（[mamelet の更新](../mamelet-update.md)、本ブランチ）。段 2 は、revision と報告の形を変えるので、全ノードの mamelet を新しい形に一斉に替える必要があり、手で替えるのは、検証環境の 3 台を超えると続かない。

## 決めてほしいこと

1. 拡張は、**コンパイル時に組み込む**（実行時のプラグインなし）でよいか。
2. `NodePackage` と `NodeService`（apt、systemd）は、核に残すか、同じ仕組みの標準の拡張（`ext/os`）にするか。推奨は、まず核に残し、段 3 のあとで判断する。
3. k3s を別リポジトリに出す時期（段 4）。
