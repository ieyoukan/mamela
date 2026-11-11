# ADR 0006 — Mamela 本体から Proxmox を出し、安全装置をプロバイダーの契約にする

- 状態: Accepted（2026-11-11）。実装済み（2026-11-11。1–5 を実装し、Mamela 本体から Proxmox のコードを削除した）
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
- **記録の片付け:** 削除が済めば、作成の記録を消す。Mamela が振ったノード番号は消さない。番号を別のマシンに振らないためである（同じ名前のマシンを作り直せば、同じ番号に戻る）。
- **Proxmox での操作:**
  - `delete-vm`（止めてから削除）
  - `reboot-vm`
  - `reset-vm`
  - `adopt-vm`: 旧組み込みプロバイダーの印を持つ VM を引き継ぐ。移行のため。

### 3. root で実行するスクリプトは、プロバイダー（WASM）が書く

pool、ユーザー、Token、ACL を作るスクリプトは、基盤ごとに違い、Mamela の Token では実行できない（人が root で実行する）。これらは、プロバイダーが書く。

- **宣言:** マニフェストの `scripts` に、名前と説明（何をするか、どこでだれが実行するか）を書く。
- **呼び出し:** `script` の呼び出しで、名前と設定を渡すと、本文が返る。
  - 渡すもの: `[provider.X.guard]` と、資格情報の ID と秘密のファイルの場所。秘密そのものは渡さない。
  - host は、この間の要求をすべて拒む。本文は UTF-8 で 1 MiB まで。
- **実行:** `mamela provider script <pome-dir> <provider> [<script>]`。宣言の依存（Pome.lock で固定）からプロバイダーを見つけて動かす。使う人に必要なのは Mamela だけである。
- **Proxmox での中身:**
  - `bootstrap`: pool、ユーザー、Token、ACL を作る。権限は、guard が確かめるのと同じマニフェストの needs から導く。
  - `permissions`: 権限と、それが要る操作の説明。
  - `cleanup`: 使われなくなったアップロードの一覧。`--free` のときだけ消す。
- **権限カタログ:** Proxmox の権限カタログ（各呼び出しに要る権限）は `mamela-proxmox` に移し、マニフェストの needs がそれを満たすことを試験で確かめる。

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

## 実装で足したこと

決定の本文になかったが、実装で足したもの。

- **遅い変更（`late`）:** 変更（`Change`）に `late` を足した。`late` の変更は、その apply のほかのすべての変更の後に実行する。組み込みのプロバイダーが持っていた「全台を作ってから起動する」順番を、プロバイダーが印で示す（Proxmox ではマシンの起動）。順番を変えるだけで、何も許可しない。reconcile からは Proxmox の操作名がなくなった。
- **明示操作の記録:** `mamela act` は、送る前に意図を SQLite の `explicit_operations`（store の migration 10）に記録し、結果（done、failed、partly-done、unknown）と確認した人も記録する。結果が分からないときは lookup で確かめ、やみくもに繰り返さない（MAM-ACT-0201）。引き継ぎが済めば作成の記録を足し、データを失う操作が済めば作成の記録を消す。拒否のコードは MAM-ACT-0100–0106。
- **guard を呼ばないとき:** plan を作ったときとプロバイダーの契約（プログラム、API の接続先・資格情報・カタログ）が違う apply は、guard も呼ばない。reconciler が、何にも触れずに MAM-REC-0007 で止める。
- **controlplane bundle:** Proxmox のトークンの代わりに、ホスト用に書き換えた `mamela.providers.toml` と、その秘密（`/etc/mamela/secrets/<provider>-token` と `<provider>-guard-token`）を入れる。
- **site のコード:** site の規則に合わない変更は、`LAB-0304`・`LAB-0305` の代わりに `MAM-SITE-0304`（ネットワーク）・`MAM-SITE-0305`（1 台あたりの予算）になった。VM の操作とともになくなった policy のコード（MAM-POL-0201・0202・0203・0211・0301・0302・0307）と MAM-GRAPH-0105 は、使い直さない（[reason-codes.md](../reason-codes.md)）。Proxmox の guard のコード（`PVE-*`）は mamela-proxmox の README にある。
