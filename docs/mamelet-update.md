# mamelet の更新

mamelet（各マシンのエージェント）の版は、宣言で固定し（`NodeMamelet`、[graph-schema](graph-schema.md)）、plan に出し、承認して更新する。k3s の版と同じ形である。

## 手順

1. 新しい mamelet を作る（`scripts/build-mamelet.sh`。linux/amd64）。
2. Control Plane のホストの `controlplane.mamelet_binary`（サイト設定）に置き、`mamela serve` を再起動する（配るのはこのファイル）。
3. その sha256 を宣言に書く（`Mamela.mamelet(graph, node, "0.9.1", "<sha256>")`）。ノードごとに 1 つ。
4. plan を見る。ノードごとに `update-mamelet`（承認が要る）が出る。ノードは **1 台ずつ、名前の順** で、前のノードが宣言した版で動くまで、次は待つ（MAM-PLAN-0120）。
5. 承認して apply する（`sync = "push"` なら push が承認）。

先に新しい Control Plane（`mamela`、`mamela serve`）にしてから、mamelet を更新する（新しい mamelet の報告は、古い Control Plane が読めない）。

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
