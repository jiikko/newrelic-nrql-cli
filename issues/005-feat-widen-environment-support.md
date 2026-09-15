# 005 feat: 対応環境を広げる（ブラウザ・タイムアウト）

公開リポジトリとして「自分の環境でしか動かない」部分を潰すための棚卸し。
どれも今すぐ困ってはいないので、要望か実害が出たら着手する。

## ブラウザ: Google Chrome 専用にしてある（決定済み）

対応表は `cmd/nrql/cookies.go` の chrome* 定数のみ。Brave / Chromium / Edge / Vivaldi /
Arc / Chrome Beta・Canary / Opera は**意図的に未対応**。

理由: 対応表の値（Keychain のサービス名・Application Support 配下のディレクトリ名）は
実機で確認しないと正しいか分からず、未確認の値を並べると「動くように見えて別ブラウザの
領域を読みに行く」形の事故になる。手元で確認できるのは Chrome だけ。

広げるときの手順（推測で書かないこと）:

1. 対象ブラウザで New Relic にログインする
2. `security find-generic-password -w -a <account> -s "<Browser> Safe Storage"` が通ることを確認
3. `~/Library/Application Support/` 配下の実ディレクトリ名を確認
4. 上記 2 つを確認した値だけを定数に足す（Firefox は Keychain を使わないので別実装が要る）

## HTTP タイムアウト

`cmd/nrql/client.go` の 60 秒固定。長い `TIMESERIES` や広い `FACET` で足りない可能性がある。
`-timeout` フラグか `NRQL_TIMEOUT` を足すのが素直。**実際にタイムアウトを踏むまで着手しない**
（踏んだら、そのクエリと所要時間をこの issue に記録する）。

## 他 OS

macOS 専用（`security` コマンドと `~/Library/Application Support` に依存）。
`NEW_RELIC_API_KEY` 経路はブラウザを読まないので他 OS でも動く見込みだが**未検証**。
Linux / Windows のセッション復号は、それぞれ別の仕組み（Secret Service / DPAPI）が要る。

## 受け入れ条件

着手する項目ごとに、実機で確認した内容をこの issue に追記してから閉じる。
