package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// root の help は概要とサブコマンドの一覧だけにし、詳細（オプション・環境変数・終了コード・注意）は各サブコマンドの help に置くこと。
// root に詳細を書き足すと、サブコマンドの help と二重になって片方だけ直る。
func TestTopUsageIsSummaryOnly(t *testing.T) {
	for _, sub := range []string{"query", "accounts", "config", "help"} {
		if !strings.Contains(topUsage, "\n  "+sub+" ") {
			t.Errorf("root の help にサブコマンド %q が無い", sub)
		}
	}
	for _, detail := range []string{"終了コード:", "環境変数:", "注意:", "-region <", "-timeout <", "優先順位:"} {
		if strings.Contains(topUsage, detail) {
			t.Errorf("root の help に詳細 %q がある（各サブコマンドの help に置く）", detail)
		}
	}
	if n := strings.Count(topUsage, "\n"); n > 20 {
		t.Errorf("root の help が %d 行ある（概要だけにする）", n)
	}
}

// New Relic に問い合わせるサブコマンドの help は、それだけで共通オプション・環境変数・終了コード・注意が分かること（root を参照させない）。
func TestSubcommandHelpsCarryCommonDetails(t *testing.T) {
	for name, h := range map[string]string{"query": queryHelp, "accounts": accountsHelp} {
		for _, want := range []string{"-region <", "-timeout <", "-profile <", "環境変数:", "終了コード:", "注意:"} {
			if !strings.Contains(h, want) {
				t.Errorf("nrql %s --help に %q が無い", name, want)
			}
		}
		if strings.Contains(h, "nrql --help を参照") {
			t.Errorf("nrql %s --help が root の help を参照している", name)
		}
	}
}

// captureStdout は fn の間の os.Stdout を取る。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	func() {
		defer func() {
			os.Stdout = orig
			_ = w.Close()
		}()
		fn()
	}()
	out := <-done
	_ = r.Close()
	return out
}

// nrql config の --help は、どの位置でもヘルプを stdout へ出して rc=0 にし、何も保存しないこと。
// 以前は config --help が「不明なサブコマンド "--help"」、config set --help が使い方エラーだった。
func TestConfigHelp(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"-h"}, {"-help"}, {"--h"}, {"help"},
		{"set", "--help"}, {"set", "account", "-h"}, {"show", "--help"}, {"path", "--help"},
	} {
		isolateEnv(t)
		var err error
		var stdout string
		stderr := captureStderr(t, func() { stdout = captureStdout(t, func() { err = cmdConfig(args) }) })
		if err != nil || stdout != configHelp || stderr != "" {
			t.Errorf("nrql config %v: ヘルプが stdout に出ていない（err=%v stderr=%q）", args, err, stderr)
		}
		if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "newrelic-nrql-cli", "config.yml")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("nrql config %v: ヘルプを求められたのに config.yml を書いた: %v", args, err)
		}
	}
}
