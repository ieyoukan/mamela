# mamelet の更新

mamelet（各マシンのエージェント）の版は、宣言で固定し（`NodeMamelet`、[graph-schema](graph-schema.md)）、plan に出し、承認して更新する。k3s の版と同じ形である。

## 手順

1. 新しい mamelet を作る（`scripts/build-mamelet.sh`。linux/amd64）。
2. Control Plane のホストの `controlplane.mamelet_binary`（サイト設定）に置き、`mamela serve` を再起動する（配るのはこのファイル）。
3. その sha256 を宣言に書く（`Mamela.mamelet(graph, node, "0.9.1", "<sha256>")`）。ノードごとに 1 つ。
4. plan を見る。ノードごとに `update-mamelet`（承認が要る）が出る。ノードは **1 台ずつ、名前の順** で、前のノードが宣言した版で動くまで、次は待つ（MAM-PLAN-0120）。
5. 承認して apply する（`sync = "push"` なら push が承認）。

先に新しい Control Plane（`mamela`、`mamela serve`）にしてから、mamelet を更新する（新しい mamelet の報告は、古い Control Plane が読めない）。

0.3.2 から、許可（承認した操作を mamelet に許す印）には期限がある。Control Plane を更新する時点で進行中だった apply の許可（期限を持たない）は配られなくなるので、その apply は結果不明で止まる。更新のあとにもう一度 apply する。revision の形は変わらない（期限は Control Plane の中だけにあり、署名する内容に入らない）ので、古い mamelet はそのまま revision を読める。

## 更新の中身（mamelet が一度だけ行う）

1. Control Plane から binary を取る（mamelet がもともと信頼している CA の TLS）。承認した sha256 と違えば、何も変えずに失敗する。
2. `mamelet.next` に書き、`-version` で動かす。この機械で動かない binary は、何も変えずに失敗する。
3. 動いている binary を `mamelet.prev` に残し、新しいものを置く。
4. 結果を報告してから、新しい binary に再起動する（exec）。
5. 新しい mamelet が、1 回報告できたら、更新は確かめられたことになる（`mamelet.prev` は残る）。

## 戻る

新しい mamelet が、次のどれかなら、`mamelet.prev` を戻して再起動する。

- 報告できないまま、起動が 3 回を超えた。
- 10 分たっても報告できない（Control Plane に届かないなど）。
- 再起動（exec）に失敗した。

戻ると、その操作の結果は「失敗（戻した）」になり、次の plan は、また更新を出す（承認が要る）。ノードは 1 台ずつなので、ほかのノードは待つ。

## 限界

- **更新の仕組みより前の mamelet は、一度だけ手で置き換える。** 古い mamelet は、`mamelet` の許可を含む revision を、知らない項目として断る。`/usr/local/sbin/mamelet` を置き換えて `systemctl restart mamelet` する（検証環境では `/tmp/vm-update.sh` のような手順）。これ以降は、宣言で更新できる。
- **新しい mamelet が、起動の前に壊れるとき**（ELF が壊れているなど）は、`-version` で見つかる。起動はするが、すぐ落ちるときは、3 回の起動で戻る。起動して動き続けるが、何かを間違えて報告するときは、戻らない（plan が違いを見つけて、また更新を出す）。
- **Control Plane のホスト**（保護されたノード）の mamelet は、plan では更新しない（ADR 0003）。手で置き換える。
- **種類の違う Control Plane（古い）** は、`/v1/nodes/<node>/wait` を知らない。mamelet はその場合も動く（今までの間隔）。

## 実機で確かめたこと（2026-11-16、検証環境の 3 台、push で同期）

| 試したこと | 結果 |
|---|---|
| 0.9.0 を宣言（3 台とも 0.9.0 で動いている） | 「0 changes」（in sync）。動いている binary の sha256 を報告している |
| 配る binary を 0.9.1 にして、宣言の sha256 を 0.9.1 に変える（push） | plan が「1 changes、2 waiting」（1 台目だけが更新で、あとは待ち）。push で承認、apply。agent-1、agent-2、server の順に **1 台ずつ**（10:31:40、10:31:55、10:32:09）。最後の plan は「0 changes」。push から 98 秒（最初の検知の 58 秒を含む） |
| 更新のあと | 3 台とも `mamelet -version` が 0.9.1、`mamelet.prev` が残っている、サービスは active。k3s と ArgoCD には影響なし |
| **戻る**（agent-2 の状態を、新しい mamelet が期限までに報告できなかった状態にして、mamelet を再起動する。実機で自然に起こすのは難しいので、状態は手で作った） | 0.9.1 が、起動時に更新が期限切れなのを見て、`mamelet.prev`（0.9.0）を戻して再起動した。更新は終わり、操作の結果は「失敗（went back to the previous mamelet）」と記録された |
| 戻ったあとの plan | agent-2 だけが「更新（承認が要る）」。ホストで承認して apply すると 0.9.1 に上がった |

試していないこと: 新しい mamelet が、起動してすぐ落ちる（3 回を超える起動で戻る）、起動はするが Control Plane に届かない（10 分で戻る）の、自然な発生。単体テストでは確かめてある。
