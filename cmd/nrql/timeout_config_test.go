package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeConfig は隔離した XDG_CONFIG_HOME に config.yml を置く。
//
// 🚨 テストから本物の ~/.config/newrelic-nrql-cli/config.yml を触らせない
// （config set のテストは実際にファイルを書くため、隔離を忘れると利用者の設定を壊す）。
func writeConfig(t *testing.T, body string) {
	t.Helper()
	resetFileConfigCache(t)
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("NRQL_TIMEOUT", "")
	dir := filepath.Join(root, "newrelic-nrql-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if body == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// resetFileConfigCache は loadFileConfig の sync.Once キャッシュを捨てる。
//
// 🚨 loadFileConfig はプロセス内で 1 度しか読まない（CLI は 1 回の実行で 1 度読めば十分）。
// テストはそれを跨ぐので、リセットしないと**前のテストが書いた config.yml の値**を見て
// 通ったり落ちたりする（実測: 隔離した空の設定を読ませたつもりが 90 が返った）。
// t.Cleanup でも捨てて、この後に走るテストへ持ち越さない。
func resetFileConfigCache(t *testing.T) {
	t.Helper()
	clear := func() {
		fileConfigOnce = sync.Once{}
		fileConfigCached = fileConfig{}
		fileConfigErr = nil
	}
	clear()
	t.Cleanup(clear)
}

// タイムアウトの既定値が「NRQL_TIMEOUT > config.yml > 組み込み既定」の順で決まること。
//
// 🚨 ここが config.yml を飛ばすと、README とヘルプが謳う優先順位と実装が食い違う。
// 食い違っても動くので（既定の 60 秒で動き続ける）、実行結果からは気づけない。
func TestResolveTimeoutPriority(t *testing.T) {
	tests := []struct {
		name string
		env  string
		file int
		want int
	}{
		{name: "環境変数が最優先", env: "30", file: 90, want: 30},
		{name: "環境変数が無ければ config.yml", env: "", file: 90, want: 90},
		{name: "どちらも無ければ既定", env: "", file: 0, want: defaultTimeoutSeconds},
		{name: "環境変数が不正なら config.yml へ落ちる", env: "abc", file: 90, want: 90},
		{name: "環境変数が 0 以下なら config.yml へ落ちる", env: "-1", file: 90, want: 90},
		{name: "config.yml が不正なら既定へ落ちる", env: "", file: -5, want: defaultTimeoutSeconds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NRQL_TIMEOUT", tt.env)
			if got := resolveTimeout(tt.file); got != tt.want {
				t.Errorf("resolveTimeout(%d) = %d, want %d", tt.file, got, tt.want)
			}
		})
	}
}

// config.yml の timeout が -timeout の既定値として実際に届くこと（配線のテスト）。
//
// 🚨 resolveTimeout 単体が正しいことと、registerCommon がそれを呼んでいることは別の主張で、
// 前者のテストは後者を 1 mm も守らない。newFlagSet を通して FlagSet の既定値を見る。
func TestConfigFileTimeoutReachesFlagDefault(t *testing.T) {
	writeConfig(t, "timeout: 90\n")

	var cfg config
	fs := newFlagSet("query", &cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.timeout != 90 {
		t.Errorf("config.yml の timeout が -timeout の既定値に届いていない: %d", cfg.timeout)
	}

	// -timeout の明示指定は config.yml より強い（優先順位の残り半分）。
	var cfg2 config
	fs2 := newFlagSet("query", &cfg2)
	if err := fs2.Parse([]string{"-timeout", "5"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg2.timeout != 5 {
		t.Errorf("-timeout の明示指定が config.yml に負けている: %d", cfg2.timeout)
	}
}

// timeout も 10 進で読む（account と同じ罠）。
//
// 🚨 yaml.v3 に任せると timeout: 060 が 8 進の 48 になる。60 秒のつもりが 48 秒で
// 切れるという、症状から原因へ辿れない形の取り違えになる。
func TestConfigFileTimeoutIsDecimal(t *testing.T) {
	writeConfig(t, "timeout: 060\n")

	if got := int(loadFileConfig().Timeout); got != 60 {
		t.Errorf("timeout: 060 は 10 進の 60 と読むべき: %d", got)
	}
}

// nrql config set timeout の検証と保存。
func TestConfigSetTimeout(t *testing.T) {
	t.Run("正の整数を保存する", func(t *testing.T) {
		writeConfig(t, "")
		if err := cmdConfig([]string{"set", "timeout", "180"}); err != nil {
			t.Fatalf("cmdConfig: %v", err)
		}
		resetFileConfigCache(t) // cmdConfig 内の loadFileConfig が書き込み前の状態を掴んでいる
		if got := int(loadFileConfig().Timeout); got != 180 {
			t.Errorf("保存されていない: %d", got)
		}
	})

	for _, bad := range []string{"0", "-1", "abc", ""} {
		t.Run("不正な値を拒む: "+bad, func(t *testing.T) {
			writeConfig(t, "")
			err := cmdConfig([]string{"set", "timeout", bad})
			var ue *usageError
			if !errors.As(err, &ue) {
				t.Fatalf("usageError を返すべき: %v", err)
			}
			resetFileConfigCache(t)
			if got := int(loadFileConfig().Timeout); got != 0 {
				t.Errorf("拒んだのに書き込まれている: %d", got)
			}
		})
	}
}
