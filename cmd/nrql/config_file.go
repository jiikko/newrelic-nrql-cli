package main

import (
	"fmt"
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
	// 🚨 value.Decode(&int) に任せない。YAML 1.1 の暗黙変換により
	//   account: 0123456 → 42798（8 進）/ 0x1F → 31 / 1.5e7 → 15000000
	// と、**書いた数字と違うアカウント**を警告なしに引く（実測）。
	// 生のスカラー文字列を 10 進として読み、それ以外は受け付けない。
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("account はスカラー値で書いてください")
	}
	raw := strings.TrimSpace(value.Value)
	if raw == "" || raw == "null" || raw == "~" {
		*a = 0
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("account を 10 進の整数として解釈できません: %q", raw)
	}
	*a = accountID(n)
	return nil
}

// fileConfig は config.yml の内容。すべて任意項目。
type fileConfig struct {
	Account accountID `yaml:"account,omitempty"` // 既定のアカウント ID
	Region  string    `yaml:"region,omitempty"`  // us / eu
	Profile string    `yaml:"profile,omitempty"` // Chrome のプロファイル名
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
	fileConfigErr    error // 解析に失敗したときの理由（config set はこれを見て書き込みを拒む）
)

// fileConfigProblem は config.yml の解析に失敗していればその理由を返す。
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
			return
		}
		fc, err := parseFileConfig(data)
		if err != nil {
			fileConfigErr = fmt.Errorf("%s の解析に失敗しました: %w", path, err)
			fmt.Fprintf(os.Stderr, "警告: %v\n", fileConfigErr)
			if fc.Region != "" || fc.Profile != "" {
				fmt.Fprintf(os.Stderr, "  （region / profile は読めたのでそのまま使います）\n")
			}
		}
		fileConfigCached = fc
	})
	return fileConfigCached
}

// parseFileConfig は config.yml の中身を解釈する。
//
// 🚨 解析に失敗しても、読めた項目は返す。account の書式が不正なだけで
// region / profile まで失うと、利用者が気づかないまま別リージョンへ繋ぎに行く
// （issues/004 の症状を設定ファイル側から作ることになる）。
// 戻り値の error は「この設定ファイルは完全には読めていない」という事実で、
// config set はこれを見て上書きを拒む。
func parseFileConfig(data []byte) (fileConfig, error) {
	var fc fileConfig
	err := yaml.Unmarshal(data, &fc)
	if err == nil {
		return fc, nil
	}
	// account を除いてもう一度読む（読めるものは救う）。
	var partial struct {
		Region  string `yaml:"region,omitempty"`
		Profile string `yaml:"profile,omitempty"`
	}
	if err2 := yaml.Unmarshal(data, &partial); err2 == nil {
		return fileConfig{Region: partial.Region, Profile: partial.Profile}, err
	}
	return fileConfig{}, err
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
		"# profile: Chrome のプロファイル名（auto でログイン済みを自動検出）\n"
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

// resolveIntDefault は環境変数から正の整数の既定値を読む。
// 不正な値は黙って既定へ落とさず、警告してから既定を使う
// （黙って落とすと「設定したのに効いていない」ことに気づけない）。
func resolveIntDefault(envKey string, builtin int) int {
	v := os.Getenv(envKey)
	if v == "" {
		return builtin
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "警告: %s=%q は正の整数ではありません（既定の %d 秒を使います）\n", envKey, v, builtin)
		return builtin
	}
	return n
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
