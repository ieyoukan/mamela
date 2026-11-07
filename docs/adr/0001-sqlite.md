# ADR 0001 — Control Plane の State Store に SQLite を採用

- 状態: Accepted for MVP
- 日付: 2026-11-08
- 対象: Mamela v0.1 構想

## 文脈

Mamela は Kubernetes に近い Reconciliation を行うが、MVP は単一 Go Control Plane と少数の Proxmox VM を管理する。保存対象は Desired、Observed、Revision、Plan、Operation、Agent、Event。初期段階では高可用性 Control Plane や多ノード同時 writer は不要。

## 決定

**SQLite を Control Plane の組み込み永続ストアとして採用する。**

- WAL モードとトランザクションを使用する。
- SQLite ファイルは同一ホストの信頼できる永続ディスクに置く。
- DB 更新競合を防ぐため resourceVersion/CAS と Operation Journal を使う。
- 複数 Reconciler を使う場合もプロセス内・DB 側の排他を明示する。
- マイグレーション、バックアップと復元の確認を実装要件とする。
- 外部 Provider の操作とは原子的にコミットできないため、Intent の先行記録と再観測で回復する。

## 理由

単一バイナリで開始しやすく、DB サーバー運用を増やさずに安全性・収束ロジックの検証に集中できる。JSON 形式の Spec、トランザクション、索引、SQL クエリで MVP の要件を満たせる。

## 見送った案

- PostgreSQL: 分散した複数 Control Plane や高度な運用要求が生じたら再評価するが、現時点では外部 DB の導入負荷が大きい。
- etcd: 強い整合性と Watch に優れるが、運用/実装負荷が高く、監査履歴・Operation Journal の保存に適しているとは限らない。

## 制約と将来

SQLite ファイルを NFS などで共有する HA を行わない。Watch 通知は初期段階ではアプリケーション内イベントと定期再同期で実現する。複数 Control Plane が必要になったときは、DB だけでなく Lease、リーダー選出、監査ログの整合性も含めて再設計する。現時点で移行先は固定しない。
