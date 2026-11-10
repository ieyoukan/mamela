# `mamela-graph/v1` — Resource Graph の受け渡し形式

Emela の宣言を WASM で評価したとき、stdout に書かれる JSON 1 文書。Go 側の正は `internal/graph`。

## 例

~~~json
{
  "resources": [
    {"kind": "VirtualMachine", "name": "app-server",
     "spec": {"cpu": 1, "diskGiB": 10, "image": "ubuntu-24.04", "memoryMiB": 1024}},
    {"kind": "NodePackage", "name": "app-server/nginx",
     "spec": {"node": "app-server", "package": "nginx", "state": "Present"}},
    {"kind": "NodeService", "name": "app-server/nginx",
     "spec": {"enabled": true, "node": "app-server", "running": true, "service": "nginx"}}
  ],
  "schema": "mamela-graph/v1"
}
~~~

実際の出力は空白なし・キーはソート順の 1 行。

## 規則

- 最上位は `schema`（値は `"mamela-graph/v1"` のみ）と `resources`（配列）の 2 つ。
- 各リソースは `kind`・`name`・`spec` の 3 つ。
- 書かれていないメンバー、重複したメンバー、`null`、小数・指数表記の数値、不正な UTF-8、文書の後に続くデータは、すべて拒否する（末尾の改行 1 つだけは許す）。
- 文書は 4 MiB まで、リソースは 10,000 件まで。

## kind

### VirtualMachine

| フィールド | 型 | 範囲 |
|---|---|---|
| `name` | string | ホスト名（小文字・数字・`-`、1–63 文字） |
| `spec.cpu` | 整数 | 1–64 |
| `spec.memoryMiB` | 整数 | 256–1,048,576 |
| `spec.diskGiB` | 整数 | 1–16,384 |
| `spec.image` | string | `ubuntu-24.04` のような名前 |

### NodePackage

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/<package>` |
| `spec.node` | string | 同じ Graph の VirtualMachine の名前 |
| `spec.package` | string | Debian のパッケージ名 |
| `spec.state` | string | `"Present"` のみ。削除は宣言できない |

### NodeService

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/<service>` |
| `spec.node` | string | 同じ Graph の VirtualMachine の名前 |
| `spec.service` | string | systemd のユニット名（`.service` なし） |
| `spec.enabled` | bool | |
| `spec.running` | bool | |

### NodeK3s

k3s（軽量な Kubernetes）の server または agent。mamelet が、承認のあと一度だけ、GitHub のリリースからバイナリを取得し、sha256 が一致したときだけ `/usr/local/bin/k3s` に置いて、Mamela が決めた systemd unit を書く。起動は `NodeService`（`k3s`）で行う。

| フィールド | 型 | 規則 |
|---|---|---|
| `name` | string | `<node>/k3s` |
| `spec.node` | string | 同じ Graph の VirtualMachine の名前 |
| `spec.role` | string | `"server"` か `"agent"` |
| `spec.server` | string | agent のとき、同じ Graph で k3s server を宣言した VM の名前（server のときは書かない） |
| `spec.version` | string | k3s のリリース（`v1.37.2+k3s1`） |
| `spec.sha256` | string | そのノードのアーキテクチャのバイナリの sha256（16 進の小文字 64 桁。リリースの `sha256sum-<arch>.txt`） |

起動の引数は Mamela が決める。宣言からは変えられない。

| 役割 | 起動の引数 |
|---|---|
| server | `k3s server --token-file=/etc/rancher/k3s/cluster-token --disable=traefik --disable=metrics-server --write-kubeconfig-mode=0600` |
| agent | `k3s agent --server=https://<server のアドレス>:6443 --token-file=/etc/rancher/k3s/cluster-token` |

- **参加のトークン:** Control Plane がクラスタごとに乱数で作る。そのクラスタのノードの署名した revision にだけ入れる（ノードとの接続は相互認証の TLS）。宣言・plan・報告には出ない。
- **server のアドレス:** server の mamelet が報告した IPv4 アドレスを使う。plan はそれを表示し、承認したアドレスと revision のアドレスが違えば導入しない。

## プロバイダーのリソース（`mamela-graph/v2`）

外部のプロバイダーの種類のリソースは、`provider` を持つ。これがある文書は `"schema": "mamela-graph/v2"` にする（v1 の文書にあれば MAM-GRAPH-0011）。v1 の文書は、プロバイダーのリソースがない v2 の文書と同じ意味である。Mamela は両方を読み、プロバイダーのリソースがないときは v1 を書く。

~~~json
{"kind": "KVItem", "name": "web", "provider": "kv-example", "spec": {"labels": ["prod"], "value": "v1"}}
~~~

| フィールド | 規則 |
|---|---|
| `kind` | 英大文字で始まり、英数字が 63 文字まで。組み込みの kind は使えない |
| `provider` | プロバイダーの名前（マニフェストの `provider`）。英小文字・数字・`-` |
| `name` | 英数字と `.` `_` `:` `@` `-`、253 文字まで |
| `spec` | JSON のオブジェクト。文字列・整数・真偽値・配列・オブジェクトだけ（`null` と小数は不可）。整数は ±(2^53−1) まで（正規形は数を倍精度で書くので、それを超える整数は別の値になる。黙って変えずに拒否する。大きな値は文字列で書く）。深さ 32 まで。Mamela は中身を解釈せず、正規形（RFC 8785）にしてプロバイダーに渡す |

宣言では、プロバイダーの Pome の関数（`Kv.item(...)`）で書く。それらは `Mamela.provided` を呼ぶ。

## 正規化と digest

- 正規形: リソースを kind の順（VirtualMachine → NodePackage → NodeService → NodeK3s → プロバイダーの kind を名前の順）、次に名前の順に並べ、キーをソートし、空白なしで書いたもの。
- digest: 正規形の SHA-256（`sha256:<hex>`）。宣言の順番を入れ替えても digest は変わらない。
- `pome/mamela` の出力は、リソースが kind 順に並んでいれば正規形と 1 バイトも違わない（テストで確認）。

## 互換性

形を変えるときは `mamela-graph/v2` にする。v1 のまま意味を変えない。
