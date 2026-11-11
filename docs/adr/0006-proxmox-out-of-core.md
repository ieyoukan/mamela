# ADR 0006 — Mamela 本体から Proxmox を出し、安全装置をプロバイダーの契約にする

- 状態: Accepted（2026-11-11）
- 日付: 2026-11-11
- 対象: `internal/proxmox`、`internal/provider/pve`、組み込み kind `VirtualMachine`、`internal/lab`、`mamela lab`・`destroy`・`reboot`

## 文脈

Proxmox のプロバイダーは、`github.com/ieyoukan/mamela-proxmox` に出た（ADR 0004）。それでも Mamela 本体には、次の 2 つの形で Proxmox が残っていた。

1. **組み込みのプロバイダー:** `internal/provider/pve` と、組み込み kind の `VirtualMachine`。
2. **lab の安全装置:** 共有の本番クラスタで試すための道具。
   - preflight と postflight（lab の外のゲストが変わっていないことの確認）
   - bootstrap（root で実行する ACL のスクリプト）
   - `destroy` と `reboot`
   - トンネル

   どれも `internal/proxmox` のクライアントと権限カタログを使う。

本体が特定の基盤を知っていると、ほかの基盤（AWS など）では同じ安全を得られない。安全装置は、どのプロバイダーの作者でも、自分の基盤に合わせて書けなければならない。

## 決定

### 1. guard: 適用の前後の安全確認は、プロバイダーが書き、Mamela が強制する

- **マニフェスト:** `guard` に、確認で使う読み取りの呼び出し（`calls`）を宣言する。
- **呼び出し方:** Mamela は、そのプロバイダーの変更を apply する前後に `guard` を呼ぶ。
  - 前（`before`）: 計画の要約を渡す。作る数と、変更の一覧である。
  - 後（`after`）: 前の結果の `baseline` を渡す。
- **結果:**
  - 結果は、検査の結果（`findings`: コード、重さ ok・warn・block、文）と、次に渡す `baseline` である。
  - 前の検査に block が 1 つでもあれば、そのプロバイダーの変更を何も送らない。
  - 後の検査の block は、apply の失敗として報告する。
- **host の強制:**
  - `guard` の間は読み取りしか送らない。
  - 資格情報は、`mamela.providers.toml` に別の読み取り専用のもの（`guard_token_id`、`guard_token_file`）があればそれを使う。Proxmox では、lab の外も見える監査用のトークンである。
  - 設定（`[provider.X.guard]`）は、秘密でない値として、そのままプロバイダーに渡す。
- **単独の実行:** `mamela guard <pome-dir>` で、前の検査だけを実行できる（これまでの preflight）。
- **Proxmox での中身:** guard はプロバイダーが実装する。
  - クォーラム
  - ノード
  - トークンの権限が境界の中にあること
  - メモリとストレージの余裕
  - VM の数
  - lab の外のゲストが変わっていないこと

### 2. 明示操作: 削除や再起動は、plan に入れず、人が名前を打ち直したときだけ行う

- **宣言:** マニフェストの操作に `explicit: true` を書ける。
- **diff との分離:** diff は、明示操作の変更を返してはならない（host が拒む）。
- **実行:** `mamela act <op> <resource>` は次の順に進む。
  1. プロバイダーに `prepare`（その操作の変更を作る。読み取りだけ）を頼む。
  2. 送る要求を表示する。
  3. `--confirm <名前>` があるときだけ、guard の前の検査、許可を発行して apply、guard の後の検査を行う。
- **所有の確認:** 対象は、所有を Mamela の記録で確かめたもの（ADR 0004 の `Vouch`）に限る。
- **policy との関係:** 明示操作は policy の承認の代わりに名前の打ち直しを求める。データを失う効果（lose-data）でも実行できるのは、この経路だけである。
- **記録の片付け:** 削除が済めば、作成の記録とノード番号を消す。
- **Proxmox での操作:**
  - `delete-vm`（止めてから削除）
  - `reboot-vm`
  - `reset-vm`
  - `adopt-vm`: 旧組み込みプロバイダーの印を持つ VM を引き継ぐ。移行のため。

### 3. 管理用の道具は、プロバイダーのリポジトリに置く

root で実行するスクリプトを作る道具は、基盤ごとに違う。Proxmox のものは `mamela-proxmox` の CLI（`mamela-proxmox lab bootstrap`、`cleanup`）に移す。

- bootstrap: pool、ユーザー、トークン、ACL を作るスクリプト
- cleanup: 使われなくなったアップロードを消すスクリプト

権限の一覧は、プロバイダーのマニフェストと Proxmox の権限カタログから導く。カタログも `mamela-proxmox` に移す。

トンネルは、Control Plane を LAN に置く形（ADR 0003）になったので、なくす。

### 4. 本体の設定は汎用のものだけにする

site の定義（`mamela-site/v1`）に残すのは、次のものだけにする。

- Control Plane
- ゲストのログインユーザーと SSH の鍵
- ネットワークの規則（DHCP、固定アドレスの範囲）
- 1 台あたりの上限（vCPU、メモリ、ディスク）

Proxmox の接続先、トークン、pool、VMID の範囲、ストレージ、安全の閾値は、`mamela.providers.toml`（API と guard の設定）と宣言（`Proxmox.Site`）に移す。

### 5. 組み込み kind の VirtualMachine をなくす

- **マシン:** マシンは、すべてプロバイダーの node kind になる（ADR 0004）。
- **Pome:** Mamela の Pome は v0.3.0 で `Vm` と `vm` をなくし、`package`・`service`・`k3s_server`・`k3s_agent` は `Node` を取るようにする。破壊的な変更である。`Network` は残す。
- **例の置き場所:** Proxmox を使う例（homelab、k3s など）は `mamela-proxmox` に移す。

## 結果

- Mamela 本体のコードには、特定の基盤の知識がなくなる。残るのは、Mamela が持つカタログ（`provider/catalogs`）というデータだけである。
- どのプロバイダーの作者も、同じ仕組み（guard、明示操作、作成の記録、上限）で安全を作れる。Proxmox のプロバイダーは、その見本になる。
- **既存の VM:** lab で組み込みのプロバイダーが作った VM は、`adopt-vm` で外部のプロバイダーに引き継ぐ。
