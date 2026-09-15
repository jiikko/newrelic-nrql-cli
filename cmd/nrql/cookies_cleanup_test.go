package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 層①: 登録した一時ディレクトリが runAllCleanups で消えること。
func TestRunAllCleanupsRemovesRegisteredDirs(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	root, err := ensureCookieTempRoot()
	if err != nil {
		t.Fatalf("ensureCookieTempRoot: %v", err)
	}
	dir, err := os.MkdirTemp(root, fmt.Sprintf("%d-", os.Getpid()))
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cookies"), []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	registerCleanup(dir)

	runAllCleanups()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("一時ディレクトリが残っている: %s (err=%v)", dir, err)
	}
}

// 層③: 強制終了で残った残骸だけを消し、それ以外は触らないこと。
//
// 破壊的操作なので、消してよいもの / 触ってはいけないものを 1 つのテーブルで固定する。
func TestSweepStaleCookieDirsOnlyRemovesDeadPidDirs(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	root, err := ensureCookieTempRoot()
	if err != nil {
		t.Fatalf("ensureCookieTempRoot: %v", err)
	}

	// 確実に死んでいる pid を作る（起動して終了を待ち、回収済みにする）。
	done := exec.Command("/usr/bin/true")
	if err := done.Run(); err != nil {
		t.Fatalf("ヘルパープロセスの起動に失敗: %v", err)
	}
	deadPid := done.Process.Pid
	// 🚨 「死んでいること」の確認に production の processAlive を使わない。
	// 使うと production のバグ（死んだ pid を生きていると誤判定する）が skip に化け、
	// スイートは green のまま退行を通す（実測で踏んだ）。ここは kill(2) を直接呼ぶ。
	if err := syscall.Kill(deadPid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Skipf("pid %d が再利用された可能性があるため判定不能 (err=%v)", deadPid, err)
	}
	if processAlive(deadPid) {
		t.Fatalf("processAlive が死んだ pid %d を「生きている」と誤判定した", deadPid)
	}

	mk := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "Cookies"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	stale := mk(fmt.Sprintf("%d-abc123", deadPid))    // 消える: 死んだ pid
	mine := mk(fmt.Sprintf("%d-xyz789", os.Getpid())) // 残る: 自分のもの（実行中）
	alive := mk("1-alive")                            // 残る: pid 1 (launchd) は常に生きている
	notPid := mk("nrql-cookie-old")                   // 残る: 名前の形が違う（旧版が作ったもの）
	noDash := mk("12345")                             // 残る: ハイフンが無い
	plainFile := filepath.Join(root, "9999999-file")  // 残る: ディレクトリではない
	if err := os.WriteFile(plainFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	sweepStaleCookieDirs()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("死んだ pid の残骸が消えていない: %s", stale)
	}
	for _, keep := range []string{mine, alive, notPid, noDash, plainFile} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("触ってはいけないものが消えた: %s (err=%v)", keep, err)
		}
	}
}

// 層③の前提: pid <= 0 を kill(2) に渡さないこと。
// 0 は自分のプロセスグループ宛てになり「生きている」に化けるため、判定に使えない。
func TestProcessAliveRejectsNonPositivePid(t *testing.T) {
	for _, pid := range []int{0, -1, -12345} {
		if !processAlive(pid) {
			t.Errorf("pid %d は判定不能として「生きている」扱いにすべき（消さない方へ倒す）", pid)
		}
	}
}

// 層③の前提: 名前の形の判定。
func TestPidFromTempDirName(t *testing.T) {
	cases := []struct {
		name    string
		wantPid int
		wantOK  bool
	}{
		{"1234-abc", 1234, true},
		{"1-x", 1, true},
		{"nrql-cookie-old", 0, false}, // 旧版が作った名前は対象外
		{"12345", 0, false},           // ハイフン無し
		{"-abc", 0, false},            // pid 部が空
		{"abc-123", 0, false},         // pid が数値でない
		{"0-abc", 0, false},           // pid 0 は不正
		{"-1-abc", 0, false},          // 負の pid
	}
	for _, c := range cases {
		gotPid, gotOK := pidFromTempDirName(c.name)
		if gotOK != c.wantOK || (gotOK && gotPid != c.wantPid) {
			t.Errorf("%q: got (%d, %v), want (%d, %v)", c.name, gotPid, gotOK, c.wantPid, c.wantOK)
		}
	}
}

// 層②: SIGINT を受けたとき、defer が走らない状況でも一時ディレクトリが消えること。
//
// 自プロセスに SIGINT を撃つとテストバイナリごと死ぬので、子プロセスとして自分自身を
// 起動して確かめる（`-test.run` で同じテストを走らせ、環境変数でヘルパーモードに入る）。
func TestSignalCleanupRemovesTempDirOnSIGINT(t *testing.T) {
	if os.Getenv("NRQL_TEST_SIGNAL_HELPER") == "1" {
		signalCleanupHelper()
		return
	}

	tmpRoot := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestSignalCleanupRemovesTempDirOnSIGINT")
	cmd.Env = append(os.Environ(), "NRQL_TEST_SIGNAL_HELPER=1", "TMPDIR="+tmpRoot)
	out, err := cmd.Output()

	// 終了コードは 130（128 + SIGINT）であるべき。
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("子プロセスがシグナル経路で終了しなかった: err=%v out=%q", err, out)
	}
	if code := ee.ExitCode(); code != 130 {
		t.Errorf("終了コードが 130 ではない: %d（シグナルで即死していると -1 になる）", code)
	}

	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatalf("子プロセスが一時ディレクトリのパスを出力しなかった: %q", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("SIGINT の後に一時ディレクトリが残っている: %s (err=%v)", dir, err)
	}
}

// signalCleanupHelper は子プロセス側。一時ディレクトリを作って登録し、
// ハンドラを仕掛けてから自分に SIGINT を撃つ。
func signalCleanupHelper() {
	root, err := ensureCookieTempRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ensureCookieTempRoot:", err)
		os.Exit(3)
	}
	dir, err := os.MkdirTemp(root, fmt.Sprintf("%d-", os.Getpid()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "MkdirTemp:", err)
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cookies"), []byte("dummy"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "WriteFile:", err)
		os.Exit(3)
	}
	registerCleanup(dir)
	installCleanupOnSignal()

	fmt.Println(dir) // 親へパスを渡す
	_ = os.Stdout.Sync()

	_ = syscall.Kill(os.Getpid(), syscall.SIGINT)

	// ハンドラが os.Exit するまで待つ。ここを抜けたらハンドラが機能していない。
	time.Sleep(10 * time.Second)
	fmt.Fprintln(os.Stderr, "SIGINT ハンドラが終了させなかった")
	os.Exit(4)
}
