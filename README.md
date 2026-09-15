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

```console
go install github.com/jiikko/newrelic-nrql-cli/cmd/nrql@latest
```

**Go 1.24 以上が必要です**（標準ライブラリの `crypto/pbkdf2` を使うため。それ以前の Go では
ビルドできません）。

**macOS + Google Chrome 専用**です（Chrome の保存領域を macOS Keychain 経由で復号するため。
`security` コマンドと `~/Library/Application Support/Google/Chrome` に依存しています）。
Brave / Chromium / Edge / Vivaldi には対応していません（`issues/005`）。
`NEW_RELIC_API_KEY` を使う経路は Chrome を読まないので他 OS でも動く見込みですが**未検証**です。

## セットアップ

1. Google Chrome で <https://one.newrelic.com> にログインしておく
2. アカウント ID を確認して保存する

```console
$ nrql accounts
id       name
--       ----
1234567  Example Inc.

$ nrql config set account 1234567
```

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

サブコマンドはこの 4 つ（`query` / `accounts` / `config` / `help`）だけです。
本体は NRQL を投げる `query` で、`accounts` は最初にアカウント ID を調べるため、
`config` はそれを保存して以後 `-a` を省くための補助です。

| オプション | 説明 |
|---|---|
| `-a`, `-account <id>` | アカウント ID |
| `-format <fmt>` | `tsv`（既定） / `table` / `json` |
| `-no-header` | TSV のヘッダ行を出さない |
| `-region <us\|eu>` | アカウントのデータセンター（既定 `us`） |
| `-profile <name>` | Chrome のプロファイル名。既定 `auto`（ログイン済みを自動検出） |

設定の優先順位: **コマンドラインフラグ > 環境変数 > `config.yml` > 既定**

| 環境変数 | 説明 |
|---|---|
| `NEW_RELIC_ACCOUNT_ID` | 既定のアカウント ID |
| `NEW_RELIC_REGION` | `us` / `eu`（既定 `us`） |
| `NEW_RELIC_API_KEY` | User API key。設定するとブラウザを読まず公開 NerdGraph を使う（CI 向け） |
| `NRQL_CHROME_PROFILE` | Chrome のプロファイル名 |

設定ファイルは `$XDG_CONFIG_HOME/newrelic-nrql-cli/config.yml`（既定 `~/.config/newrelic-nrql-cli/config.yml`）。

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
  （複数アカウントの同時クエリは未実装。`issues/003` 参照）

## 開発

```console
go build ./...
go vet ./...
go test ./...
```

Chrome の保存領域を復号する部分（`cmd/nrql/cookies.go`）は
[jiikko/esa-cli](https://github.com/jiikko/esa-cli) からの移植です（あちらは複数ブラウザ対応、
こちらは Chrome 専用）。復号の仕様は共通なので、そこを直す場合は両方に当ててください。

## ライセンス

MIT
