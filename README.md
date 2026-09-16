# newrelic-nrql-cli

New Relic に NRQL を投げる読み取り専用 CLI（コマンド名 `nrql`）。

**API キーの発行が要りません。** ログイン済みの Google Chrome のセッションをそのまま借ります。
CI など無人環境では User API key にも切り替えられます。

**macOS + Google Chrome 専用です。**

```console
$ nrql "SELECT count(*) FROM Transaction SINCE 30 minutes ago"
count
12345

$ nrql -format table "SELECT count(*) FROM Transaction FACET name SINCE 1 hour ago LIMIT 5"
name              count
----              -----
WebTransaction/A  120394
WebTransaction/B   88210
...
```

## インストール

### Homebrew（推奨）

```console
brew install jiikko/tap/nrql
```

更新は `brew upgrade nrql`、master の先端を試すなら `brew install --HEAD jiikko/tap/nrql`。
Go は formula の build 依存として Homebrew が入れるので、自分で用意する必要はありません。

formula の正本は [jiikko/homebrew-tap](https://github.com/jiikko/homebrew-tap) の
`Formula/nrql.rb` で、このリポジトリの `packaging/nrql.rb` は同じ内容の写しです
（リリース時に sha256 を更新する対象をこちらからも辿れるようにするため）。
直すときは tap 側が先です。

### go install

```console
go install github.com/jiikko/newrelic-nrql-cli/cmd/nrql@latest
```

`go.mod` の `go` ディレクティブが **1.25.0** なので、それ以上の Go が要ります
（Go 1.21 以降なら既定の `GOTOOLCHAIN=auto` が新しいツールチェインを自動で取りに行きます）。

**macOS + Google Chrome 専用です**（方針として固定しています。`issues/done/005` 参照）。
Chrome の保存領域を macOS Keychain 経由で復号するため、`security` コマンドと
`~/Library/Application Support/Google/Chrome` に依存します。
Brave / Chromium / Edge / Vivaldi / Arc などには対応しません。
`NEW_RELIC_API_KEY` を使う経路は Chrome を読まないので他 OS でも動く見込みですが、
**未検証でサポート対象外**です。

## セットアップ

1. Google Chrome で <https://one.newrelic.com> にログインしておく
2. アカウント ID を確認して保存する

```console
$ nrql accounts
id	name
1234567	Example Inc.

$ nrql config set account 1234567
```

既定の出力はタブ区切りです。桁を揃えて読みたいときは `nrql accounts -format table`。

初回実行時に macOS の Keychain 許可ダイアログが出ます。「常に許可」を選んでください。
ターミナルに**フルディスクアクセス**が必要な場合もあります
（システム設定 → プライバシーとセキュリティ → フルディスクアクセス）。

### EU リージョンのアカウント

New Relic は US と EU でホストが分かれています。EU のアカウントなら `-region eu` が要ります。

```console
nrql config set region eu
```

**EU は未検証です**（EU のアカウントを持っていないため）。動かない場合は
`cmd/nrql/client.go` の `regions` にあるホスト名を疑ってください。

> リージョンを間違えた場合、v0.1.2 までは**エラーにならず 0 件が返っていました**が、
> v0.1.3 で「アカウントを参照できませんでした」というエラー（rc=1）になります
> （`issues/done/004` 参照）。0 件が返るのは本当に 0 件のときだけです。

## 使い方

```console
nrql [オプション] "<NRQL>"        # クエリ実行（query サブコマンドは省略可）
nrql query [オプション] "<NRQL>"  # 同上（明示形）
nrql accounts                     # アクセスできるアカウント一覧
nrql config show|set|path         # 設定（key: account / region / profile）
nrql help                         # ヘルプ
```

サブコマンドはこの 4 つ（`query` / `accounts` / `config` / `help`）だけです
（`query` には別名 `q` があります）。
本体は NRQL を投げる `query` で、`accounts` は最初にアカウント ID を調べるため、
`config` はそれを保存して以後 `-a` を省くための補助です。

| オプション | 説明 |
|---|---|
| `-a`, `-account <id>` | アカウント ID。カンマ区切りで複数指定可（`-a 123,456`） |
| `-format <fmt>` | `tsv`（既定） / `table` / `json` |
| `-no-header` | TSV のヘッダ行を出さない |
| `-region <us\|eu>` | アカウントのデータセンター（既定 `us`） |
| `-profile <name>` | Chrome のプロファイル名。既定 `auto`（ログイン済みを自動検出） |
| `-timeout <秒>` | 1 リクエストの上限秒数。既定 `60`。広い `TIMESERIES` / `FACET` で伸ばす |

**フラグは NRQL より前に置いてください。** 後ろに置くと NRQL 本文に吸収されるため、
`nrql "<NRQL>" -format json` は使い方エラー（rc=2）になります。

終了コードは **0=成功 / 1=実行時エラー（セッション切れ・NRQL 構文エラー等） / 2=使い方の誤り**。

設定の優先順位: **コマンドラインフラグ > 環境変数 > `config.yml` > 既定**

| 環境変数 | 説明 |
|---|---|
| `NEW_RELIC_ACCOUNT_ID` | 既定のアカウント ID |
| `NEW_RELIC_REGION` | `us` / `eu`（既定 `us`） |
| `NEW_RELIC_API_KEY` | User API key。設定するとブラウザを読まず公開 NerdGraph を使う（CI 向け） |
| `NRQL_CHROME_PROFILE` | Chrome のプロファイル名 |
| `NRQL_TIMEOUT` | 1 リクエストの上限秒数（既定 60） |

設定ファイルは `$XDG_CONFIG_HOME/newrelic-nrql-cli/config.yml`（既定 `~/.config/newrelic-nrql-cli/config.yml`）。

### 複数アカウントをまとめて見る

`-a` はカンマ区切りで複数のアカウントを受けます。結果は**合算**されます。

```console
nrql -a 1234567,2345678 "SELECT count(*) FROM Transaction SINCE 1 hour ago"
```

アカウント別に割りたいときは **`FACET tags.accountId`** を使ってください
（`FACET accountId` は 0 件になります。`accountId` はイベントの属性ではないため）。

```console
nrql -a 1234567,2345678 -format table \
  "SELECT count(*) FROM Transaction FACET tags.accountId SINCE 1 hour ago"
```

指定したうちの 1 つでも権限が無いと**クエリ全体が失敗**します（部分的な結果は返りません）。

### 出力

- `tsv`（既定）: ヘッダ + タブ区切り。`awk -F'\t'` や `cut` に流す用途
- `table`: 桁を揃えた表。人が読む用途
- `json`: 結果オブジェクトの配列。`jq` に流す用途

カラムは NRQL の `SELECT` の並び順で出ます。`FACET` などで行ごとにカラムが欠ける場合は
全行の和集合を取り、欠けたセルは空にします。

```console
nrql -no-header "SELECT uniques(host) FROM Transaction" | sort
nrql -format json "SELECT average(duration) FROM Transaction TIMESERIES" | jq '.[0]'
```

## 仕組みと制約

ブラウザセッションを使う場合、New Relic の **UI 自身が叩いている内部エンドポイント**
（`one.newrelic.com/graphql`）へ NerdGraph クエリを送ります。これは公式にサポートされた
インターフェースではありません。

- **アイドルでセッションが切れます**。401/403 が返ったらブラウザで開き直してください
- **New Relic 側の仕様変更で壊れる可能性があります**。無人環境では `NEW_RELIC_API_KEY` を使ってください
  （その場合は公開 NerdGraph `api.newrelic.com/graphql` を使うので、この制約はありません）
- 読み取り専用です。書き込み系の NerdGraph mutation は実装していません
- **アカウントは NRQL の `WHERE` では切り替えられません**。`accountId` はイベントの属性ではなく
  「どのデータストアを見るか」というスコープなので、`-a` で外から渡す必要があります

## 開発

```console
go build ./...
go vet ./...
go test ./...
```

Chrome の保存領域を復号する部分（`cmd/nrql/cookies.go`）は
[jiikko/esa-cli](https://github.com/jiikko/esa-cli) からの移植です。どちらも macOS + Chrome 専用
です（esa-cli も Brave / Chromium / Edge / Vivaldi の対応を落としました）。復号の仕様は共通なので、
そこを直す場合は両方に当ててください。

## ライセンス

MIT
