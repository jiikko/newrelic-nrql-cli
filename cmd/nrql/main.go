// Command nrql は New Relic に NRQL を投げる読み取り専用 CLI。
//
// 認証は 2 通り:
//   - 既定: ログイン済みブラウザのセッションを借りる（API キーの発行が要らない）
//   - NEW_RELIC_API_KEY があれば User API key（CI など無人環境向け）
//
// パスはすべて HOME 基準で解決し、カレントディレクトリに依存しない。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// config は解決済みの実行設定。
type config struct {
	accountSpec string // -a に渡された生の文字列（"123" / "123,456"）
	accountIDs  []int  // 解析済み
	region      string // us / eu（New Relic のデータセンター）
	profile     string // Default / Profile 1 / auto（Chrome のプロファイル）
	timeout     int    // 1 リクエストの上限秒数
	format      string // tsv / table / json
	noHeader    bool
}

// registerCommon は全サブコマンド共通のフラグを登録する。
// 既定値は「環境変数 > config.yml > 組み込み既定」で解決し、-flag の明示指定が最優先になる。
func registerCommon(fs *flag.FlagSet, cfg *config) {
	fc := loadFileConfig()
	accountDefault, warn := resolveAccountDefault(os.Getenv("NEW_RELIC_ACCOUNT_ID"), int(fc.Account))
	if warn != "" {
		fmt.Fprintln(os.Stderr, warn)
	}
	def := ""
	if accountDefault > 0 {
		def = strconv.Itoa(accountDefault)
	}
	fs.StringVar(&cfg.accountSpec, "account", def, "New Relic アカウント ID（必須）。カンマ区切りで複数指定可 / NEW_RELIC_ACCOUNT_ID / config.yml account")
	fs.StringVar(&cfg.accountSpec, "a", def, "-account の別名")
	fs.StringVar(&cfg.region, "region", resolveDefault("NEW_RELIC_REGION", fc.Region, "us"), "New Relic のデータセンター（us / eu）/ NEW_RELIC_REGION / config.yml region")
	fs.IntVar(&cfg.timeout, "timeout", resolveTimeout(int(fc.Timeout)), "1 リクエストの上限秒数（既定 60）/ NRQL_TIMEOUT / config.yml timeout")
	fs.StringVar(&cfg.profile, "profile", resolveDefault("NRQL_CHROME_PROFILE", fc.Profile, profileAuto), "ブラウザのプロファイル名。既定 auto（自動検出）/ NRQL_CHROME_PROFILE")
}

// topUsage は `nrql` / `nrql --help` の出力。概要とサブコマンドの一覧だけを持ち、詳細は各サブコマンドの --help に置く。
const topUsage = `nrql - New Relic に NRQL を投げる CLI（読み取り専用 / Chrome のログインで認証）

使い方:  nrql [オプション] "<NRQL>"         NRQL を実行する（nrql query の省略形）
         nrql <サブコマンド> [オプション] [引数]

サブコマンド:
  query       NRQL を実行する（サブコマンドを省くとこれ）
  accounts    アクセスできるアカウントの一覧を出す
  config      設定ファイル（config.yml）を表示・更新する
  help        このヘルプ

各サブコマンドの詳細（オプション・環境変数・終了コード・注意）:  nrql <サブコマンド> --help
はじめて使うとき:  nrql accounts でアカウント ID を調べ、nrql config set account <ID> で保存する
例:  nrql "SELECT count(*) FROM Transaction SINCE 30 minutes ago"
`

// commonOptionsHelp は New Relic に問い合わせるサブコマンドの help に共通のオプションの説明（正本はここだけ）。
const commonOptionsHelp = `
共通オプション:
  -region <us|eu>    アカウントのデータセンター。既定 us（NEW_RELIC_REGION / config.yml の region）
  -timeout <秒>      1 リクエストの上限秒数。既定 60（NRQL_TIMEOUT / config.yml の timeout）
  -profile <name>    Chrome のプロファイル。既定 auto=ログイン済みを自動検出（NRQL_CHROME_PROFILE / config.yml の profile）
  優先順位: コマンドラインフラグ > 環境変数 > config.yml > 既定（詳細は nrql config --help）
`

// commonTailHelp は New Relic に問い合わせるサブコマンドの help の末尾に付ける、環境変数・終了コード・注意（正本はここだけ）。
const commonTailHelp = `
環境変数:
  NEW_RELIC_ACCOUNT_ID  既定のアカウント ID
  NEW_RELIC_REGION      us / eu。EU のアカウントは eu が要る（既定 us）
  NRQL_TIMEOUT          1 リクエストの上限秒数（既定 60）。広い TIMESERIES / FACET で伸ばす
  NEW_RELIC_API_KEY     User API key。設定するとブラウザを読まずに公開 NerdGraph を使う（CI 向け）

終了コード: 0=成功 / 1=実行時エラー（セッション切れ・NRQL 構文エラー等） / 2=使い方の誤り

注意:
  これは New Relic の非公開エンドポイントを叩く非公式ツール（macOS + Google Chrome 専用）。アイドルでセッションが切れると
  401/403 になる（ブラウザで開き直せば復帰する）。仕様変更で壊れる可能性があるため、
  CI など無人環境では NEW_RELIC_API_KEY を使うこと。
`

const queryHelp = `nrql query - NRQL を実行する

使い方:
  nrql query [オプション] "<NRQL>"
  nrql [オプション] "<NRQL>"        （query は省略できる）

オプション:
  -a, -account <id>  アカウント ID（必須。nrql accounts で確認できる）
                     カンマ区切りで複数指定すると、まとめて 1 回のクエリになる
                     例: -a 1234567,2345678（結果は合算。アカウント別には割れない）
  -format <fmt>      tsv（既定）/ table / json
  -no-header         TSV のヘッダ行を出さない

出力:
  NRQL の結果カラムを New Relic が返した並び順で出す（SELECT の順とは限らない）。
  FACET 等で行ごとにカラムが欠ける場合は全行の和集合を取り、欠けたセルは空にする。

例:
  nrql -a 1234567 "SELECT count(*) FROM Transaction SINCE 30 minutes ago"
  nrql -a 1234567 -format table "SELECT count(*) FROM Transaction FACET name LIMIT 10"
  nrql -a 1234567,2345678 "SELECT count(*) FROM Transaction SINCE 1 hour ago"   # 合算
  nrql -a 1234567 -format json "SELECT average(duration) FROM Transaction TIMESERIES" | jq '.[0]'
  nrql -a 1234567 -no-header "SELECT uniques(host) FROM Transaction" | sort

イベント型・属性の調べ方（属性はアカウントごとに違うので、実データに聞く）:
  # イベント型の一覧
  nrql -a 1234567 "SHOW EVENT TYPES SINCE 1 day ago"
  # 属性名と型（数千行になることがあるので grep で絞る）
  nrql -a 1234567 "SELECT keyset() FROM Transaction SINCE 1 day ago" | grep -i duration

NRQL の構文:  https://docs.newrelic.com/docs/nrql/nrql-syntax-clauses-functions/
標準の属性:   https://docs.newrelic.com/attribute-dictionary/
` + commonOptionsHelp + commonTailHelp

const accountsHelp = `nrql accounts - アクセスできるアカウント一覧を出す

使い方:
  nrql accounts [オプション]

オプション:
  -format <fmt>   tsv（既定）/ table / json
` + commonOptionsHelp + commonTailHelp

const configHelp = `nrql config - 設定ファイル（config.yml）を表示・更新する

使い方:
  nrql config show              現在の設定と保存先パスを表示
  nrql config set <key> <value> 値を保存する
  nrql config path              設定ファイルのパスを表示

key: account / region / profile / timeout

例:
  nrql config set account 1234567
  nrql config set region eu
  nrql config set profile "Profile 3"
  nrql config set timeout 180
`

// containsHelpArg は config の引数にヘルプの要求があるかを返す（flag パッケージがヘルプとみなす 4 つの綴りはどの位置でも、help は先頭だけ）。
func containsHelpArg(args []string) bool {
	for i, a := range args {
		switch a {
		case "-h", "-help", "--h", "--help":
			return true
		case "help":
			if i == 0 {
				return true
			}
		}
	}
	return false
}

// subcommands はサブコマンド名から実装への対応表。
//
// dispatch と「サブコマンド名を NRQL と取り違えていないか」の検査の両方がこれを引くので、
// 片方だけ更新して食い違うことがない。init() で組むのは、cmdQuery がこの変数を参照して
// いて、パッケージ変数の初期化式に書くと初期化サイクルになるため。
var subcommands map[string]func([]string) error

func init() {
	subcommands = map[string]func([]string) error{
		"query":    cmdQuery,
		"q":        cmdQuery,
		"accounts": cmdAccounts,
		"config":   cmdConfig,
	}
}

func main() {
	// Chrome の Cookie DB の一時コピーを、Ctrl-C でも残さないようにする（cookies.go の②）。
	installCleanupOnSignal()
	sweepStaleCookieDirs() // ③: 資格情報を読まないコマンド（help 等）でも前回の残骸を消す

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, topUsage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch {
	case subcommands[cmd] != nil:
		err = subcommands[cmd](args)
	case cmd == "help" || cmd == "-h" || cmd == "--help":
		fmt.Fprint(os.Stdout, topUsage)
		return
	default:
		// サブコマンド名でないなら NRQL 本体（またはフラグ）とみなす。
		// これで `nrql -a 123 "SELECT ..."` がそのまま通る。
		err = cmdQuery(os.Args[1:])
	}

	if err != nil {
		code := exitCodeFor(err)
		if code == exitUsage {
			fmt.Fprintln(os.Stderr, err.Error())
		} else {
			fmt.Fprintln(os.Stderr, "エラー: "+err.Error())
		}
		os.Exit(code)
	}
}

// 終了コード。シェルから使うときの契約なので定数で持つ。
const (
	exitOK      = 0 // 成功
	exitRuntime = 1 // 実行時エラー（セッション切れ・NRQL 構文エラー・ネットワーク等）
	exitUsage   = 2 // 使い方の誤り（引数・フラグ・設定値）
)

// exitCodeFor はエラーから終了コードを決める。
// 「使い方の誤り」と「実行時エラー」を混ぜると、スクリプト側で再試行の可否を判断できない。
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return exitUsage
	}
	return exitRuntime
}

// usageError は「引数の指定ミス」を表す。main で終了コード 2 として扱う。
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// checkNoTrailingFlags は「クエリの後ろに置かれたフラグ」を検出する。
//
// `nrql "SELECT ..." -format json` と書くと flag パッケージはそこで解析を止め、
// -format json が NRQL 本文に吸収される。そのままでは New Relic 側の構文エラーとして
// 返るため、原因がフラグの位置だと分からない（実測）。
// NRQL は SELECT / FROM 等で始まるので、`-` で始まる語が残るのは正当な形ではない。
func checkNoTrailingFlags(args []string) error {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			return &usageError{fmt.Sprintf(
				"エラー: %q はフラグとして解釈されませんでした（NRQL 本文の一部になっています）。\n"+
					"  フラグは NRQL より前に置いてください。\n"+
					"  正: nrql %s \"<NRQL>\"\n"+
					"  誤: nrql \"<NRQL>\" %s", a, a, a)}
		}
	}
	return nil
}

// parseAccountSpec は -a の値（"123" / "123,456"）をアカウント ID の並びに直す。
//
// 複数指定は NerdGraph の nrql(accounts: [...]) を使う形になる（issues/003）。
// 重複は取り除き、順序は指定どおりに保つ（案内文とクエリの並びを一致させるため）。
func parseAccountSpec(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var ids []int
	seen := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, &usageError{fmt.Sprintf(
				"エラー: アカウント ID は正の整数です: %q\n  例: nrql -a 1234567 / nrql -a 1234567,2345678", part)}
		}
		if !seen[n] {
			seen[n] = true
			ids = append(ids, n)
		}
	}
	if len(ids) == 0 {
		return nil, &usageError{fmt.Sprintf("エラー: アカウント ID を解釈できません: %q", spec)}
	}
	return ids, nil
}

// requireAccount はアカウント ID を解析し、未設定なら使い方エラーを返す。
func (c *config) requireAccount() error {
	ids, err := parseAccountSpec(c.accountSpec)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return &usageError{"エラー: アカウント ID が未設定です。\n" +
			"  nrql accounts               で一覧を確認し\n" +
			"  nrql config set account <id> で保存する（推奨。以後は指定不要）\n" +
			"  もしくは nrql -a <id> \"<NRQL>\" / export NEW_RELIC_ACCOUNT_ID=<id>"}
	}
	c.accountIDs = ids
	return nil
}

// parseArgs は Parse の結果を 3 つに分ける。
//
//   - --help: 明示的な要求なので **stdout** へ出して正常終了する（パイプで読める）
//   - フラグの誤り: usage は stderr（stdout に混ざるとパイプが壊れる）。rc=2
//   - 正常: そのまま続行
//
// flag.ExitOnError だとこの分岐がプロセスの外で決まってしまい、
// 終了コードの決定（exitCodeFor）もテストも通らない。
//
// 🚨 usage を出すのはこの関数だけ（newFlagSet の fs.Usage は no-op にしてある）。
// flag は ErrHelp を返す**前に**自分で fs.Usage を呼ぶので、そちらでも出すと
// --help が stdout と stderr の両方に出る（実測: nrql query --help が両方に 22 行）。
func parseArgs(fs *flag.FlagSet, help string, args []string, stdout io.Writer) (helpRequested bool, err error) {
	if e := fs.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			fmt.Fprint(stdout, help)
			return true, nil
		}
		fmt.Fprint(fs.Output(), help) // フラグの誤りのときだけ usage を stderr へ
		return false, &usageError{"エラー: " + e.Error()}
	}
	return false, nil
}

func newFlagSet(name string, cfg *config) *flag.FlagSet {
	// 🚨 ExitOnError にしない。フラグの誤りでプロセスごと落ちると、
	// 終了コードの決定が exitCodeFor を通らず、テストからも呼べない。
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr) // 使い方の出力が stdout に混ざるとパイプが壊れる
	// 🚨 no-op。flag は ErrHelp を返す前にここを呼ぶため、ここでも出すと
	// parseArgs が stdout へ出す --help と二重になる。usage は parseArgs が出す。
	fs.Usage = func() {}
	registerCommon(fs, cfg)
	fs.StringVar(&cfg.format, "format", "tsv", "出力形式（tsv / table / json）")
	fs.BoolVar(&cfg.noHeader, "no-header", false, "TSV のヘッダ行を出力しない")
	return fs
}

func cmdQuery(args []string) error {
	var cfg config
	fs := newFlagSet("query", &cfg)
	if done, err := parseArgs(fs, queryHelp, args, os.Stdout); err != nil || done {
		return err
	}

	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return &usageError{"エラー: NRQL を指定してください。\n" +
			"使い方: nrql [オプション] \"<NRQL>\"\n" +
			"例:     nrql \"SELECT count(*) FROM Transaction SINCE 30 minutes ago\"\n" +
			"詳細:   nrql query --help"}
	}
	// `nrql -region eu accounts` のように、サブコマンドをフラグの後ろに書くと
	// flag パッケージはそこで解析を止め、"accounts" が NRQL 本体として残る。
	// 黙って NRQL 構文エラーにすると原因が分からないので、ここで気づかせる。
	if err := checkNoTrailingFlags(fs.Args()); err != nil {
		return err
	}
	if _, ok := subcommands[query]; ok || query == "help" {
		return &usageError{fmt.Sprintf(
			"エラー: %q はサブコマンドです。フラグより前に置いてください。\n"+
				"  正: nrql %s [オプション]\n"+
				"  誤: nrql [オプション] %s   ← NRQL 本体として解釈されます",
			query, query, query)}
	}
	if err := cfg.requireAccount(); err != nil {
		return err
	}
	if err := validateFormat(cfg.format); err != nil {
		return err
	}

	c, err := resolveClient(cfg)
	if err != nil {
		return err
	}
	rows, err := c.runNRQL(cfg.accountIDs, query)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "0 件")
		return nil
	}
	return render(os.Stdout, cfg, rows)
}

func cmdAccounts(args []string) error {
	var cfg config
	fs := newFlagSet("accounts", &cfg)
	if done, err := parseArgs(fs, accountsHelp, args, os.Stdout); err != nil || done {
		return err
	}

	if err := validateFormat(cfg.format); err != nil {
		return err
	}
	c, err := resolveClient(cfg)
	if err != nil {
		return err
	}
	accounts, err := c.accounts()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Fprintln(os.Stderr, "アクセスできるアカウントがありません")
		return nil
	}
	// アカウント一覧も NRQL の結果と同じレンダラに載せる（出力形式を 1 箇所で持つ）。
	rows := make([]resultRow, 0, len(accounts))
	for _, a := range accounts {
		rows = append(rows, resultRow{
			keys:   []string{"id", "name"},
			values: map[string]any{"id": a.ID, "name": a.Name},
		})
	}
	return render(os.Stdout, cfg, rows)
}

func cmdConfig(args []string) error {
	// --help はどの位置でもヘルプにする（以前は「不明なサブコマンド "--help"」、set --help は使い方エラーだった）。
	// 値の位置でも正当にならない: account は正の整数、region は us / eu、timeout は正の整数、profile は Chrome の
	// プロファイルのディレクトリ名（Default / Profile 3 等）で - では始まらない。help はサブコマンドの位置だけ。
	if len(args) == 0 || containsHelpArg(args) {
		fmt.Fprint(os.Stdout, configHelp)
		return nil
	}
	path, err := configFilePath()
	if err != nil {
		return err
	}
	switch args[0] {
	case "show":
		fc := loadFileConfig()
		fmt.Printf("%-10s %s\n", "path:", path)
		fmt.Printf("%-10s %s\n", "account:", formatOptionalInt(int(fc.Account)))
		fmt.Printf("%-10s %s\n", "region:", fc.Region)
		fmt.Printf("%-10s %s\n", "profile:", fc.Profile)
		fmt.Printf("%-10s %s\n", "timeout:", formatOptionalInt(int(fc.Timeout)))
		return nil
	case "path":
		fmt.Println(path)
		return nil
	case "set":
		// 🚨 読めなかったファイルを「読めたこと」にして上書きしない。
		// 以前は解析に失敗しても警告だけ出してゼロ値から書き直しており、
		// 例えば region: eu が黙って消えた（実測）。消えたことに気づく手段が無い。
		if err := fileConfigProblem(); err != nil {
			return &usageError{fmt.Sprintf(
				"エラー: 設定ファイルを読めないため書き込みを中止しました。\n  %v\n"+
					"  ファイルを直すか削除してから、もう一度実行してください: %s", err, path)}
		}
		if len(args) < 3 {
			return &usageError{"エラー: 使い方: nrql config set <account|region|profile|timeout> <値>"}
		}
		fc := loadFileConfig()
		key, value := args[1], args[2]
		switch key {
		case "account":
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return &usageError{fmt.Sprintf("エラー: account は正の整数のアカウント ID です: %q", value)}
			}
			fc.Account = accountID(n)
		case "region":
			if _, err := regionEndpoints(value); err != nil {
				return &usageError{"エラー: " + err.Error()}
			}
			fc.Region = value
		case "profile":
			fc.Profile = value
		case "timeout":
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return &usageError{fmt.Sprintf("エラー: timeout は正の整数の秒数です: %q", value)}
			}
			fc.Timeout = timeoutSeconds(n)
		default:
			return &usageError{fmt.Sprintf("エラー: 不明なキー %q（account / region / profile / timeout）", key)}
		}
		if err := saveFileConfig(fc); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s に %s = %s を保存しました\n", path, key, value)
		return nil
	default:
		return &usageError{fmt.Sprintf("エラー: 不明なサブコマンド %q\n\n%s", args[0], configHelp)}
	}
}

// formatOptionalInt は config show 用に「未設定なら空欄」で整数を文字列化する。
// account と timeout で同じ表示規則なので 1 箇所に置く。
func formatOptionalInt(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func validateFormat(f string) error {
	switch f {
	case "tsv", "table", "json":
		return nil
	default:
		return &usageError{fmt.Sprintf("エラー: 不明な出力形式 %q（tsv / table / json）", f)}
	}
}

// render は指定された形式で書き出す。出力先を引数に取るのはテストのため
// （os.Stdout 直書きだと、形式の選択が正しいかを確かめられない）。
func render(w io.Writer, cfg config, rows []resultRow) error {
	switch cfg.format {
	case "json":
		return renderJSON(w, rows)
	case "table":
		return renderTable(w, rows)
	default:
		return renderTSV(w, rows, !cfg.noHeader)
	}
}
