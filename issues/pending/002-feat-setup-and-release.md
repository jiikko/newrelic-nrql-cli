# 002 feat: セットアップ導線とリリース整備（雛形から外した分）

**状態: pending（凍結、2026-09-16）。** 起票時の 4 候補のうち Homebrew tap と CI は完了した。
残るのは次の 3 つで、どれも「無くても使える」もの。**trigger が起きたら再開する**:

- `.goreleaser.yaml` — インストールが遅いという声が出た / Go を持たない利用者へ配りたくなったとき
- `nrql setup` — `accounts` → `config set account` の 2 手が面倒だという声が出たとき
- プロファイル検出のキャッシュ — 起動が遅いと感じたとき（実測値を添えること）

esa-cli にはあるが、雛形では意図的に入れていないもの。必要になった時点で入れる。

## 候補と決着

- [x] **Homebrew tap（`jiikko/homebrew-tap` に `nrql.rb`）** — 完了（2026-09-15）
      `brew install jiikko/tap/nrql` で入る。formula の正本は tap 側の `Formula/nrql.rb` で、
      このリポジトリの `packaging/nrql.rb` は同じ内容の写し（リリース時に sha256 を更新する
      対象をこちらからも辿れるようにするため）。
      実測 2026-09-16: tap に `Formula/nrql.rb`（v0.1.5）が在ること、`brew test nrql` が rc=0、
      `brew upgrade --dry-run nrql` がベア名で rc=0 を確認した。README の「インストール」節に
      導線を書いた（`d33c336`）。
- [x] **CI（`go build` / `go vet` / `go test`）** — 完了（2026-09-16、`.github/workflows/ci.yml`）
      `macos-latest` で gofmt / build / vet / `go test -race` を回す。設計の要点:
      - **Go の版は `go-version-file: go.mod`** で引く（ワークフローに数字を書くと二重管理になる）
      - **`paths:` フィルタを付けない**。付けると対象を触らない commit では起動せず、
        起動しなかっただけの HEAD を「緑」と読み替える事故が起きる
      - gofmt ステップは**検査したファイル数を出す**。0 件なら失敗させる
        （「差分なし」と「何も見ていない」が緑で区別できなくなるため）
      - `-race` を付ける（後始末のシグナルハンドラとテストの pipe 捕捉で goroutine を使う）
      検証: gofmt ゲートの赤側 2 形（整形漏れのファイルがある / Go ファイル 0 件）を実測で rc=1、
      実リポジトリで rc=0（18 ファイルを検査）。
      **GitHub 上で走ったことを確認済み**（run 35054619314 / commit `b1f068c`、2026-09-16）:
      全 10 ステップ success で、ログに gofmt ステップ自身の出力
      `gofmt: 差分なし（18 ファイルを検査）` と
      `ok  github.com/jiikko/newrelic-nrql-cli/cmd/nrql  1.287s` が出ている
      （緑という結果だけでなく、その検査が実行された証拠を見る。skip は緑に見えるため）。
      同 run が Node.js 20 非推奨の annotation を出したので、actions を最新に上げた
      （`actions/checkout@v7` / `actions/setup-go@v7`。setup-go v7 は ESM 化と依存更新のみで
      `go-version-file` / `cache` の入力は変わらない）。
- [ ] **`.goreleaser.yaml`** — 入れない（現構成では不要）
      tap の formula は `depends_on "go" => :build` でソースの tar.gz から build する構成なので、
      ビルド済みバイナリの配布物が要らない。goreleaser が要るのは「バイナリを配って
      インストールを速くしたい」と思ったとき。移植元の esa-cli にも `.goreleaser.yaml` は在るが、
      ファイル冒頭に「【任意・高速化パス】」と書かれた雛形で、CI には配線されていない。
      **再開の trigger**: インストールに時間がかかるという声が出たとき、または
      Go のツールチェインを持たない利用者へ配りたくなったとき。
- [ ] **`nrql setup`（対話式セットアップ）** — trigger 待ち
      現状は `nrql accounts` → `nrql config set account <id>` の 2 手で足りる。
      **再開の trigger**: この 2 手が面倒だという声が出たとき。
- [ ] **プロファイル自動検出結果のキャッシュ** — trigger 待ち
      現状は候補が複数あるときだけ `ping()` で探索する。候補 1 件なら往復ゼロ。
      **再開の trigger**: 起動が遅いと感じたとき（速度を主張するなら before / after の実測値を添える）。

## 判断基準（起票時）とその後

起票時は「どれも無くても使える。**実環境での疎通（issue 001）が済むまで着手しない**」としていた。
issue 001 は 2026-09-15 に実測で閉じており（残る 2 項目は有効な User API key と EU アカウントの
**物待ち**で、ツールが動くかどうかの検証は済んでいる）、着手条件は満たされた。

## 残タスク

- 上の 3 つの trigger 待ち項目（`.goreleaser.yaml` / `nrql setup` / プロファイルキャッシュ）のみ。
  着手できる項目は無くなったので `issues/pending/` へ凍結する（再開の主導権はこちらにあり、
  各項目の trigger は上に書いた）
