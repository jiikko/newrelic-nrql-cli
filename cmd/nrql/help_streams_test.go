package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr は fn の実行中にプロセスの os.Stderr へ書かれたものを集める。
//
// 🚨 buffer を fs.SetOutput() で差すだけでは足りない。退行の実体（v0.1.5）は
// fs.Usage が **os.Stderr へ直接** 書く形だったので、FlagSet の output を
// 差し替えただけのテストはその形を素通りさせる。fn の中で newFlagSet を呼ぶこと
// （newFlagSet は構築時の os.Stderr の値を FlagSet に焼き込む）。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	func() {
		defer func() {
			os.Stderr = orig
			_ = w.Close()
		}()
		fn()
	}()
	out := <-done
	_ = r.Close()
	return out
}

// isolateEnv は config.yml / 環境変数の既定値に左右されないようにする。
// registerCommon は不正な既定値を見つけると os.Stderr へ警告を出すため、
// 隔離しないとその警告が stderr の assert を汚す。
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("NEW_RELIC_ACCOUNT_ID", "")
	t.Setenv("NRQL_TIMEOUT", "")
}

// --help は stdout だけに出す（stderr には 1 バイトも出さない）ことを固定する。
//
// 🚨 flag は ErrHelp を返す**前に** fs.Usage を自分で呼ぶ。fs.Usage が usage を
// 出す実装だと、parseArgs が stdout へ出す分と合わせて**両方**に全文が出る
// （実測 v0.1.5: nrql query --help が stdout 22 行 / stderr 22 行で同一内容）。
// stdout 側だけを見るテストではこの退行を検出できない。
func TestParseArgsHelpGoesToStdoutOnly(t *testing.T) {
	isolateEnv(t)

	var stdout bytes.Buffer
	var done bool
	var err error
	stderr := captureStderr(t, func() {
		var cfg config
		fs := newFlagSet("query", &cfg) // 本番の配線（no-op の fs.Usage）を通す
		done, err = parseArgs(fs, queryHelp, []string{"--help"}, &stdout)
	})

	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !done {
		t.Fatal("--help は helpRequested=true を返すべき")
	}
	if got := stdout.String(); got != queryHelp {
		t.Errorf("stdout に help 全文が出るべき:\n%q", got)
	}
	if stderr != "" {
		t.Errorf("--help で stderr に出してはいけない（%d バイト出ている）:\n%q", len(stderr), stderr)
	}
}

// フラグの誤りでは逆に stderr だけに出す（stdout に混ざるとパイプが壊れる）。
//
// --help を stdout 専用にするために usage の出力元を parseArgs へ寄せたので、
// 「誤ったフラグのときに usage が出る」側が落ちていないことを同時に固定する。
func TestParseArgsFlagErrorGoesToStderrOnly(t *testing.T) {
	isolateEnv(t)

	var stdout bytes.Buffer
	var done bool
	var err error
	stderr := captureStderr(t, func() {
		var cfg config
		fs := newFlagSet("query", &cfg)
		done, err = parseArgs(fs, queryHelp, []string{"-bogus", "x"}, &stdout)
	})

	if done {
		t.Error("フラグの誤りは helpRequested=false であるべき")
	}
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("usageError を返すべき: %v", err)
	}
	if got := stdout.String(); got != "" {
		t.Errorf("フラグの誤りで stdout に出してはいけない:\n%q", got)
	}
	if !strings.Contains(stderr, queryHelp) {
		t.Errorf("stderr に usage が出るべき:\n%q", stderr)
	}
	if !strings.Contains(stderr, "-bogus") {
		t.Errorf("stderr に flag のエラー文が出るべき:\n%q", stderr)
	}
}
