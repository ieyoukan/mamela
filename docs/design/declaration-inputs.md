# 宣言の入力: ファイルの取り込みと、配布物の sha256 の固定

- 状態: 採用（2026-11-21）。段 1〜3 を実装する。
- 対象: Go の `mamela`（評価、`mamela pin`）、Mamela Pome と K3s Pome の API、mamelacd が Control Plane に渡す宣言のツリー。
- 前提: Emela の依存管理は Pome のまま。Emela の言語仕様はこの設計では変えない（長い目で見た案は「Emela への提案」）。

**長い YAML や unit は、拡張子のあるファイルのまま宣言の Pome に置き、宣言からは相対パスで指す。配布物の sha256 は、宣言には書かず、`mamela pin` が書く `mamela.sum` に置く。** どちらも Go が評価のときに Pome のツリーから読み、Graph を検証する前に埋める。Emela のコードを生成しないので、生成ファイルのコミット、生成忘れを見張る hook、宣言のリポジトリの Python は要らなくなる。

## 問題

宣言が大きくなると、VM の定義、ソフトウェアの版、導入の手順、YAML、unit が一つの `main.emel` に集まる。ファイルを分けるだけなら Emela のモジュールでできた（home-infra-mamela の分割、2026-11-21。plan は分割の前後で in sync）。それでも次が残った。

- **テキスト:** 宣言の WASM はファイルを読めない。YAML や unit を Emela の文字列にするスクリプトと、生成した `.emel` のコミット、生成忘れを見る pre-commit hook が要った。hook は各自の任意の設定なので、入れていない環境から push すると、古い生成物がそのまま適用される。
- **配布物:** 版を変えるたびに、URL、sha256、アーカイブの中のパスを別々に直す。版と sha256 を JSON に置き、Emela の定数を生成するスクリプトが要った。
- **言語:** 宣言を書く人と、それを確かめる環境が、Python を用意する。

## 方針

| 書くもの | 書く場所 | 誰が書くか |
|---|---|---|
| VM、ネットワーク、ソフトウェアの配置、導入のレシピ | 宣言の Pome の Emela | 人 |
| 版と取得先の URL | それを使うレシピ（Emela） | 人 |
| YAML、unit などのテキスト | 宣言の Pome の中のファイル（`manifests/argocd.yaml`） | 人 |
| 配布物の sha256 | 宣言の Pome の `mamela.sum` | `mamela pin` |
| ライブラリと provider の Pome | `Pome.toml`、`Pome.lock` | 人と `emela pome` |

- go.mod と go.sum と同じ分け方にする。人が書いて読むのは版と URL。sha256 は道具が書き、レビューでは差分を見る。
- 評価はネットワークを使わず、`mamela.sum` も書き換えない。取得するのは、人が呼ぶ `mamela pin` だけ。
- Graph の正規形は変えない。ファイルの中身と sha256 は、正規形では今と同じ `content` と `sha256` に入る。だから、同じ中身なら Graph と plan の digest は前と同じ。
- Emela/WASM がするのは Desired State の生成まで。取り込んだテキストも sha256 も、今の schema・policy・承認・mamelet を通る。

## 1. ファイルの取り込み（`contentFrom`）

`NodeFile` と `NodeK3sManifest` の spec に、`content` の代わりに `contentFrom`（Pome のルートからの相対パス）を書ける。両方はなく、どちらか一つ。

```emela
# 構文は Mamela Pome 0.4.1 の案
graph
  |> Mamela.file_from(node, "/etc/systemd/system/node-exporter.service", "files/node-exporter.service")
  |> K3s.manifest_from(server, "argocd", "manifests/argocd.yaml")
```

- **読むもの:** 評価した Pome のツリーの中のファイルだけ。パスは相対で、`path.Clean` したものと同じで、`..` を含まない。パスのどの段もシンボリックリンクではなく、最後は通常のファイル。
- **中身:** UTF-8 のテキストをそのまま使う。末尾の改行も含めてバイトのとおりで、変数の展開も、改行の正規化もしない。大きさは、その kind の上限（`NodeFile` 256 KiB、manifest は `names.MaxManifest`）と、1 つの宣言で読むファイルの合計の上限で限る。
- **正規形:** Go が、Graph を検証する前に `contentFrom` を読んで `content` に置き換える。正規形と digest に `contentFrom` は残らない。plan には、中身の全文に加えて、どのファイルから来たかを示す。
- **理由コード:** ファイルがない、範囲の外、リンク、テキストでない、大きすぎる、を区別する（MAM-EVAL の新しい番号）。
- **mamelacd:** Control Plane に渡す宣言のツリーに、Pome の中の `.emel` 以外のファイルも入れる（入っていなければ、ここで直す）。評価はそのツリーだけを読む。
- **エディタ:** 生成するモジュールがないので、言語サーバーと `emela check` はそのまま使える。指したファイルがあるかは、`mamela check` が確かめる。
- **実装（段 1）:** 核は kind の member を知らない。kind の `Decode` が `SpecDecoder.Text("content", content, contentFrom, 上限)` で中身を求め（`content` と `contentFrom` のどちらか一方）、評価器が `graph.Files` として Pome のディレクトリから読む（`graph.DecodeWith`）。`NodeFile`（linux）と `NodeK3sManifest`（k3s）がこれを使う。読んだパスは `Resource.Sources` に残し、正規形・digest には入れない。plan は `Change.Sources`（digest の外）から diff に `content (from files/app.conf)` と出し、`mamela eval` の一覧と、中身の問題の場所にも出す。理由コードは MAM-EVAL-0501〜0506（[reason-codes.md](../reason-codes.md)）。mamelacd が送るツリーは、もともとコミットのすべてのファイルを含んでいた（`internal/tree` がリンクと特殊なファイルを断る）。

## 2. 配布物の sha256（`mamela.sum` と `mamela pin`）

sha256 を書かない宣言は、その sha256 を `mamela.sum` から取る。

```emela
# 構文は案
Mamela.artifact_with(node, Mamela.Artifact(
  extract: "node_exporter-#{v}.linux-amd64/node_exporter",
  ..Mamela.artifact_pinned("/usr/local/bin/node_exporter",
    "https://github.com/prometheus/node_exporter/releases/download/v#{v}/node_exporter-#{v}.linux-amd64.tar.gz"),
))
```

```text
# mamela.sum（mamela pin が書く。手で直さない）
https://dl.cloudsmith.io/public/caddy/stable/gpg.key sha256:783dfee0...
https://github.com/k3s-io/k3s/releases/download/v1.37.2%2Bk3s1/k3s sha256:265b3ee6...
https://github.com/prometheus/node_exporter/releases/download/v1.13.0/node_exporter-1.13.0.linux-amd64.tar.gz sha256:f6a6c72e...
```

- **キー:** 取得先の URL（mamelet が実際に取りに行くもの）。版を変えると URL が変わり、`mamela.sum` にない状態になるので、古い sha256 が残ったまま評価が通ることはない。前の案の `request_digest` は要らない。
- **対象:**
  - `NodeArtifact` の `url`
  - `NodeAptRepo` の `keyUrl`
  - `NodeK3s` のリリースの URL（k3s 拡張が版から作る。今は linux/amd64 だけ）
- **`NodeMamelet`:** 公開の URL がないので、今は宣言に sha256 を書く。mamelet を sha256 つきでリリースとして公開したら、同じ形にする。
- **宣言に sha256 を書いたもの:** これまでどおりそれを使う。`mamela.sum` は見ない。今の宣言は変えなくても動く。
- **評価:** `mamela.sum` は Pome のルートに置く。Go が、Graph を検証する前に sha256 を埋める。`mamela.sum` にない URL があれば、URL と「`mamela pin` を実行する」を示して評価を止める（MAM-EVAL の新しい番号）。形の不正、重複、未知の行も拒む。
- **`mamela pin <pome-dir>`:**
  - 宣言を評価して、sha256 の要る URL を集める。
  - `mamela.sum` にない URL だけを取得する。https だけ（リダイレクトも）で、大きさと時間の上限がある。
  - 取得したバイト列の sha256 を書く。GitHub のリリースの asset なら、GitHub が返す asset の digest とも照合し、食い違えば書かない。
  - 使われなくなった行は消す。行は URL の順に並べ、書き換えは一時ファイルと rename で行う。
- **`mamela pin --check`:** ネットワークを使わない。足りない行と、使われていない行を示して、ファイルは変えない。
- **`mamela pin --verify`:** 既存の行も取得し直して照合する。
- **信頼:** pin が確かめるのは、取得した時点のバイト列だけ（初回に見たものを信じる、TOFU）。守りの中心は、PR で版と URL の差分を人が読むこと。署名鍵（`keyUrl`）を新しく固定するときは、`mamela pin` が鍵の sha256 を示し、配布元の指紋と照合するよう案内する。

## 3. 書きやすくする部品（Mamela Pome 0.4.1）

宣言で何度も書いていた組み合わせを、Pome の関数にする。どれも今の kind を出すだけで、新しい kind は足さない。

- `Mamela.running(graph, node, name)`: 有効にして動かす（`service(node, name, True, True)`）。
- `Mamela.system_user(graph, node, name)`: system ユーザー。
- `Mamela.systemd_service(graph, node, name, unit)`: `/etc/systemd/system/<name>.service` に unit を置き、そのサービスを再起動の対象にして、有効にして動かす。`systemd_service_from` はファイルから取り込む版。
- `Mamela.file_from`、`Mamela.artifact_pinned`、`K3s.manifest_from`、`K3s.server_pinned`、`K3s.agent_pinned`（1、2 の口）。

node_exporter のレシピは次のくらいになる（案）。

```emela
pub fn node_exporter(graph: Mamela.Graph, node: Mamela.Node, v: String) -> Mamela.Graph {
  graph
    |> Mamela.system_user(node, "node-exporter")
    |> Mamela.artifact_with(node, Mamela.Artifact(
      extract: "node_exporter-#{v}.linux-amd64/node_exporter",
      restart: ["node-exporter"],
      ..Mamela.artifact_pinned("/usr/local/bin/node_exporter", "https://github.com/prometheus/node_exporter/releases/download/v#{v}/node_exporter-#{v}.linux-amd64.tar.gz"),
    ))
    |> Mamela.systemd_service_from(node, "node-exporter", "files/node-exporter.service")
}
```

## 段階

| 段 | 内容 | 完了条件 |
|---|---|---|
| 1 | `contentFrom`（`NodeFile`、`NodeK3sManifest`）、mamelacd のツリー、Pome の `file_from` / `manifest_from` | 同じ中身なら Graph の digest が同じ。拒否のテスト（なし、範囲外、リンク、非 UTF-8、上限） |
| 2 | `mamela.sum`、`mamela pin`（`--check`、`--verify`）、Pome の `artifact_pinned` / `server_pinned` / `agent_pinned` | sha256 を書いた宣言と、sum から埋めた宣言の Graph が同じ。オフラインの評価。取得の失敗で sum を変えない |
| 3 | 部品（`running`、`system_user`、`systemd_service(_from)`） | Mamela Pome 0.4.1、K3s Pome 0.1.2 |
| 4 | 検証環境の宣言（home-infra-mamela）を移す | Python、生成ファイル、hook がなくなり、plan は中身の差（末尾の改行）を除いて in sync |

Control Plane（`mamela`）を先に、宣言をあとに更新する。古い評価器は `contentFrom` や sha256 のない spec を MAM-GRAPH-0003 / 0004 で拒むので、黙って違う中身で動くことはない。

## Emela への提案（出していない）

テキストの取り込みは、言語の機能のほうが自然である。たとえば `embed("manifests/argocd.yaml")` のような、コンパイル時に Pome の中のファイルを `String` として取り込む式があれば、次が満たせる。

- 中身は Pome の内容の hash に入る。
- 言語サーバーと `emela check` がそのまま扱える。
- 取り込んだ中身では、補間が効かない。

1 の `contentFrom` は、それまでのつなぎであり、言語に入ったら Pome の関数の中身を差し替える。Emela のリポジトリへの提案は、Mamela の作者が判断して出す（このリポジトリからは書き込めない）。

## 採らなかった案

- **`mamela.inputs.toml` から Emela のモジュールを生成する（この文書の前の版）:** 次のものが要り、どれも生成するから要るものだった。
  - 生成のエスケープ
  - 一時領域への Pome のコピーと `[replace]` の相対パス
  - エディタ用の `prepare --watch` とモジュール名の予約
  - 取得のパターン（repository、asset、extract）を TOML に置くので、レシピの知識が Emela と TOML に分かれ、レシピを Pome に切り出せなくなる。
- **WASM に stdin で入力を渡す:** WASM の import の許可（stdout、stderr、exit）を広げることになり、受け取った値に型もない。
- **宣言のリポジトリに Python を置き続ける:** 生成忘れを防げない。書く人の用意も増える。

## 関連

- [Emela / WASM の評価](../emela-wasm.md)
- [ADR 0007: Pome と依存](../adr/0007-stable-pome-and-dependencies.md)
- [ADR 0015: provider の固定](../adr/0015-provider-pinning.md)
- [ADR 0016: linux の部品](../adr/0016-linux-primitives.md)
- [宣言のリポジトリで運用する](../gitops.md)
- [Pome のリリース](../releasing.md)
