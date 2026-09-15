# 001 feat: 実環境（New Relic 本番セッション）での疎通確認

実測日: 2026-09-15 / 実行環境: macOS, Chrome（プロファイルは auto 検出）。
実測はプライベートなアカウントに対して行ったため、**このファイルにアカウント ID・
アカウント名・件数の実値は書かない**（公開リポジトリのため）。結論だけを残す。

## 検証結果

- [x] `one.newrelic.com/graphql` にブラウザセッションで POST すると 200 が返る
- [x] `newrelic-requesting-services` ヘッダが無いと 403 になる
      — **実験で確認**。ヘッダの `req.Header.Set` を削った変異バイナリを使い捨てコピーで
        ビルドして実行したところ、ログイン済みのまま HTTP 403。ヘッダ有りでは同じ実行が 200。
        値の任意性（`nrql-cli` で通る）も同時に確認できた
- [x] `nrql(query: "...")` のインライン埋め込みが通る
      — 文字列リテラルを含むクエリ（`WHERE name LIKE '%"x"%'`）も rc=0 で返った
- [x] `{ actor { accounts { id name } } }` がブラウザセッションでも通る
- [x] プロファイル自動検出が動く（New Relic のセッションを持つプロファイルを選ぶ）
- [ ] `{ actor { user { name } } }`（`ping()`）
      — 候補プロファイルが 1 件だったため**今回の実行では呼ばれていない**。GraphQL としては
        accounts と同じ actor 配下なので通る見込みだが、複数プロファイルに New Relic の
        セッションがある環境まで**未確認**
- [ ] セッション切れ時に返るのが 401 か 403 か
      — **未確認**（アイドル待ちが必要）。現状の実装は両方を同じ扱いにしているので、
        どちらでもメッセージは出る。**trigger**: 実際にセッションを切らして遭遇したとき、
        返ってきたステータスをここに追記する
- [ ] `NEW_RELIC_API_KEY` 経由（`api.newrelic.com/graphql` + `Api-Key`）
      — **未確認**（User API key を用意していない）。**trigger**: CI で使う必要が出たとき

## 受け入れ条件

- [x] `nrql accounts` がアカウント一覧を返す
- [x] `nrql -a <id> "SELECT count(*) FROM Transaction SINCE 30 minutes ago"` が件数を返す
- [x] `FACET` 付きクエリでカラムが API のレスポンス順どおりに出る（`-format table` で目視）
- [x] `-format json` が TIMESERIES の結果を配列で返す（数値は指数表記にならない）
- [x] NRQL 構文エラーが rc=1 と NerdGraph のメッセージで返る
      （`SELECT count(*) FROM` → `NRQL Syntax Error: ... query ended unexpectedly`）
- [ ] セッションを切らした状態のエラーメッセージ確認（上記のとおり未確認）
- [ ] `NEW_RELIC_API_KEY` 経由の実行（上記のとおり未確認）

## この検証で直したもの

- 403 のエラーメッセージ（`cmd/nrql/client.go` の `errSessionExpired`）:
  403 は「セッション切れ」と「CSRF 対策の変更」の 2 原因を持つことが実験で分かったため、
  「ログインし直しても 403 が続くならヘッダのチェックが変わった可能性」を追記した。
  変異実験で出た 403 のメッセージが原因を 1 つしか案内しておらず、誤誘導になっていた

## 分かったこと（仕様メモ）

- **アカウントは NRQL の `WHERE` では切り替えられない**。実測: アカウント A に対して
  `WHERE accountId = <B>` を投げると 0 件で、同じクエリを B に直接投げると件数が返る。
  `SELECT keyset()` にも `accountId` は無い（`tags.accountId` 等のエンティティタグのみ）。
  アカウントは GraphQL 側の `actor { account(id:) }` で決まるスコープなので、`-a` が要る
- **複数アカウントの同時クエリは可能**。`actor { nrql(accounts: [A, B], query:) }` の形で
  投げると合算値が返ることを実験で確認した（→ issue 003）。ただし `FACET accountId` で
  アカウント別に割ることは**できなかった**（結果 0 件。理由は未調査）

## 残タスク

1. セッション切れ時のステータス確認（trigger 待ち）
2. API キー経路の確認（trigger 待ち）

どちらも待ちなので、遭遇するまでこの issue は open のままにする。
