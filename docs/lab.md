# Mamela — 実機での検証（Lab）

> 2026-11-11（ADR 0006）。この文書は公開リポジトリにあるため、サイト固有の値（ホスト名、IP、既存のゲストやインスタンスの一覧、トークン）は書かない。それらは gitignore した `mamela.site.toml`、`mamela.providers.toml`、`lab/local/` に置く。

Mamela 本体は、特定の基盤を知らない。実機での検証の安全装置（適用の前後の検査、削除や再起動、root で実行する準備のスクリプト）は、それぞれのプロバイダーが書き、Mamela が強制する。考え方は [ADR 0006](adr/0006-proxmox-out-of-core.md) にある。

**Proxmox の検証環境の手順は、[mamela-proxmox](https://github.com/ieyoukan/mamela-proxmox) の `docs/lab.md` にある。** 境界（pool、VMID の範囲、タグ）、bootstrap、guard の設定、旧組み込みプロバイダーの VM の引き継ぎは、そちらに書く。

## 1. 原則

- **明示の opt-in:** 実機の基盤やパッケージを変えるテストは、`MAMELA_LAB=1` を明示したときだけ動かす。CI の標準ジョブでは動かさない。
- **guard のあるプロバイダーだけ:** 共有の本番環境（ほかのワークロードが動いている基盤）で試すときは、guard を持つプロバイダーだけを使う。guard のないプロバイダーでは、適用の前後に何も確かめられない。
- **境界の外は読むだけ:** プロバイダーの境界の外にあるもの（Mamela が作っていないゲストやインスタンス）は `Foreign` として扱い、変えない。
- **公開しない:** サイト固有の値、既存のゲストの一覧、トークンをコミットしない。ログやテストの fixture にも残さない。
- **緩めるときはレビュー:** 予算や guard のしきい値を緩める変更は、理由を書いて人がレビューする。

## 2. 置き場所

| ファイル | 中身 | コミット |
|---|---|---|
| `mamela.site.toml`（`--site`、`MAMELA_SITE`） | Mamela の site（`mamela-site/v1`）: Control Plane、マシンのログインユーザーと SSH 公開鍵のファイル、ネットワークの規則（DHCP、固定アドレスの範囲）、1 台あたりの予算 | しない（雛形は `mamela.site.example.toml`） |
| `mamela.providers.toml`（`--providers`、`MAMELA_PROVIDERS`） | プロバイダーごとの API の URL、資格情報（`token_id`、`token_file`）、guard の資格情報（`guard_token_id`、`guard_token_file`）、guard の設定（`[provider.<name>.guard]`）、カタログ | しない |
| `lab/local/` | 実アドレスを含む検証用の宣言 | しない |

トークンの秘密そのものは、どのファイルにも書かない。リポジトリの外のファイルに置き、`chmod 600` にする（グループや他人が読めるファイルは拒否する）。

## 3. 手順

### 3.1 準備（人が実行）

プロバイダーが Mamela に使わせるユーザー、トークン、権限を作るには、Mamela が持たない権限（root など）が要る。そのスクリプトはプロバイダーが書く。Mamela は表示するだけで、実行しない。

~~~bash
mamela provider script <pome-dir> <provider>              # そのプロバイダーが書くスクリプトの一覧
mamela provider script <pome-dir> <provider> <script>     # 本文を表示する。人が読んでから実行する
~~~

- プロバイダーは、宣言の `Pome.lock` で固定した版を使う。使う人に必要なのは Mamela だけである。
- スクリプトは `mamela.providers.toml` の秘密でない値（guard の設定、資格情報の ID と秘密のファイルの場所）から書かれる。秘密そのものはプロバイダーに渡らない。スクリプトを書く間、プロバイダーは API に要求を送れない。

### 3.2 適用の前に: `mamela guard`

~~~bash
mamela guard <pome-dir>
~~~

宣言が使うプロバイダーのうち guard を持つものが、適用の前の検査（`before`）だけを行う。読み取りだけで、guard 用の資格情報（`guard_token_id`）があればそれを使う。block が 1 つでもあれば終了コード 1 になる。そのまま apply しても、何も送られない。

### 3.3 plan・approve・apply

~~~bash
mamela plan <pome-dir>          # 観測して計画し、保存する（読み取りだけ）
mamela approve <plan-digest>    # その計画だけを承認する
mamela apply <plan-digest>      # guard（前）→ 再観測して実行 → guard（後）
~~~

apply は、変更の前後でプロバイダーごとに次を行う。

1. **guard（前）:** その plan のうち、そのプロバイダーが実行しうる変更（Denied は除く）を渡して検査する。block が 1 つでもあれば、何も送らずに終わる。
2. **実行:** 観測し直し、承認した変更と完全に同じ変更だけを、許可（permit）のもとで送る。`late` の変更（例: マシンの起動）は、ほかのすべての変更の後に送る。
3. **guard（後）:** 前の検査の結果（`baseline`）と比べる。実行が途中で止まっても、必ず行う。block があれば apply は失敗する。次の apply の前に、原因を人が調べる。

plan を作ったときとプロバイダーの契約（プログラム、API の接続先・資格情報・カタログ）が違うときは、guard を呼ばない。reconciler が何も送らずに MAM-REC-0007 で止める。

guard の結果のコードはプロバイダーが決める（Proxmox は `PVE-*`。[reason-codes.md](reason-codes.md)）。

### 3.4 削除・再起動・引き継ぎ: `mamela act`

宣言から消しても、実物は消えない（plan に削除は現れない）。削除や再起動は、プロバイダーが明示操作として宣言したものだけを、人が名前を打ち直したときだけ行う。

~~~bash
mamela act <op> <resource> <pome-dir>                         # 送る要求を表示するだけ（何も変えない）
mamela act --confirm <resource> <op> <resource> <pome-dir>    # 名前を打ち直したときだけ実行する
~~~

- 対象は、Mamela が作ったと記録したものだけである。引き継ぎ（adopt）の操作は、プロバイダーが `adoptable` と報告したものだけが対象になる。Control Plane のホスト（site の `controlplane.host`）は対象にならない（MAM-POL-0308）。
- `--confirm` のときは、次の順に進む。
  1. guard（前）。block があれば何もしない。
  2. 意図を記録する（SQLite の `explicit_operations`）。
  3. その 1 つの変更だけの許可で送る。
  4. 結果を記録する。データを失う操作（削除）が済めば、作成の記録を消す。引き継ぎが済めば、作成の記録を足す。
  5. guard（後）。
- 結果が分からないときは、繰り返さない。`--confirm` なしでもう一度実行し、今の状態を見る（MAM-ACT-0201）。
- コードは [reason-codes.md](reason-codes.md#mam-act--明示操作mamela-act) にある。

### 3.5 後片付け

Mamela の権限で消せないもの（アップロードしたイメージなど）を片付けるスクリプトも、プロバイダーが書く（例: Proxmox の `cleanup`）。3.1 と同じく、`mamela provider script` で表示し、人が確かめてから実行する。

## 4. ネットワークと予算（site）

どのプロバイダーのマシンにも、site の同じ規則が効く。

~~~toml
[network]
dhcp = true                  # Dhcp を宣言したマシンを許す
static_first = "10.0.0.40"   # 固定アドレスに使ってよい範囲（両端を含む。架空の値）
static_last = "10.0.0.49"

[budget]
max_vcpu_per_vm = 2
max_memory_mib_per_vm = 4096
max_disk_gib_per_vm = 20
~~~

- **範囲の決め方:** LAN の DHCP サーバーの払い出し範囲と、使われているアドレスのどちらにも重ならない範囲にする。Mamela はルーターの設定を読めないので、範囲が空いていることは人が確かめる。
- **範囲の外は plan で止める**（MAM-SITE-0304）。apply でも、seed を作る前にもう一度確かめる。
- **予算を超える変更は plan で止める**（MAM-SITE-0305）。apply でも、送る前にもう一度確かめる。
- **公開リポジトリに実アドレスを書かない。** 固定アドレスを使う宣言は、gitignore した `lab/local/` に置く。

## 5. Control Plane を専用のホストに移す（ADR 0003）

常用するときは、Control Plane（`mamela serve`）を LAN の中の常駐ホストに移す。考え方は [ADR 0003](adr/0003-controlplane-host.md) にある。

~~~bash
# 手元で
mamela plan <宣言>                    # site に controlplane.move_to を書いてから。全ノードが revision を取りに来るのを待つ
mamela controlplane bundle --url https://<ホストのアドレス>:8443 --host <CP のマシン名>
GOOS=linux GOARCH=amd64 go build -o .mamela/bin/mamela-linux-amd64 ./cmd/mamela
scp .mamela/controlplane-bundle.tar .mamela/bin/mamela-linux-amd64 <ホスト>:   # 秘密を含む。コピー後に消す
# ホストで（root）
install -m 0755 mamela-linux-amd64 /usr/local/bin/mamela
mkdir b && tar -xf controlplane-bundle.tar -C b && less b/install.sh && b/install.sh b
systemctl enable --now mamela-serve   # 手元の mamela serve を止めてから
~~~

- **バンドルの中身:** 状態データベースの一貫したコピー、CA と署名鍵、ホスト用に書き換えた site、ホスト用に書き換えた `mamela.providers.toml`（秘密は `/etc/mamela/secrets/<provider>-token` と `<provider>-guard-token`）、mamelet のバイナリ、systemd unit、install.sh。
- **扱い:** バンドルは mode 0600 で書く。秘密を含むので、ホストにコピーした後に消す。
- **ホストでの権限:** serve は、権限のない専用ユーザー `mamela-cp` で動く。
- **移した後の plan と apply:** ホストの上で `--site /etc/mamela/site.toml --providers /etc/mamela/providers.toml --db /var/lib/mamela/state.db` を付けて実行する。CP のマシンとその上のリソースは変えない（MAM-POL-0308）。
