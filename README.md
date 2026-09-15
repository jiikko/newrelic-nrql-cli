# newrelic-nrql-cli

New Relic に NRQL を投げる読み取り専用 CLI（コマンド名 `nrql`）。

**API キーの発行が要りません。** ログイン済みのブラウザ（Chrome 等）のセッションをそのまま借ります。
CI など無人環境では User API key にも切り替えられます。

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

macOS 専用です（ブラウザの保存領域を macOS Keychain 経由で復号するため。
`security` コマンドと `~/Library/Application Support` 配下のパスに依存しています）。
`NEW_RELIC_API_KEY` を使う経路はブラウザを読まないので、他 OS でも動く見込みですが**未検証**です。

## セットアップ

1. Chrome で <https://one.newrelic.com> にログインしておく
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

> 🚨 **リージョンを間違えてもエラーになりません。** US のアカウントに `-region eu` を付けて
> 実行したところ、認証エラーにならず `count 0` が返りました（実測）。「データが無い」のか
> 「リージョンが違う」のかを CLI 側から区別できないため、**0 件が返って心当たりが無いときは
> リージョン設定を疑ってください**（`issues/004` 参照）。

## 使い方

```console
nrql [オプション] "<NRQL>"        # クエリ実行（query サブコマンドは省略可）
nrql query [オプション] "<NRQL>"  # 同上（明示形）
nrql accounts                     # アクセスできるアカウント一覧
nrql config show|set|path         # 設定（key: account / region / browser / profile）
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
| `-browser <name>` | `Chrome` / `Brave` / `Chromium` / `Edge` / `Vivaldi`（既定 `Chrome`） |
| `-profile <name>` | プロファイル名。既定 `auto`（ログイン済みを自動検出） |

設定の優先順位: **コマンドラインフラグ > 環境変数 > `config.yml` > 既定**

| 環境変数 | 説明 |
|---|---|
| `NEW_RELIC_ACCOUNT_ID` | 既定のアカウント ID |
| `NEW_RELIC_REGION` | `us` / `eu`（既定 `us`） |
| `NEW_RELIC_API_KEY` | User API key。設定するとブラウザを読まず公開 NerdGraph を使う（CI 向け） |
| `NRQL_BROWSER` / `NRQL_CHROME_PROFILE` | ブラウザ / プロファイル |

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

ブラウザの保存領域を復号する部分（`cmd/nrql/cookies.go`）は
[jiikko/esa-cli](https://github.com/jiikko/esa-cli) からの移植です。仕様が共通なので、
修正が要る場合は両方に当ててください。

## ライセンス

MIT
