# 既存のマシンを、作り直さずに Mamela の管理下に移す（計画、2026-11-22）

> 実装した（feat/observe-only と mamela-proxmox の feat/lxc）: 下の「作るもの」の 1〜5。検証環境で確かめた結果は「検証環境での試験」にある。
>
> 実装した（feat/enroll-existing）: 7（enroll）。手順は「既存のマシンに mamelet を入れる（enroll）」にある。

人が手で作った Proxmox のゲスト（LXC と VM）と、その中で動くもの（k3s の HA の control plane など）を、作り直さずに Mamela の管理下に移す。この文書はその計画である。

前提を 2 つ置く。

- **ほかの道具は管理していない。** Terraform（OpenTofu）の state になく、あっても古くて使っていないものとする。そのため `mamela import terraform`（ADR 0009）は使わない。残っている Terraform の設定がそのゲストを apply しないことだけは、先に確かめる（自動の apply を止める、など）。2 つの道具が同じゲストを変えないためである。
- **クラスタは共有の本番である。** 境界の外のゲストは読むだけにする（mamela-proxmox の guard）。

## 安全の約束

段ごとに、何が変わりうるかを先に決める。それを人の注意ではなく、Mamela の仕組みで強制する。

| 段 | 変わりうるもの | 強制する仕組み |
|---|---|---|
| 1. 見る | 何も変わらない | Token に読む権限しかない。宣言は引き継ぐ対象（MAM-PLAN-0114）なので、plan に操作がない |
| 2. 引き継ぐ | 説明（description）の末尾の印と、タグの追加だけ | 引き継ぎは明示操作（`mamela act --confirm`）。observe-only のあいだ、ほかの操作はすべて拒否される |
| 3. 中を見る | mamelet を入れること（人が実行する） | observe-only のノードの mamelet には、許可（grant）が届かない |
| 4. 任せる | 宣言した値だけ | 承認した plan だけを実行する。宣言しない値は管理しない |

どの段でも、引き継いだものは削除しない。宣言から外しても残す（ADR 0013 の削除は、Mamela が作ったものに限る）。消すときは、人が手で消すか、プロバイダーの明示操作を使う。

## 作るもの

| # | もの | 場所 | 状態 |
|---|---|---|---|
| 1 | **observe-only**: policy の `observeOnly`、または `mamela.site.toml` の `observe_only`。名前を挙げたマシンとリソースについて、Mamela は観測と plan はするが、何も変えない。変更は表示して拒否し（MAM-POL-0312）、mamelet には許可を送らない。明示操作は、引き継ぎだけを許す | mamela | 今回 |
| 2 | **宣言の ID による引き継ぎ**（`kit.Adopter`）: 宣言が既存の物を ID（VMID）で名指しすると、import の記録がなくても、その物を引き継ぐ対象として観測する。Mamela の印を持つ物は対象にしない | mamela（provider kit） | 今回 |
| 3 | **引き継ぐ前のプレビュー**（`DiffResult.Previews`）: 引き継ぐ対象の物について、引き継いだあとに何が変わるかを plan に示す。承認も送信もしない | mamela | 今回 |
| 4 | **引き継いだものを消さない**: 引き継いだものは、宣言から外しても削除の対象にしない（今は、引き継いだ VM が、Mamela の作った VM と同じく削除されうる） | mamela | 今回 |
| 5 | **ProxmoxLXC**: 既存の LXC を引き継ぎ（`adopt-lxc`）、宣言した値（cores、memory、swap、rootfs の拡張、onboot、電源）だけを管理する。説明とタグは、書き換えずに追記する。コンテナはまだ作らない | mamela-proxmox | 今回 |
| 6 | **境界に名指しで足す**: guard の設定に、引き継ぐ既存のゲスト（node と VMID）の一覧を足す。pool と VMID の範囲の外にあるゲストでも、名指ししたものだけを境界に入れる。Token の ACL も、そのゲスト（`/vms/<vmid>`）だけに足す。段 1 では `VM.Audit` だけ、段 2 で変更の権限を足す（bootstrap が書く） | mamela-proxmox | 次 |
| 7 | **既存のマシンへの mamelet の導入**（enroll）: Control Plane が 1 回限りの token を発行し、人が読んでから root で実行するスクリプトを書く（`mamela controlplane enroll`）。Control Plane が自分でマシンに入ることはしない。LXC では `pct push` と `pct exec`、VM では SSH で実行する | mamela | 実装した |
| 8 | **ProxmoxVM の、宣言の VMID による引き継ぎ**（Terraform なし）。今の `adopt-vm` は、import の記録か、旧組み込みプロバイダーの印を前提にしている | mamela-proxmox | 次 |
| 9 | **NodeK3s の HA と引き継ぎ**: embedded etcd の server 3 台。install スクリプトで入れた k3s を読み（unit、`config.yaml`、バイナリの sha256）、宣言と同じなら in sync にする。版上げは 1 台ずつ、etcd の quorum を保ったまま進める（ほかの 2 台が健全なときだけ次へ）。上げる前に etcd のスナップショットを取る | mamela | 次 |
| 10 | **確認が取れなければ戻す**: 適用したあと、決めた時間内に確認（疎通、名前解決など）が取れなければ、前の状態に戻す。締め出しの危険があるゲスト（SSH の経路、DNS）に要る | mamela | 次 |

## ゲスト 1 台の進め方

1. **準備（人）:**
   1. vzdump でバックアップを取る。
   2. ゲストの名前を `observe_only` に入れる（Control Plane のホストで）。
   3. ゲストを境界に名指しで足す（6）。Token には `VM.Audit` だけを与える。
2. **見る:**
   1. 宣言を書く。名前、VMID、管理したい値だけを書く。書かない値は管理しない。
   2. `mamela plan` で、引き継ぐ対象として、引き継いだあとに変わることが示される（プレビュー）。
   3. プレビューが空になるまで宣言を直す。ここまで、Proxmox には何も送らない。
3. **引き継ぐ:**
   1. 変更の権限を足す（bootstrap を root で実行し直す）。
   2. `mamela act --confirm <name> adopt-lxc "ProxmoxLXC <name>" <pome-dir>` を実行する。書くのは、説明の末尾の印とタグの追加だけである。
   3. `plan` が in sync になることを確かめる。observe-only のままなので、以後も何も変えない。
4. **中を見る:**
   1. enroll（7）で mamelet を入れる（下の「既存のマシンに mamelet を入れる」）。
   2. パッケージ、サービス、ファイル、ユーザーを宣言する。
   3. plan で差を見る。差は拒否され、mamelet には許可が届かない。宣言を直して in sync にする。
5. **任せる:**
   1. `observe_only` から外す（Control Plane のホストで）。
   2. 最初の変更は小さなもの 1 つにし、結果を確かめる。

## 既存のマシンに mamelet を入れる（enroll）

Mamela が作ったマシンは、seed（cloud-init）で mamelet を入れて登録する。既存のマシンには seed がないので、Control Plane のホストで**スクリプト**を書き、人がそれを読んでから、マシンの上で root で実行する。Control Plane は、マシンに入らない（SSH の鍵も、Proxmox のホストの root も持たない）。

**スクリプトを書く（Control Plane のホストで、DB の所有者として）:**

~~~bash
sudo -u mamela-cp mamela controlplane enroll --site /etc/mamela/site.toml --db /var/lib/mamela/state.db \
  --node <名前> --vmid <VMID> --out <ファイル> [--hostname <マシンのホスト名>] [--ttl 1h] [--policy <policy.json>]
~~~

- **引き継いだマシンだけ。** そのノードに、plan が公開した revision があり、その VMID が `--vmid` と同じこと。revision は、plan がそのマシンを Mamela の物（作った、または引き継いだ）と観測したときだけ公開する（`service.go` の `Managed`）。なければ断る（MAM-PKI-0303。hint: 先に引き継ぐ）。VMID が違えば断る（MAM-PKI-0304）。mamelet が登録済みのノードも断る（MAM-PKI-0305。証明書が切れただけなら `reenroll`。入れ直すなら `--reenroll`）。observe-only のノードにも出せる（mamelet は観測と報告だけをして、許可は届かない）。
- **token:** 1 回限り、そのノードと VMID にだけ効く。既定 1 時間、最長 24 時間（`reenroll` と同じ表と期限）。ファイルを先に作り（mode 0600、既存のファイルには書かない）、そのあとで token を発行する。断ったときは token を出さない。鍵がない Control Plane では、鍵を作らずに断る（MAM-PKI-0201）。
- **中身:** seed と同じもの（`internal/seed` の同じコードで作る）。`/etc/mamelet/config.json`（0600）、`enroll-token`（0600）、`ca.pem`（0644）、`/etc/systemd/system/mamelet.service`（0644）、`/usr/local/sbin/mamelet`（0755）。バイナリは、クラウドの seed と同じく、Control Plane の `/v1/mamelet/<sha256>` から https で取り、Mamela の CA で検証し、sha256 を確かめてから入れる（ISO の seed は CD-ROM を mount するので、LXC では使えない）。秘密は token だけである（CA の証明書と revision の公開鍵は秘密ではない）。APT の origin は、`--policy` の `allowedAptOrigins`（なければ既定の `Ubuntu:`）。Debian のマシンでは `Debian:` と `Debian-Security:` を policy に足す。
- **出力:** 次にすること（LXC と VM の経路）、マシンが Control Plane に届く必要があること、登録の見方。

**スクリプトがすること（POSIX sh、`set -eu`）:**

1. 確かめる。root であること、ホスト名（`uname -n`）が `--hostname`（既定はノードの名前）と同じこと、systemd で動いていること、mamelet がまだないこと（`/etc/mamelet`、unit、バイナリのどれかがあれば断る。`--reenroll` を付けたときだけ置き換える）、curl があること。
2. ファイルを一時ディレクトリに書き、mamelet を取ってきて sha256 を確かめる。ここまでに失敗すれば、マシンは何も変わっていない。
3. 入れる。`systemctl daemon-reload`、`systemctl enable --now mamelet`。
4. 登録を 1 分まで待つ（mamelet が証明書を得て、token のファイルを消す）。確かめ方を表示する。

パッケージ、ユーザー、ネットワークの設定は変えない。

**実行する（例）:**

~~~bash
# LXC: スクリプトを Proxmox のホストに写してから、ホストで
pct push <VMID> <ファイル> /root/mamelet-enroll.sh --perms 0600
pct exec <VMID> -- sh /root/mamelet-enroll.sh
pct exec <VMID> -- rm /root/mamelet-enroll.sh
# VM: root になれる利用者で
scp <ファイル> <user>@<address>:mamelet-enroll.sh
ssh -t <user>@<address> 'sudo sh mamelet-enroll.sh && rm mamelet-enroll.sh'
~~~

**確かめる:** スクリプトが「enrolled as <名前>」と表示する。マシンで `journalctl -u mamelet`（「enrolled as」のあとに「connected to the Control Plane」）。Control Plane のホストで `mamela controlplane agents`（ノードが並ぶ）。次の `mamela plan` に、mamelet の観測が出る。終わったら、スクリプトを両方で消す。

**unprivileged の LXC（Debian 13）で確かめたこと（コードを読んで）:**

- mamelet は、DMI、cloud-init、qemu-guest-agent、カーネルのモジュールを使わない。実行するのは `/usr/bin/dpkg-query`、`/usr/bin/apt-get`、`/usr/bin/systemctl`（と拡張の `useradd` などと k3s）だけで、Debian 13 にある。自分の更新は `/proc/self/exe` と `exec` で、LXC でも動く。
- mamelet の unit は、名前空間を使う設定（`PrivateTmp`、`ProtectSystem` など）を持たない。nesting のないコンテナでも起動する（`internal/seed` の試験が、この種の設定を足すと失敗する）。パッケージが入れるサービスには、nesting（`features: nesting=1`）が要るものがある。
- スクリプトは `mount` を使わない（unprivileged のコンテナでは AppArmor に止められる）。
- コンテナの中から VMID は分からない。違うマシンで実行するのを防ぐのは、ホスト名の確かめと、token が名前と VMID に結び付いていることである。
- Proxmox の Debian のテンプレートに curl がないことがある。スクリプトは何も変えずに断り、`apt-get install curl` を示す。
- 既定の policy の APT origin は `Ubuntu:` なので、Debian のパッケージは入らない（plan は MAM-POL-0305、mamelet も断る）。policy と mamelet の両方に `Debian:` を足す（スクリプトは `--policy` の値を mamelet の設定に書く）。
- k3s をコンテナで動かすことは、ここでは扱わない（k3s 自体が privileged などを要する）。linux 拡張で UID を宣言するときは、unprivileged のコンテナでは 65535 までに収める。

## 順番

危険の小さいものから進める。

1. 止まっているもの、なくても困らないもの。
2. 一般のサービス。
3. k3s の worker。
4. k3s の server。1 台ずつ、etcd の quorum を確かめながら進める（9 のあと）。
5. 締め出しの危険があるもの（SSH の経路、DNS、CI のランナー）。戻す仕組み（10）ができてから。

具体的なゲストと順番は、公開しない宣言のリポジトリに書く。

## 検証環境での試験（今回、確かめた）

検証用の pool と VMID の範囲の中に、手で LXC を 1 つ作った（Ubuntu 24.04、cores 1、memory 512 MiB、rootfs 4 GiB、説明とタグ付き）。Mamela が作っていない既存のゲストを模すためである。Control Plane と mamelacd を新しい版に替えたうえで、次をすべて確かめた。既存の宣言（k3s と Web）は、替えたあとも in sync のままだった。

| 確かめること | 結果 |
|---|---|
| VMID を名指しした宣言 | 引き継ぐ対象（MAM-PLAN-0114）になり、プレビューに差が出た。何も送らない |
| observe-only での引き継ぎ | 説明とタグに追記するだけ。もとの説明とタグは残り、再起動しない |
| observe-only のあいだの差 | 表示され、拒否される（MAM-POL-0312）。承認しても何も送らない |
| observe-only を外したあとの cores、memory、swap の変更 | 再起動せずに効く |
| rootfs の拡張 | 広がる。縮める宣言は拒否される |
| 境界の外の VMID、別の宣言の印があるゲスト、VM の VMID | 引き継ぐ対象にならない |
| apply の前後の guard | 境界の外のゲストが変わっていない（PVE-0400） |
| 電源（止める宣言、動かす宣言） | `shutdown-lxc`、`start-lxc` で、宣言どおりになる |

実際の様子:

1. 名指しした宣言（管理する値なし）: plan は「引き継ぐ対象、引き継いでも何も変わらない」。値を宣言すると、プレビューに rootfs 4 → 6 GiB、cores 1 → 2、memory 512 → 1024 MiB、swap 512 → 256 MiB が出た。
2. observe-only で `adopt-lxc`: 説明の末尾に印の行が 1 行足され、タグは、もとのタグに site のタグが加わった。もとの説明とタグは残り、稼働時間は途切れなかった。
3. observe-only のあいだ: plan は `[DENIED: observe-only]`。承認しても apply は「held: observe-only」として何も送らず、`reboot-lxc` は MAM-POL-0312 で断られた。
4. observe-only を外す: 1 回目の plan で rootfs を広げ、2 回目で cores、memory、swap を変えた。コンテナの中で、CPU 2、メモリ 1024 MiB、スワップ 256 MiB、ルート 5.9G になり、起動からの時間は続いていた（再起動していない）。3 回目の plan は in sync。
5. 拒むもの: 縮める宣言は MAM-POL-0309、VM の VMID を LXC として宣言すると「VM であり、コンテナではない」、同じコンテナを別の名前で名指しすると observe が失敗して理由を示した。

見つけた小さなこと:

- 引き継いだあとの plan は、digest を前提にする変更を 1 回に 1 つしか出さないので、rootfs を広げる plan に残りの差が出なかった。理由に「次の plan で」として並べるようにした（mamela-proxmox）。
- 電源の操作の直後のまとめは、Proxmox のクラスタの一覧が更新される前の状態を見ることがある（次の plan では in sync）。
- MAM-PLAN-0114 の hint は、引き継ぎの操作の名前を `<adopt-op>` としか書かない。プロバイダーのマニフェストから名前を出せる。

## まだ決めていないこと

- k3s の HA の宣言の形。server の 1 台目（`cluster-init`）と、2 台目以降の join をどう表すか。今の NodeK3s は server 1 台を前提にしている。
- 境界に名指しで足したゲストを、あとで境界から外す手順。印とタグを消して、Foreign に戻す。
