# mamelet なし、mamelacd なしで動くか、分けられるか（調査、2026-11-16）

「動く」は実機（検証環境）で試したこと、「コードから」は依存関係とコードを読んで言えること。

## 3 つのプログラムは、互いに依存しない

| プログラム | 役目 | 依存（`go list -deps`） |
|---|---|---|
| `mamelet` | マシンの中のエージェント（固定のプログラムだけ実行） | `noderev` と `mamelet` だけ。Mamela の核（plan、store、provider）に依存しない |
| `mamelacd` | GitHub の宣言に揃え続ける | `tree`、`api`（局所 API の呼び出し）、`gitops` だけ。plan、承認、apply は `mamela api` に頼む |
| `mamela` | 宣言の評価、plan、承認、apply、Control Plane（`mamela serve`）、局所 API（`mamela api`） | `gitops` を含まない。`mamelet` の設定と seed の作成、`cp`（mamelet に答える HTTP）を含む |

## mamelacd なし: 動く（実機で確かめた）

`mamela plan`、`mamela approve`、`mamela apply` を手で実行すればよい。mamelacd は、その 3 つを `mamela api`（Unix socket）越しに呼ぶ薄い層で、Control Plane は mamelacd の有無を知らない。実機では、mamelacd が動いたままの Control Plane で、手で plan、承認、apply して VM を作れた（42 秒）。mamelacd を止めても、手での運用は変わらない。

失うのは、Git の追従（push が承認）、ずれの検知（`resync`）、失敗のやり直し（`retry`）、commit status への報告である。

## mamelet なし: VM だけなら動く（実機で確かめた。確かめて、穴を直した）

mamelet が要るのは、マシンの中のこと（`NodePackage`、`NodeService`、`NodeK3s`、`NodeK3sManifest`）だけである。マシンそのもの（Proxmox の VM、AWS の EC2）は、provider が API で作るので、mamelet は要らない。

見つけた穴: これまでは、provider が VM を作るとき、cloud-init の seed（ユーザー、SSH 鍵、ネットワーク）が必ず要り、seed は mamelet の導入と一体で、サイト設定に `[controlplane]` が無いと作れなかった。実機で、`[controlplane]` の無いサイトで VM を作ると、`MAM-PRV-0007: ... this Mamela makes no seeds: the site has no Control Plane` で失敗した。何も作られず、guard の後チェックは境界の外が変わっていないと言った（安全には失敗した）。

直した: `[controlplane]` の無いサイトでは、**素の seed**（ログインのユーザーと鍵、ネットワーク、guest agent だけ。mamelet の binary、設定、参加用トークン、unit は入れない）を作る。トークンも DB も要らない。VM は作られ、起動し、宣言したアドレスを持つ。mamelet が要る宣言（`Node*`）を同じサイトで plan すると、そのリソースは「エージェントの報告を待つ」と出て、止まる（`MAM-PLAN-0103`、15 分を超えると `MAM-PLAN-0110`）。

## 組み合わせ

| 構成 | できること | 要るもの |
|---|---|---|
| mamela だけ（手で plan、承認、apply） | VM などの provider のリソース、明示操作 | provider、サイト設定（`[controlplane]` なしでよい） |
| mamela + mamelet | 上に加えて、マシンの中（package、service、k3s、manifest） | `[controlplane]`、`mamela serve`、各マシンの mamelet |
| mamela + mamelacd | Git に揃え続ける（push が承認、ずれの検知、やり直し） | `mamela api`、GitHub のトークン |
| 全部 | 今の検証環境 | |

## 分けるために残っていること

- mamelet の更新の仕組みがない（Control Plane が配る seed の binary を差し替えても、既存のマシンは古いまま。手で入れ替える）。[k3s-operations](k3s-operations.md) の直し方の表を参照。
- provider（mamela-proxmox）の plan の文言は、`[controlplane]` が無くても「mamelet seed」と言う（素の seed でも、この文言は変わらない）。
