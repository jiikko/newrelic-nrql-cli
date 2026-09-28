package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// accountID は config.yml の account 値。
//
// 🚨 数値と文字列の両方を受ける。v0.1.0 は文字列として書き出していた
// （`account: "1234567"`）ため、int だけを受ける実装にすると古い設定ファイルの
// 解析がそこで失敗し、**account だけでなく region / profile まで丸ごと無視される**。
// 書き出しは常に数値（この型の underlying type が int なので）。
type accountID int

func (a *accountID) UnmarshalYAML(value *yaml.Node) error {
	n, err := decodeDecimalScalar(value, "account")
	if err != nil {
		return err
	}
	*a = accountID(n)
	return nil
}

// timeoutSeconds は config.yml の timeout 値。account と同じ理由で自前で読む
// （timeout: 060 が 8 進と解釈されて 48 秒になる、のような取り違えを避ける）。
type timeoutSeconds int

func (t *timeoutSeconds) UnmarshalYAML(value *yaml.Node) error {
	n, err := decodeDecimalScalar(value, "timeout")
	if err != nil {
		return err
	}
	*t = timeoutSeconds(n)
	return nil
}

// decodeDecimalScalar は YAML のスカラーを 10 進の整数として読む。空値は 0。
//
// 🚨 value.Decode(&int) に任せない。YAML 1.1 の暗黙変換により
//
//	account: 0123456 → 42798（8 進）/ 0x1F → 31 / 1.5e7 → 15000000
//
// と、**書いた数字と違う値**を警告なしに使う（実測）。
// 生のスカラー文字列を 10 進として読み、それ以外は受け付けない。
func decodeDecimalScalar(value *yaml.Node, field string) (int, error) {
	if value.Kind != yaml.ScalarNode {
		return 0, fmt.Errorf("%s はスカラー値で書いてください", field)
	}
	raw := strings.TrimSpace(value.Value)
	if raw == "" || raw == "null" || raw == "~" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s を 10 進の整数として解釈できません: %q", field, raw)
	}
	return n, nil
}

// fileConfig は config.yml の内容。すべて任意項目。
type fileConfig struct {
	Account accountID      `yaml:"account,omitempty"` // 既定のアカウント ID
	Region  string         `yaml:"region,omitempty"`  // us / eu
	Profile string         `yaml:"profile,omitempty"` // Chrome のプロファイル名
	Timeout timeoutSeconds `yaml:"timeout,omitempty"` // 1 リクエストの上限秒数
}

// configDir は $XDG_CONFIG_HOME/newrelic-nrql-cli（無ければ ~/.config/newrelic-nrql-cli）。
// cwd には依存しない（HOME / XDG 基準）。
func configDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "newrelic-nrql-cli"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "newrelic-nrql-cli"), nil
}

func configFilePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yml"), nil
}

var (
	fileConfigOnce   sync.Once
	fileConfigCached fileConfig
	fileConfigErr    error // 読めなかった / 一部を解析できなかったときの理由（config set はこれを見て書き込みを拒む）
)

// fileConfigProblem は config.yml が在るのに完全には読めていなければその理由を返す（無いだけなら nil）。
func fileConfigProblem() error {
	loadFileConfig()
	return fileConfigErr
}

// loadFileConfig は config.yml を読む（無ければゼロ値）。プロセス内で 1 回だけ読む。
func loadFileConfig() fileConfig {
	fileConfigOnce.Do(func() {
		path, err := configFilePath()
		if err != nil {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			// 🚨 「無い」は ENOENT だけ。EACCES / EIO まで無い扱いにすると fileConfigErr が
			// nil のままで、config set が読めなかったファイルをゼロ値から書き直す。
			if !errors.Is(err, fs.ErrNotExist) {
				fileConfigErr = fmt.Errorf("%s を読めませんでした: %w", path, err)
				fmt.Fprintf(os.Stderr, "警告: %v\n  （config.yml の設定は使わずに続けます）\n", fileConfigErr)
			}
			return
		}
		fc, err := parseFileConfig(data)
		if err != nil {
			fileConfigErr = fmt.Errorf("%s の解析に失敗しました: %w", path, err)
			fmt.Fprintf(os.Stderr, "警告: %v\n", fileConfigErr)
			var fe *fileConfigFieldError
			if errors.As(err, &fe) {
				fmt.Fprintf(os.Stderr, "  （%s は使いません。それ以外の項目は読めた値を使います）\n",
					strings.Join(fe.fieldNames(), " / "))
			}
		}
		fileConfigCached = fc
	})
	return fileConfigCached
}

// fileConfigFieldError は config.yml のうち読めなかった項目の一覧。
// 文書そのものは読めていて、ここに挙がった項目だけを落としたことを表す。
type fileConfigFieldError struct {
	fields []fieldProblem
}

type fieldProblem struct {
	name string
	err  error
}

func (e *fileConfigFieldError) Error() string {
	msgs := make([]string, len(e.fields))
	for i, f := range e.fields {
		msgs[i] = fmt.Sprintf("%s: %v", f.name, f.err)
	}
	return strings.Join(msgs, " / ")
}

// fieldNames は落とした項目の名前を返す（警告文用。<< → account / region / profile / timeout の順）。
func (e *fileConfigFieldError) fieldNames() []string {
	names := make([]string, len(e.fields))
	for i, f := range e.fields {
		names[i] = f.name
	}
	return names
}

// parseFileConfig は config.yml の中身を解釈する。
//
// まず従来どおり struct へ一括で読む（YAML のマージキー `<<` の展開・重複キーの拒否は
// yaml.v3 がこの経路でだけ行う）。成功すればそれがすべて。
//
// 🚨 一括で読めなかったときも、読めた項目は返す。account の書式が不正なだけで
// region / profile まで失うと、利用者が気づかないまま別リージョンへ繋ぎに行く
// （issues/004 の症状を設定ファイル側から作ることになる）。救済は項目ごとに独立に読む
// （以前の「account を除いてもう一度読む」形は、timeout 等が壊れていると全部を失っていた）。
// 戻り値の error は「この設定ファイルは完全には読めていない」という事実で、
// config set はこれを見て上書きを拒む（救済で読めた項目があっても必ず non-nil）。
func parseFileConfig(data []byte) (fileConfig, error) {
	var fc fileConfig
	err := yaml.Unmarshal(data, &fc)
	if err == nil {
		return fc, nil
	}
	return salvageFileConfig(data, err)
}

// salvageFileConfig は一括の読み込みに失敗したファイルから、項目ごとに読める値を拾う。
// whole は一括読み込みのエラー（読めた項目があっても問題が見つからなければこれを返す）。
//
// 🚨 マージキー（<<）の展開と優先順位（トップレベルの値がマージ元より優先）を自前で
// 再現しない。以前の「読めなかったキーを外して読み直す」近似は、外したキーの値がマージ元から
// 戻って「使わない」と警告した値が使われ、マージ元の中の壊れた値が表に出て全体を失った。
// 各項目を yaml.Node で受ける struct へ読むと、展開と優先順位は yaml.v3 がそのまま行うので、
// その後で項目ごとに値を解釈する。
//
// 検出しない形: Chrome が作らない手書き YAML の珍しい組み合わせ（アンカーの中の重複キー等）は
// 網羅しない。その場合も fileConfigErr は non-nil になり、config set が上書きを拒むのが最後の砦。
func salvageFileConfig(data []byte, whole error) (fileConfig, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fileConfig{}, err // 構文として読めない（項目を切り分けられない）
	}
	if doc.Kind == 0 || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fileConfig{}, whole // マッピングでない文書（項目が無い）
	}
	root := doc.Content[0]

	// yaml.v3 は重複キーがあると（知らないキーでも）struct への Decode ごと拒む。
	// どちらが意図した値か分からないので、重複したキーは外してから読む。
	count := map[string]int{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if k := root.Content[i]; !isMergeKey(k) {
			count[k.Value]++
		}
	}
	dup := map[string]bool{}
	for k, n := range count {
		if n > 1 {
			dup[k] = true
		}
	}

	var nodes struct {
		Account yaml.Node `yaml:"account"`
		Region  yaml.Node `yaml:"region"`
		Profile yaml.Node `yaml:"profile"`
		Timeout yaml.Node `yaml:"timeout"`
	}
	var fc fileConfig
	fields := []struct {
		name   string
		node   *yaml.Node
		target any
	}{
		{"account", &nodes.Account, &fc.Account},
		{"region", &nodes.Region, &fc.Region},
		{"profile", &nodes.Profile, &fc.Profile},
		{"timeout", &nodes.Timeout, &fc.Timeout},
	}

	var problems []fieldProblem
	if err := withoutKeys(root, dup).Decode(&nodes); err != nil {
		// マージの展開に失敗した（と考えられる）。原因を取り違えないよう実際のエラーを添える。
		// マージを使わずに、トップレベルに書かれた項目だけを読む。
		problems = append(problems, fieldProblem{name: "<<",
			err: fmt.Errorf("マージキーを展開できません（マージで入る項目は使いません）: %w", err)})
		nodes.Account, nodes.Region, nodes.Profile, nodes.Timeout = yaml.Node{}, yaml.Node{}, yaml.Node{}, yaml.Node{}
		for i := 0; i+1 < len(root.Content); i += 2 {
			k, v := root.Content[i], root.Content[i+1]
			for _, f := range fields {
				if k.Value == f.name && !isMergeKey(k) && !dup[f.name] {
					*f.node = *v
				}
			}
		}
	}

	for _, f := range fields {
		if dup[f.name] {
			// 🚨 マージ元で埋め直さない（外したキーの値がマージ元から戻らないよう Node を読まない）。
			problems = append(problems, fieldProblem{name: f.name, err: fmt.Errorf("キーが %d 回書かれています", count[f.name])})
			continue
		}
		if f.node.Kind == 0 {
			continue // 書かれていない
		}
		if err := f.node.Decode(f.target); err != nil {
			problems = append(problems, fieldProblem{name: f.name, err: err})
		}
	}

	if len(problems) == 0 {
		// 一括では失敗したのに項目単位では問題が見つからない形（知らないキーの重複など）。
		// 読めた値は使うが、error は返し続ける（config set はこれを見て上書きを拒む）。
		return fc, whole
	}
	return fc, &fileConfigFieldError{fields: problems}
}

// isMergeKey は YAML のマージキー（<<）かを返す。
func isMergeKey(k *yaml.Node) bool {
	return k.Tag == "!!merge" || k.Value == "<<"
}

// withoutKeys は mapping から drop に含まれるキーの組を取り除いた写しを返す（元は変えない）。
func withoutKeys(mapping *yaml.Node, drop map[string]bool) *yaml.Node {
	cp := *mapping
	cp.Content = nil
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		if !isMergeKey(k) && drop[k.Value] {
			continue
		}
		cp.Content = append(cp.Content, k, mapping.Content[i+1])
	}
	return &cp
}

// saveFileConfig は config.yml を書き出す（ディレクトリごと作成）。
func saveFileConfig(fc fileConfig) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(fc)
	if err != nil {
		return err
	}
	header := "# newrelic-nrql-cli 設定ファイル（nrql config set で更新できます）\n" +
		"# account: 既定のアカウント ID（nrql accounts で調べられます）\n" +
		"# region: アカウントのデータセンター（us / eu）\n" +
		"# profile: Chrome のプロファイル名（auto でログイン済みを自動検出）\n" +
		"# timeout: 1 リクエストの上限秒数（既定 60）\n"
	return os.WriteFile(filepath.Join(dir, "config.yml"), append([]byte(header), data...), 0o600)
}

// resolveDefault は「環境変数 > config.yml > 組み込み既定」の順で既定値を決める。
// これを flag の既定値に使うことで、-flag の明示指定が最優先になる。
func resolveDefault(envKey, fileValue, builtin string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if fileValue != "" {
		return fileValue
	}
	return builtin
}

// defaultTimeoutSeconds は 1 リクエストの上限（秒）。
// 広い TIMESERIES や FACET を投げると 60 秒では足りないことがあるので -timeout で伸ばせる。
const defaultTimeoutSeconds = 60

// resolveTimeout は「NRQL_TIMEOUT > config.yml の timeout > 組み込み既定」の順で
// タイムアウトの既定値を決める（resolveDefault の int 版）。
//
// 🚨 config.yml を読む段を飛ばさない。-timeout だけが config.yml から読まれない状態は、
// README とヘルプが謳う優先順位（フラグ > 環境変数 > config.yml > 既定）と食い違う。
func resolveTimeout(fileValue int) int {
	if v := os.Getenv("NRQL_TIMEOUT"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil && n > 0 {
			return n
		}
		fmt.Fprintf(os.Stderr, "警告: NRQL_TIMEOUT=%q は正の整数ではありません（config.yml か既定値を使います）\n", v)
	}
	if fileValue > 0 {
		return fileValue
	}
	if fileValue != 0 {
		fmt.Fprintf(os.Stderr, "警告: config.yml の timeout=%d は正の整数ではありません（既定の %d 秒を使います）\n", fileValue, defaultTimeoutSeconds)
	}
	return defaultTimeoutSeconds
}

// resolveAccountDefault はアカウント ID の既定値を「環境変数 > config.yml > 0（未設定）」で決める。
//
// 環境変数だけは文字列で届くので、ここで数値に直す。数値でない値は黙って 0 に落とさず
// 警告つきで無視する（0 に落とすと「アカウント ID が未設定です」と誤案内してしまい、
// 実際には「値が壊れている」ことに気づけない）。
// 戻り値の warn が空でなければ、呼び出し側が利用者へ表示する。
func resolveAccountDefault(envValue string, fileValue int) (account int, warn string) {
	if envValue != "" {
		n, err := strconv.Atoi(envValue)
		if err != nil {
			return fileValue, fmt.Sprintf("警告: アカウント ID %q を数値として解釈できません（無視します）", envValue)
		}
		return n, ""
	}
	return fileValue, ""
}
