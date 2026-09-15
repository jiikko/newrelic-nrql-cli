# 005 feat: 対応環境を広げる（ブラウザ・タイムアウト）

公開リポジトリとして「自分の環境でしか動かない」部分を潰すための棚卸し。
どれも今すぐ困ってはいないので、要望か実害が出たら着手する。

## ブラウザの対応表

`cmd/nrql/cookies.go` の `browserProfiles` は Chrome / Brave / Chromium / Edge / Vivaldi の
5 つだけ。**Arc・Chrome Beta / Canary・Opera は未対応**。

- 追加には「Keychain のサービス名 / アカウント名」と「Application Support 配下のディレクトリ名」の
  2 つが要る。**推測で書かない**（間違えると Keychain の取得に失敗するだけでなく、
  別ブラウザの領域を読みに行く）。実機で `security find-generic-password` を確認してから足す
- Firefox は仕組みが違う（Keychain を使わない）ので、対応するなら別実装

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
