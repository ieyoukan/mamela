# ADR 0010 — Control Plane が Git を見る（CI からの plan・承認・apply）

- 状態: Accepted（2026-11-12）。一部を [ADR 0012](0012-mamelacd.md) で変えた: gitops は別のプログラム mamelacd になり、本体の局所 API（`mamela api`）を通して plan・承認・apply する。マージの後は、PR ごとではなく、ブランチの先頭の宣言だけを apply する。
- 日付: 2026-11-12
- 対象: 宣言のリポジトリと CI からの運用。ADR 0001（SQLite、1 つの Control Plane）、0003（Control Plane のホスト）、0006（明示操作）

## 文脈

使う人は、宣言を Git のリポジトリに置き、PR で変更を確かめ、マージで反映したい（GitOps）。CI の runner で `mamela plan`・`apply` を動かすと、次の問題がある。

- **状態が Control Plane にある:** plan・承認・許可の一度きりの使用・作成と引き継ぎの記録は、Control Plane の SQLite にある（ADR 0001）。使い捨ての runner が自分の DB で plan すると、Mamela の記録のない世界で計画することになる。記録がなければ、Mamela は自分の物も Foreign と見る。
- **届かない:** Control Plane は NAT の内側にあることが多い（ラボもそう）。GitHub の runner からは届かない。
- **資格情報が広がる:** Proxmox のトークンや AWS の profile を CI の secrets に置くと、漏れたときの範囲が広がる。今は Control Plane のホストにだけある。
- **runner で他人のコードが動く:** 公開リポジトリの PR は fork から来る。self-hosted runner を Control Plane のホストに置くと、PR のコードがそのホストで動く。

## 決定

### 1. Control Plane が GitHub を見に行く（pull）

`mamela gitops run --config <file>` を、Control Plane のホストで `mamela serve` と並べて動かす（systemd）。

- **見に行く:** 一定の間隔で GitHub の API を読み、PR と承認のコメントを見る。外から Control Plane へ入る道は要らない。mamelet が Control Plane から revision を取りに行くのと同じ向きである。
- **計画と記録:** plan・承認・apply は、人が CLI で行うのとまったく同じ処理（同じ DB、同じ guard、同じ許可）で行う。
- **資格情報は動かない:** API の資格情報は Control Plane のホストから出ない。GitHub に渡すのは、plan の表示と結果だけである。
- **GitHub のトークン:** そのリポジトリだけに限った fine-grained token を使う。権限は次のものだけである。
  - Contents: read
  - Pull requests: read and write（コメント）
  - Commit statuses: read and write

### 2. 流れ

1. **PR で plan:** 開いている PR のうち、次のものの head を取ってきて（tarball）、設定した宣言のディレクトリを plan する。
   - 同じリポジトリのブランチから（fork からは扱わない）。
   - 作者が `authors` にいる。
   
   plan は、変更・理由・送る要求をすべて表示し、digest とともに PR にコメントする。commit status `mamela/<dir>` も付ける。head が変わるたびに plan し直す。
2. **承認は人:** `approvers` にいる人が、PR に次の 1 行だけのコメントを書く。

   ```
   /mamela approve sha256:<digest>
   ```

   Mamela は、そのコメントの GitHub 上の作者を承認者として記録する（`github:<login>`）。承認できるのは、gitops がその PR に出した plan だけである。digest が合わなければ、何も承認しない。
3. **マージで apply:** PR が設定したブランチにマージされたら、マージコミットの宣言を評価する。
   - **宣言が同じとき:** 宣言の digest（revision）が承認した plan と同じなら、その plan を apply する。apply は今までどおり、観測し直し、承認したときと変更が同じことを確かめる（MAM-REC-0002）。guard は前後に走る。
   - **宣言が違うとき:** 例えば main が先に進んでいたときである。マージコミットで plan し直してコメントし、もう一度承認を求める。この承認があれば、すぐに apply する。
4. **結果:** apply の結果を PR にコメントし、マージコミットの commit status にする。

### 3. 任せないこと

- **明示操作:** 削除・再起動・引き継ぎ（`mamela act --confirm`）は gitops ではしない（ADR 0006）。宣言から消しても何も消えない。
- **取り込み:** `mamela import terraform` は人がする。
- **PR を経ない push:** 設定したブランチへの直接の push は plan しない（承認の場がない）。

### 4. 信頼の根

- **`authors`:** PR の宣言の評価は WASM の中で行い、プロバイダーも WASM で隔離して動く。それでも、観測では本物の資格情報で読み取りが行われ、プロバイダーが書く変更の理由は PR のコメントに出る。そのため、計画するのは信頼する作者の PR だけにする。
- **新しいプロバイダー:** 宣言の依存が新しいプロバイダーのプログラムを持ち込んだら、plan のコメントでそれを示す（Pome と sha256）。
- **`approvers`:** 承認できる人である。作者が自分の PR を承認してよいかは設定で決める（`self_approval`、既定は許す。1 人の運用のため）。
- **承認の期限:** 承認は `approval_ttl`（既定 24 時間）で切れる。マージが遅れれば、承認し直す。

### 5. 記録

gitops が出した plan と処理済みのコメント・マージは、SQLite に記録する（migration 13）。そのため、何度起動し直しても、同じ PR に同じ plan を二度コメントせず、同じコメントで二度承認せず、同じマージで二度 apply しない。

## 検討したが採らなかったもの

- **CI の runner で plan・apply する:** 状態と資格情報の問題（上の文脈）がある。
- **Control Plane に push 型の API を開き、GitHub Actions の OIDC で認証する:** Control Plane が外から届く場所にある運用では使える。ただし、ラボは NAT の内側で、公開の入口を新しく作る必要がある。必要になったら、同じ承認の規則で足す。

## 結果

- **使う人:** 宣言を PR で変え、plan を PR で読み、1 行のコメントで承認し、マージで反映できる。
- **資格情報と状態:** Control Plane から動かない。
- **GitHub が落ちても:** 何も変わらない。CLI での運用は今までどおりである。
- **増えるもの:** GitHub の API の読み書き（`internal/gitops`）、tarball の展開（パスを検査し、リンクを拒み、大きさを制限する）、記録の表、`mamela gitops`。
