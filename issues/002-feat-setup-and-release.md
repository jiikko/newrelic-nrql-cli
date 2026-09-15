# 002 feat: セットアップ導線とリリース整備（雛形から外した分）

esa-cli にはあるが、雛形では意図的に入れていないもの。必要になった時点で入れる。

## 候補

- [ ] `nrql setup`（対話式セットアップ。アカウント一覧を出して選ばせ config.yml に保存）
      — 現状は `nrql accounts` → `nrql config set account <id>` の 2 手で足りる
- [ ] プロファイル自動検出結果のキャッシュ
      — 現状は候補が複数あるときだけ `ping()` で探索する。候補 1 件なら往復ゼロなので、
        遅いと感じてから入れる（速度を主張するなら実測値を添える）
- [ ] `.goreleaser.yaml` + Homebrew tap（`jiikko/homebrew-tap` に `nrql.rb`）
- [ ] CI（`go build` / `go vet` / `go test`）

## 判断基準

どれも「無くても使える」もの。**実環境での疎通（issue 001）が済むまで着手しない**
（動かないツールの配布導線を先に作っても意味がないため）。
