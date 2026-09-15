package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// fileConfig は config.yml の内容。すべて任意項目。
type fileConfig struct {
	Account string `yaml:"account,omitempty"` // 既定のアカウント ID
	Region  string `yaml:"region,omitempty"`  // us / eu
	Profile string `yaml:"profile,omitempty"` // Chrome のプロファイル名
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
