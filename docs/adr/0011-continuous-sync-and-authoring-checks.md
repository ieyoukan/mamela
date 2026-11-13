# ADR 0011 — 継続的な同期と、書いているときに分かる確認

- 状態: Proposed（2026-11-13）
- 日付: 2026-11-13
- 対象: ADR 0010（gitops）、0002（provider の契約）、0008（カタログ）、0009（kit と Pome の生成）、設計案「宣言に追従する削除」

## 文脈

### 目標

- **Argo CD のように運用する:** Kubernetes なしで、Git の宣言に実物を揃え続ける。自動同期は、オンにしたときだけ動く。手動同期は、いつでもできる。
- **どんな AI でも安全に保守できる:** AI がコードを書き、状態を理解して変更を提案できる。それでも、インフラを壊す道はない。

### 既存の IaC を AI に任せたときに起きること

1. **状態を勝手に読み、書き換える:** state ファイルとその backend の資格情報が、AI の作業場所から届く。AI は state を読み、同じ資格情報で CLI からインフラを直接変える。`state rm`・`import`・手での編集で、記録まで書き換える。
2. **権限の不足に apply で気づく:** 実行する資格情報に何が許されているかは、要求を送るまで分からない。apply の途中で止まり、作りかけが残る。
3. **無駄な変更に plan で気づく:** 置き換え・再起動・意図しない差分は、資格情報を持って plan するまで見えない。そのため AI は、plan を見ようとして資格情報を欲しがる（1 に戻る）。

### Mamela にすでにあるもの

- 資格情報と状態は、Control Plane にだけある（ADR 0010）。Mamela には、state を手で書き換えるコマンドがない。所有の記録が変わるのは、apply と、人が `--confirm` する明示操作（`adopt` など）だけである。
- `mamela check` は、資格情報なしで宣言を評価する。各リソースが至りうる操作と、それに要る権限（AWS の IAM アクション、Proxmox の権限）を列挙する。
- provider の `diff` は、観測と宣言から変更を作る純粋な処理である。観測は `observe` が別に行う。
- ホストのカタログは、送る要求ごとに効果（read / create / change / restart / lose-data）を分類する。policy はその効果から、自動・承認・拒否を決める（`autoProviderEffects`）。

### 足りないもの

- **同期を続けること:** 今の gitops は、PR とマージのときだけ動く。マージの後で実物がずれても、誰も気づかない。
- **書く側が、資格情報なしで知る手段:** 「今の実物」と「許されていること」を、書く側（人・AI・CI）が知る手段がない。

## 決定

### 1. 同期の状態と健全性

Control Plane は `interval` ごとに、設定したブランチの先頭を、設定したディレクトリごとに plan する。観測は毎回し直す。結果は二つの軸で表す。

| 同期 | 意味 |
|---|---|
| Synced | 変える物がなく、待ち・拒否もない |
| OutOfSync | 変える物がある（新しいマージ、または実物のずれ） |
| Waiting | 観測や依存を待っている |
| Blocked | 拒否された（policy、guard、権限の不足 → 6） |
| Unknown | plan できなかった（評価の誤り、provider の誤り、API に届かない） |

| 健全性 | 意味 |
|---|---|
| Healthy | 宣言した物がすべてあり、動いている |
| Progressing | 作りかけ、または provider が完了をまだ確かめていない |
| Degraded | provider が「動いていない物がある」と報告した（mamelet の報告が途絶えた、サービスの数が足りない） |
| Missing | 宣言した物がない |
| Unknown | 観測できなかった |

- **provider の健全性:** provider は、観測の結果に健全性（状態と、人が読む 1 行）を任意で付けられる（`Observed.Health`）。付けなければ、存在と作りかけ（`Unfinished`）から決める。
- **残置:** 宣言から外れて残っている物（所有しているが、宣言にない物）は「残置」と表示する。削除の設計（declarative-removal）が入るまで、同期の状態には数えない。
- **見せる場所:**
  - ブランチの先頭のコミットの commit status（`mamela/<dir>`）。
  - ディレクトリごとに 1 つの GitHub issue（「同期の issue」）。Control Plane が作り、本文を今の状態に保つ。
  - Control Plane のホストの `mamela gitops status`。
  - 4 の写し。
- **書く頻度:** 状態が変わったときだけ書く。同じ状態を poll のたびにコメントしない。

### 2. 手動同期（いつでもできる）

自動同期の設定に関係なく、承認者はいつでも同期できる。Argo CD の「Refresh → Sync」と同じく、差分を見てから決める二段にする。

1. 同期の issue に `/mamela sync` と書く。Control Plane は先頭を観測し直して plan し、その plan と digest を issue にコメントする。
2. 承認者が `/mamela approve sha256:<digest>` と書く。宣言はすでにブランチにあるので、すぐに apply する。

- **承認の規則:** PR のときと同じである（承認者に限る、Bot は不可、期限、10 の署名）。
- **CLI:** Control Plane のホストでは、`mamela gitops sync <dir>` で同じことができる。plan を出し、`mamela approve` と `mamela apply` で進める。
- **戻すとき:** 古いコミットに戻すときも Git で行う（revert の PR）。Git にない宣言には同期しない。正は Git である。

### 3. 自動同期（オンにしたときだけ）

ディレクトリごとに設定する。既定はオフである。

```toml
[[sync]]
dir = "prod"
auto = true          # 自動同期。既定 false
self_heal = true     # 実物のずれも自動で直す。既定 false
windows = ["Mon-Fri 09:00-18:00 Asia/Tokyo"]  # 自動で apply してよい時間帯。空ならいつでも
max_applies = "4/h"  # 自動の apply の上限
```

自動同期がオンのとき、先頭の plan は、次をすべて満たすときだけ、人の承認なしで apply する。

1. **すべて自動:** すべての変更の判定が「自動」である。判定は、Control Plane のホストの policy（7）で決める。承認の要る変更が 1 つでもあれば、plan 全体を手動同期に回す。digest は plan 全体に結び付くので、一部だけを実行しない。
2. **provider が同じ:** provider のプログラムと契約が、最後に apply したときと同じである。宣言の依存が新しい provider を持ち込んだら、人が確かめる。
3. **時間帯と上限:** `windows` と `max_applies` の中にある。
4. **失敗の後は止まる:** 直前の自動の apply が失敗していない。失敗したら、そのディレクトリの自動同期は、手動同期が成功するまで止まる。
5. **争わない:** 自動で apply した後に、同じ変更（変更の内容の digest）がまた出ていない。出たら、何か（別の管理者や人の手）が戻していると見て止め、同期の issue で知らせる。Argo CD の self-heal が他の管理者と争い続ける問題を避ける。

何を自動で apply するか:
- **新しいコミットによる変更:** `auto = true` なら、1〜5 を満たせば apply する。PR で `/mamela approve` しなくてよい。PR の plan のコメントは、マージで自動で apply されるか、承認が要るかを書く。
- **実物のずれ（コミットは同じなのに変更が出た）:** `self_heal = true` のときだけ自動で直す。オフなら、OutOfSync を同期の issue で知らせ、手動同期を待つ。

その他:
- **記録:** 承認者は `policy:auto-sync` と policy の digest として残す。人の承認と区別できる。
- **自動同期でも決してしないこと:**
  - 明示操作（削除・再起動・引き継ぎ）
  - 取り込み
  - データを失いうる要求（policy が拒否する）
  - 宣言から消えた物の削除。削除の設計で `deletion_approval = automatic` を明示したときだけ、その設計の規則で行う。

### 4. 状態の写し（view）を、資格情報なしで読めるようにする

Control Plane は plan のたびに、ディレクトリごとの「状態の写し」（`mamela-view/v1`）を公開する。書く側（人・AI・CI）は、これを読んで今の実物を知る。資格情報も、Control Plane への道も要らない。

写しに入れるもの:
- **宣言した物ごと:**
  - 観測（provider が返した `Observed`。秘密の項目は除く → 下）
  - 同期と健全性
  - 待ち・拒否の理由コード
  
  宣言の名前（種類と名前）で引ける。AI はクラウドの ID ではなく、コードの名前で実物を知る。
- **境界の中の、宣言にない物:** 残置・引き継げる物・Foreign（名前と種類だけ）。同じ名前を宣言すると拒否されることが、書く前に分かる。
- **provider:** 各 provider の Pome・版・プログラムの digest、カタログの digest。
- **許されていること:** grants（6）、policy（7）、予算、guard の公開してよい設定（名前の接頭辞、VMID の範囲など）。
- **由来:** 観測した時刻と、元にしたコミット。

規則:
- **写しは正ではない:** Control Plane は写しを読み戻さない。写しを書き換えても、何も変わらない。正の状態は Control Plane の SQLite にあり、それを書き換えるのは apply と明示操作だけである。
- **秘密は入れない:** provider は、spec の項目に `Sensitive` を付けられる。写しでは、その値を伏せる（`"<sensitive>"`）。確認（5）は、伏せた項目の差分を「分からない」と表示する。
- **置き場所:**
  - 既定は、宣言のリポジトリの `mamela/view` ブランチ（`views/<dir>.json`）。別の非公開リポジトリも選べる。
  - 公開リポジトリには書かない（ID やアドレスが載るため）。Control Plane は、書く前にリポジトリが非公開であることを確かめる。
- **トークン:** Control Plane のトークンには、写しを書く先にだけ Contents: write を足す。宣言のブランチは、GitHub の ruleset で PR 経由だけにする。Control Plane のトークンでも直接 push できない。

### 5. 書いているときの確認: `mamela check --view`

```
mamela check --view views/prod.json prod
```

資格情報なし、ネットワークなしで、次を出す。AI の作業場所でも、CI（GitHub Actions）でも、同じように動く。

1. **差分の見込み:** provider の `diff` を、写しの観測で呼ぶ（`observe` は呼ばない。ホストは要求を一つも送らない）。リソースと項目ごとに、次のどれかを出す。
   - 作る
   - その場で変える
   - 明示操作が要る（`resize` など）
   - 変えられない（作り直しが要る。Mamela は計画しない）
   - 宣言から外れる（残置）
   - 変わらない
2. **要求とその効果:** 差分が送る要求を、ホストのカタログで分類して出す。「この変更はインスタンスを止める」が、書いているうちに分かる。
3. **権限:** 各要求に要る権限（manifest の needs）を、写しの grants と照らす。足りなければ誤りにする（新しい理由コード `MAM-CHECK-*`）。「apply の途中で権限が足りない」を、書いているときに知る。
4. **policy:** 各変更が自動・承認・拒否のどれになるか、自動同期で apply されるかを出す。
5. **予算と guard:** provider が API なしで判定できるものを出す。provider の任意の `check` が判定する（kit では、spec と写しの観測だけを見る関数）。
   - 予算の超過
   - 境界の外の名前
   - Foreign と同じ名前
   - 写しの範囲で分かる固定 IP の重複

その他:
- **出力:** 人向けの文と、`--json`（`mamela-check/v2`。今の v1 に、差分・要求・判定を足す）を出す。誤りがあれば、終了コード 1 で終わる。
- **見込みは助言である:** 正は、Control Plane が観測し直して作る plan である。
  - 写しの時刻を表示する。計算そのものは時計を使わない。
  - 写しが古ければ、PR の plan と違いうることを書く。
  - 手元の provider のプログラムが写しのもの（Control Plane のもの）と違えば、警告する。
- **PR の plan も同じ形:** PR の plan のコメントにも同じ形を使う（Control Plane の plan は本物の観測で作る）。書く側は、PR の前と後で、同じ見方で読める。

### 6. 許されていること（grants）を先に知る

provider に任意の RPC `grants` を足す。provider は、自分の資格情報に何が許されているかを API に尋ねる。manifest の各権限に「許可 / 拒否 / 不明」を返す。

- **AWS:** `iam:SimulatePrincipalPolicy`（profile のロールに対して、manifest の IAM アクション）。
- **Proxmox:** `GET /access/permissions`（トークン自身の権限）。
- **mamelet:** 許可は Control Plane が出すので、記録から分かる。

使い方:
- **写しに入れる:** Control Plane は観測のたびに（または `grants_interval` ごとに）尋ね、写し（4）に入れる。
- **plan で止める:** 変更に要る権限が「拒否」なら、plan はその変更を Blocked にする。新しい理由コード（`MAM-PLAN-01xx`）で、何が足りず、どう直すかを出す。承認の前、apply の前に分かる。
- **書いているときに出す:** `mamela check --view` は、5-3 でこれを使う。
- **分からないとき:** `grants` を持たない provider の権限は「不明」とし、確認は警告にとどめる。尋ねても分からないもの（組織の SCP など）も残る。grants は見込みであり、apply のときの API の答えが正である。

### 7. policy は Control Plane のホストにだけ置く

自動同期の判定に使う policy は、Control Plane のホストの設定ファイルから読む。宣言のリポジトリからは読まない。

```toml
# Control Plane のホストの policy（例）
auto_provider_effects = ["create", "change"]  # restart を含む変更は、人が承認する
auto_start_services = ["k3s"]
```

- **AI は policy を広げられない:** AI が PR で policy を広げ、自分の変更を自動にすることはできない。
- **policy を変えると承認は無効:** policy の digest は、今と同じく plan に入る。policy を変えたら、以前の承認は使えない。

### 8. 変更の性質を、書く言葉に載せる

provider の spec の項目（`protocol.Field`）に、変えたときの性質（`Change`）を足す。

| `Change` | 意味 |
|---|---|
| `update`（既定） | その場で変える |
| `explicit:<op>` | plan では変えない。明示操作 `<op>` で変える（インスタンスの型 → `resize`） |
| `fixed` | 作った後は変えられない（作り直しが要る） |

- **生成する型の説明に書く:** `mamela provider pome` は、これを生成する Emela の型の説明に書く（「変えるには `mamela act resize`」）。書く側は、エディターや生成されたコードを読むだけで分かる。
- **kit の `Diff` が守る:** kit の `Diff` は、この宣言に反する差分を作らない。`fixed` の変更は拒否し、`explicit` は明示操作へ案内する。provider ごとに書いていた判定を、spec から一か所で行う。
- **確認でも使う:** `mamela check --view`（5-1）は、これで差分を分類する。

### 9. AI の立ち位置

安全は、AI が指示を守ることに頼らない。AI に何ができないかを、資格情報と承認の置き場所で決める。

| | 人 | AI |
|---|---|---|
| 宣言を書き、PR を出す | ○ | ○ |
| 写し・plan・同期の状態を読む | ○ | ○ |
| `mamela check --view` | ○ | ○ |
| 承認・手動同期 | 承認者だけ | × |
| 明示操作・取り込み | Control Plane のホストで | × |
| 資格情報・SQLite | Control Plane のホストだけ | × |
| policy を変える | Control Plane のホストで | × |

- **状態を書き換える道がない:** Mamela には state を編集するコマンドがない。写しを変えても、何も変わらない。
- **インフラを直接変える道がない:** AI の作業場所にも CI にも、クラウドや Proxmox の資格情報を置かない。
- **AI 向けの手引き:** 宣言のリポジトリに、AI 向けの短い手引き（`AGENTS.md`）を置ける（`mamela gitops init` が書く）。何を実行すればよいか（`mamela check --view`）と、承認は人がすることを書く。ただし、これは助けであって、安全の根拠ではない。

### 10. 承認の本人性: 物理キーの署名

AI はしばしば、人の GitHub のログイン（`gh` のトークン）で動く。そのとき GitHub の上では、AI のコメントと人のコメントを区別できない。AI が `/mamela approve` と書けてしまう。

そこで、承認者ごとに、物理キーの SSH 公開鍵（`sk-ssh-ed25519@openssh.com`）を設定できるようにする。

```toml
[approval_keys]
alice = ["sk-ssh-ed25519@openssh.com AAAA..."]
```

- **署名がなければ受け付けない:** 鍵を設定した承認者の承認は、署名がなければ受け付けない。承認のコメントには、digest への署名を添える（`ssh-keygen -Y sign -n mamela-approve`、または `mamela approve --sign`）。
- **署名する内容:** リポジトリ、ディレクトリ、plan の digest。別の plan、別のリポジトリには使えない。
- **キーに触れたことを確かめる:** Control Plane は、署名に user presence のフラグがあることを確かめる。触れずに作った署名は受け付けない。ソフトウェアだけでは署名できないので、人のトークンを持つ AI にも承認はできない。
- **鍵を設定しない運用:** AI には別の GitHub アカウント（承認者に入れない）を使わせる。Bot の種類のアカウントは、今も承認できない。

## 検討したが採らなかったもの

- **AI に読み取りだけの資格情報を渡して plan させる:** 読み取りの権限でも、秘密（環境変数、ユーザーデータ）や構成の全体が読める。資格情報の管理が、Control Plane の外に広がる。写しなら、何を出すかを Control Plane が決められる。
- **写しを Control Plane の API で配る:** Control Plane は NAT の内側にあり、外からの入口を作らない（ADR 0010）。
- **自動同期で、plan のうち自動にできる変更だけを実行する:** 承認は plan 全体の digest に結び付く。一部だけを実行すると、残りの承認が、別の世界に対するものになる。
- **自動同期で、ずれを何度でも直す（Argo CD の既定）:** 別の管理者と争い続け、要求を送り続ける。同じ変更が二度出たら止める。

## 結果

- **人:** 自動同期をオンにしたディレクトリでは、マージと、policy が自動とする変更だけで運用が回る。それ以外は、issue と PR で差分を見て、1 行（とキーに触れること）で承認する。
- **AI:** 資格情報なしで、今の実物・許されていること・自分の変更の結果を、書いているうちに知れる。インフラも状態も直接は変えられず、承認もできない。
- **provider の作者:** `Change`・`Sensitive`・`Health` を spec と観測に書き、`grants` を実装する。大部分は kit が受け持つ。
- **増えるもの:** 先頭の定期的な plan、同期の issue、写しの公開、`check --view`、`grants`、policy の設定ファイル、署名の検証。

## 実装の順番

1. 同期の状態（先頭の定期的な plan、commit status、同期の issue、`/mamela sync`、`mamela gitops status`）
2. 写し（`mamela-view/v1`）と `mamela check --view`（差分・要求・policy）
3. `grants`（AWS・Proxmox）と、plan の Blocked、check の権限の誤り
4. spec の `Change`・`Sensitive`、Pome の生成、kit の `Diff`
5. Control Plane の policy の設定ファイルと自動同期（self-heal、時間帯、上限、止まる条件）
6. 承認の署名

それぞれ、実機（ラボの Proxmox と AWS）で確かめてから次へ進む。
