# 005 feat: 対応環境の方針（決着済み）

## 結論: macOS + Google Chrome に固定する（2026-09-16、ユーザー判断）

対応環境を広げない。この issue は「広げるかどうか」を決めるものだったので、決着として閉じる。

### ブラウザ: Google Chrome のみ

Brave / Chromium / Edge / Vivaldi / Arc / Chrome Beta・Canary / Opera は**対応しない**。

理由は、対応表の値（Keychain のサービス名・Application Support 配下のディレクトリ名）は
実機で確認しないと正しいか分からず、未確認の値を並べると「動くように見えて別ブラウザの
領域を読みに行く」形の事故になるため。手元で確認できるのは Chrome だけ。

将来これを覆すなら、対象ブラウザごとに次を実機で確認してから足すこと（推測で書かない）:

1. 対象ブラウザで New Relic にログインする
2. `security find-generic-password -w -a <account> -s "<Browser> Safe Storage"` が通る
3. `~/Library/Application Support/` 配下の実ディレクトリ名
4. 確認できた値だけを `cmd/nrql/cookies.go` の chrome* 定数と同じ形で足す

Firefox は Keychain を使わないので別実装が要る（同じ手順では足せない）。

### OS: macOS のみ

`security` コマンドと `~/Library/Application Support/Google/Chrome` に依存する。
Linux / Windows のセッション復号はそれぞれ別の仕組み（Secret Service / DPAPI）が要る。

`NEW_RELIC_API_KEY` を使う経路は Chrome を読まないので他 OS でも動く見込みだが**未検証**で、
サポート対象にもしない。

### タイムアウト: 設定可能にした（対応済み）

60 秒固定だったものを `-timeout <秒>` / `NRQL_TIMEOUT` で変更できるようにした。
広い `TIMESERIES` / `FACET` で足りなくなったときに伸ばせる。

- 既定は 60 秒。0 以下・数値でない値は警告を出して既定へ落とす
- セッション経路と API キー経路の両方に届くことをテストで固定した
  （「フラグは在るが効いていない」を検出するため、クライアントの Timeout を直接見る）
