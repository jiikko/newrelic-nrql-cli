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
	"os"
	"strconv"
	"strings"
)

// config は解決済みの実行設定。
type config struct {
	accountID int
	region    string // us / eu（New Relic のデータセンター）
	browser   string // Chrome / Brave / ...
	profile   string // Default / Profile 1 / auto
	format    string // tsv / table / json
	noHeader  bool
}

// registerCommon は全サブコマンド共通のフラグを登録する。
// 既定値は「環境変数 > config.yml > 組み込み既定」で解決し、-flag の明示指定が最優先になる。
func registerCommon(fs *flag.FlagSet, cfg *config) {
	fc := loadFileConfig()
	account := resolveDefault("NEW_RELIC_ACCOUNT_ID", fc.Account, "")
	accountDefault := 0
	if account != "" {
		n, err := strconv.Atoi(account)
		if err != nil {
			// 黙って 0 に落とすと「アカウント ID が未設定です」と誤案内してしまう。
			fmt.Fprintf(os.Stderr, "警告: アカウント ID %q を数値として解釈できません（無視します）\n", account)
		}
		accountDefault = n
	}
	fs.IntVar(&cfg.accountID, "account", accountDefault, "New Relic アカウント ID（必須）/ NEW_RELIC_ACCOUNT_ID / config.yml account")
	fs.IntVar(&cfg.accountID, "a", accountDefault, "-account の別名")
	fs.StringVar(&cfg.region, "region", resolveDefault("NEW_RELIC_REGION", fc.Region, "us"), "New Relic のデータセンター（us / eu）/ NEW_RELIC_REGION / config.yml region")
	fs.StringVar(&cfg.browser, "browser", resolveDefault("NRQL_BROWSER", fc.Browser, "Chrome"), "セッションを読むブラウザ（Chrome/Brave/Chromium/Edge/Vivaldi）/ NRQL_BROWSER")
	fs.StringVar(&cfg.profile, "profile", resolveDefault("NRQL_CHROME_PROFILE", fc.Profile, profileAuto), "ブラウザのプロファイル名。既定 auto（自動検出）/ NRQL_CHROME_PROFILE")
}

const topUsage = `nrql - New Relic に NRQL を投げる CLI（読み取り専用 / ブラウザのログインセッションを利用）

概要:
  NRQL クエリを実行して結果を TSV / 表 / JSON で出力する。API キーの発行は不要で、
  Chrome 等でログイン済みのセッションをそのまま借りる（無人環境では API キーも使える）。

使い方:
  nrql [オプション] "<NRQL>"        クエリを実行する（query は省略可）
  nrql query [オプション] "<NRQL>"  同上（明示形）
  nrql accounts                     アクセスできるアカウント一覧
  nrql config <sub>                 設定ファイルの表示・更新
  nrql help                         このヘルプ

オプション:
  -a, -account <id>  アカウント ID（NEW_RELIC_ACCOUNT_ID / config.yml account でも可）
  -format <fmt>      tsv（既定）/ table / json
  -no-header         TSV のヘッダ行を出さない
  -region <us|eu>    アカウントのデータセンター。既定 us（NEW_RELIC_REGION）
  -browser <name>    Chrome/Brave/Chromium/Edge/Vivaldi。既定 Chrome（NRQL_BROWSER）
  -profile <name>    プロファイル。既定 auto=ログイン済みを自動検出（NRQL_CHROME_PROFILE）

設定の優先順位: コマンドラインフラグ > 環境変数 > config.yml > 既定
  例: nrql config set account 1234567

環境変数:
  NEW_RELIC_ACCOUNT_ID  既定のアカウント ID
  NEW_RELIC_REGION      us / eu。EU のアカウントは eu が要る（既定 us）
  NEW_RELIC_API_KEY     User API key。設定するとブラウザを読まずに公開 NerdGraph を使う（CI 向け）

終了コード: 0=成功 / 1=実行時エラー（セッション切れ・NRQL 構文エラー等） / 2=使い方の誤り

例:
  nrql "SELECT count(*) FROM Transaction SINCE 30 minutes ago"
  nrql -format table "SELECT count(*) FROM Transaction FACET name SINCE 1 hour ago LIMIT 10"
  nrql -format json "SELECT average(duration) FROM Transaction TIMESERIES" | jq '.[0]'
  nrql -no-header "SELECT uniques(host) FROM Transaction" | sort

注意:
  これは New Relic の非公開エンドポイントを叩く非公式ツール。アイドルでセッションが切れると
  401/403 になる（ブラウザで開き直せば復帰する）。仕様変更で壊れる可能性があるため、
  CI など無人環境では NEW_RELIC_API_KEY を使うこと。
`

const queryHelp = `nrql query - NRQL を実行する

使い方:
  nrql query [オプション] "<NRQL>"
  nrql [オプション] "<NRQL>"        （query は省略できる）

オプション:
  -a, -account <id>  アカウント ID（必須。nrql accounts で確認できる）
  -format <fmt>      tsv（既定）/ table / json
  -no-header         TSV のヘッダ行を出さない
  （共通オプション -browser / -profile は nrql --help を参照）

出力:
  NRQL の結果カラムを SELECT の並び順で出す。FACET 等で行ごとにカラムが欠ける場合は
  全行の和集合を取り、欠けたセルは空にする。

例:
  nrql -a 1234567 "SELECT count(*) FROM Transaction SINCE 30 minutes ago"
  nrql -a 1234567 -format table "SELECT count(*) FROM Transaction FACET name LIMIT 10"
`

const accountsHelp = `nrql accounts - アクセスできるアカウント一覧を出す

使い方:
  nrql accounts [オプション]

オプション:
  -format <fmt>   tsv（既定）/ table / json
  （共通オプション -browser / -profile は nrql --help を参照）
`

const configHelp = `nrql config - 設定ファイル（config.yml）を表示・更新する

使い方:
  nrql config show              現在の設定と保存先パスを表示
  nrql config set <key> <value> 値を保存する
  nrql config path              設定ファイルのパスを表示

key: account / region / browser / profile

例:
  nrql config set account 1234567
  nrql config set region eu
  nrql config set profile "Profile 3"
`

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
		var ue *usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(os.Stderr, ue.Error())
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "エラー: "+err.Error())
		os.Exit(1)
	}
}

// usageError は「引数の指定ミス」を表す。main で終了コード 2 として扱う。
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// requireAccount はアカウント ID が未設定なら使い方エラーを返す。
func (c config) requireAccount() error {
	if c.accountID <= 0 {
		return &usageError{"エラー: アカウント ID が未設定です。\n" +
			"  nrql accounts               で一覧を確認し\n" +
			"  nrql config set account <id> で保存する（推奨。以後は指定不要）\n" +
			"  もしくは nrql -a <id> \"<NRQL>\" / export NEW_RELIC_ACCOUNT_ID=<id>"}
	}
	return nil
}

func newFlagSet(name, help string, cfg *config) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stdout, help) }
	registerCommon(fs, cfg)
	fs.StringVar(&cfg.format, "format", "tsv", "出力形式（tsv / table / json）")
	fs.BoolVar(&cfg.noHeader, "no-header", false, "TSV のヘッダ行を出力しない")
	return fs
}

func cmdQuery(args []string) error {
	var cfg config
	fs := newFlagSet("query", queryHelp, &cfg)
	fs.Parse(args)

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
	rows, err := c.runNRQL(cfg.accountID, query)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "0 件")
		return nil
	}
	return render(cfg, rows)
}

func cmdAccounts(args []string) error {
	var cfg config
	fs := newFlagSet("accounts", accountsHelp, &cfg)
	fs.Parse(args)

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
	return render(cfg, rows)
}

func cmdConfig(args []string) error {
	if len(args) == 0 {
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
		fmt.Printf("%-10s %s\n", "account:", fc.Account)
		fmt.Printf("%-10s %s\n", "region:", fc.Region)
		fmt.Printf("%-10s %s\n", "browser:", fc.Browser)
		fmt.Printf("%-10s %s\n", "profile:", fc.Profile)
		return nil
	case "path":
		fmt.Println(path)
		return nil
	case "set":
		if len(args) < 3 {
			return &usageError{"エラー: 使い方: nrql config set <account|region|browser|profile> <値>"}
		}
		fc := loadFileConfig()
		key, value := args[1], args[2]
		switch key {
		case "account":
			if _, err := strconv.Atoi(value); err != nil {
				return &usageError{fmt.Sprintf("エラー: account は数値のアカウント ID です: %q", value)}
			}
			fc.Account = value
		case "region":
			if _, err := regionEndpoints(value); err != nil {
				return &usageError{"エラー: " + err.Error()}
			}
			fc.Region = value
		case "browser":
			if _, ok := browserProfiles[value]; !ok {
				return &usageError{fmt.Sprintf("エラー: 未対応のブラウザ %q（Chrome/Brave/Chromium/Edge/Vivaldi）", value)}
			}
			fc.Browser = value
		case "profile":
			fc.Profile = value
		default:
			return &usageError{fmt.Sprintf("エラー: 不明なキー %q（account / region / browser / profile）", key)}
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

func validateFormat(f string) error {
	switch f {
	case "tsv", "table", "json":
		return nil
	default:
		return &usageError{fmt.Sprintf("エラー: 不明な出力形式 %q（tsv / table / json）", f)}
	}
}

func render(cfg config, rows []resultRow) error {
	switch cfg.format {
	case "json":
		return renderJSON(os.Stdout, rows)
	case "table":
		return renderTable(os.Stdout, rows)
	default:
		return renderTSV(os.Stdout, rows, !cfg.noHeader)
	}
}
