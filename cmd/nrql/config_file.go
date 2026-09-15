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
// （`account: "1577598"`）ため、int だけを受ける実装にすると古い設定ファイルの
// 解析がそこで失敗し、**account だけでなく region / profile まで丸ごと無視される**。
// 書き出しは常に数値（この型の underlying type が int なので）。
type accountID int

func (a *accountID) UnmarshalYAML(value *yaml.Node) error {
	var n int
	if err := value.Decode(&n); err == nil {
		*a = accountID(n)
		return nil
	}
	var str string
	if err := value.Decode(&str); err != nil {
		return fmt.Errorf("account は数値で書いてください: %w", err)
	}
	str = strings.TrimSpace(str)
	if str == "" {
		*a = 0
		return nil
	}
	n, err := strconv.Atoi(str)
	if err != nil {
		return fmt.Errorf("account を数値として解釈できません: %q", str)
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
)

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
		var fc fileConfig
		if err := yaml.Unmarshal(data, &fc); err != nil {
			fmt.Fprintf(os.Stderr, "警告: %s の解析に失敗しました（無視します）: %v\n", path, err)
			return
		}
		fileConfigCached = fc
	})
	return fileConfigCached
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
