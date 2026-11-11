# ADR 0005 — クラウドの API（AWS など）のプロバイダー

- 状態: Accepted（2026-11-11。1–6 を実装し、例の EC2 プロバイダーで適合試験を通した。実際の AWS では未確認）
- 日付: 2026-11-11
- 対象: ADR 0002（プロバイダーの契約）と ADR 0004（成果物とノード）の上に、AWS のようなクラウドの API を載せる


> **後の変更（ADR 0008）:** Mamela 本体はカタログを持たなくなった。カタログはプロバイダーの作者が書き、プロバイダーの Pome に同梱する。運用者は `catalog = "pome:<名前>"` で選ぶ。この ADR の `builtin:` のカタログは、AWS は mamela-aws に、Proxmox は mamela-proxmox に移った。

## 文脈

ADR 0004 までの契約は、Proxmox の API を念頭に置いていた。Proxmox の API には、次の特徴がある。

- 呼び出しをパスで区別する
- トークンを固定のヘッダーで送る
- マシンの ID（VMID）が作る前に決まる
- seed を ISO でアップロードできる

EC2 のようなクラウドの API は、どれも違う。

1. **呼び出しの区別:** すべての呼び出しが `POST /` で、`Action=RunInstances` のようなパラメータで区別する。パスだけでは、作成と削除を区別できない。
2. **署名:** 要求ごとに、本文を含めて SigV4 で署名する。
3. **マシンの ID:** `i-0abc…` のような文字列で、作るまで決まらない。
4. **seed の渡し方:** ISO をアップロードせず、作るときに user-data（16 KiB まで）として渡す。

## 決定

### 1. 固定パラメータで呼び出しを区別する

- **宣言:** 呼び出し（`Call`）とカタログの項目（`Entry`）に `fixed` を足す。例: `{"Action": "RunInstances", "Version": "2016-11-15", "MinCount": "1", "MaxCount": "1"}`。呼び出しは、メソッド・パス・固定パラメータで決まる。
- **宣言の検査:**
  - 同じメソッドとパスの呼び出しは、どれも固定パラメータを持つ。
  - どの 2 つも、共通に固定するパラメータのどこかで値が違う。どの要求も、1 つの呼び出しにしか当たらない。
- **host での照合:** host は、要求の固定パラメータで呼び出しとカタログの項目を決め、許可も照合する。固定値と違う値（`MaxCount=50`）の要求は、宣言した呼び出しにならないので送らない。
- **パスとパラメータ名:** パス `/` と、`.` を含むパラメータ名（`Filter.1.Name`）を認める。

### 2. SigV4

- `mamela.providers.toml` の `auth = "aws-sigv4"` で使う。`token_id` はアクセスキー ID、`token_file` はシークレット、`region` と `service` は署名の範囲である。
- host は、本文を含めて各要求に署名する（AWS SDK の署名器）。プロバイダーは鍵を見ない。
- 署名できなければ送らない。接続先の識別（承認の範囲）に、region と service を含める。

### 3. マシンの ID は、Mamela が振る番号と、作成時に決まる ID で扱う

- **番号:** mamelet の識別（revision と登録）は、整数の番号のままにする。seed の `nodeId` が 0 なら、Mamela が番号を振る（`node_numbers`）。番号は 10 億から始め、Proxmox の VMID と重ならない。
- **プロバイダーへの知らせ方:** プロバイダーは、`host/artifact` で番号を知り、`{artifact:seed.node}` でタグなどに書く。観測では、その番号を `node.id` として返す。
- **所有の照合:** 作成の記録は、作る前に分かる ID（EC2 のクライアントトークン）で残す。観測した ID（クライアントトークンとインスタンス ID）と照らすとき、「一方が他方に含まれ、値が一致する」を同じ対象とみなす（`protocol.Vouch`）。
- **`node` を返す条件:** 観測で `node` を求めるのは、プロバイダーが `owned` と申告したときだけにする。作成途中（`unfinished`）では、番号がまだ分からないことがある。

### 4. user-data の seed

- **形式:** seed に `format: "user-data"` を足す。
- **mamelet の入れ方:** user-data には mamelet のバイナリが入らない（16 KiB）。そこで、Control Plane から取得させる。
  - CP は、`/v1/mamelet/<sha256>` で配る。秘密ではないので、だれでも取得できる。
  - user-data は、Mamela の CA で CP を確かめて取得し、sha256 が一致するときだけ入れる。
- **値として渡す:** 承認した値の `{artifact:<id>.base64}` は、プロバイダーが参照のまま送り、host が送る直前に中身を入れる。登録トークンを含む中身は、プロバイダーに見せない。
- **ネットワーク:** user-data はネットワークを設定しない（クラウドのサブネットの DHCP）。

### 5. EC2 のカタログ（Mamela が持つ）

- **`builtin:ec2`（`provider/catalogs/ec2.go`）:**

  | 呼び出し | 効果 |
  |---|---|
  | DescribeInstances | read |
  | RunInstances | create |
  | Start/Stop/RebootInstances | restart |
  | ModifyInstanceAttribute、CreateTags | change |
  | TerminateInstances | lose-data |

- **大きさ:** インスタンスタイプで決まる。Mamela が vCPU とメモリの表を持ち（`format: "table"`）、site の 1 台あたりの予算で確かめる。表にないタイプは拒む。
- **ディスク:** ルートボリュームの大きさで確かめる。

### 6. 例: `examples/ec2`

- **プロバイダー:** Go/WASM で、偽の EC2（XML の応答）を相手に、Mamela の適合試験を通す。次の点を確かめる。
  - 収束すること
  - 宣言を外したインスタンスを残すこと
  - すべての変更要求について、送られなかった場合と応答が失われた場合から回復すること
  - host が埋める user-data
- **操作:**
  - `RunInstances` で作る。クライアントトークンで、再試行しても 2 台目を作らない。
  - 止まっていれば起動する。
  - タイプとイメージの変更は、unsupported として示す。

## 結果

- AWS の SigV4 とクエリ API のプロバイダーを、Mamela の外で作れる。Mamela が持つのは、カタログ（何を送ってよいか、どれだけ大きくてよいか）と、鍵と、seed である。
- **まだないもの:** 実際の AWS での確認。
- **今回の範囲外:**
  - JSON の本文を送る API（DynamoDB など）
  - 一時的な認証情報（セッショントークン、IAM ロール）
  - 結果を何ページにも分けて返す一覧
