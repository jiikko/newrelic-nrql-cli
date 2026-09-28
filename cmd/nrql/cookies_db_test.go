package main

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateCookieIO は HOME と TMPDIR を隔離する。
//
// 🚨 copyCookieDB は os.TempDir() 配下に一時ディレクトリを作って消す（破壊的操作）。
// 本物の $TMPDIR/nrql-cookie を触らせないよう、TMPDIR もテストの一時領域へ向ける。
func isolateCookieIO(t *testing.T) string {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	return fakeChromeHome(t, "Default")
}

type fakeCookieRow struct {
	host, name, value string
	enc               []byte
}

// makeCookieDB は Chrome と同じ列を持つ Cookie DB を profile の Network/Cookies に作る。
func makeCookieDB(t *testing.T, home, profile string, metaVersion int, rows []fakeCookieRow) string {
	t.Helper()
	dir := filepath.Join(home, "Library", "Application Support", chromeSupportSubdir, profile, "Network")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Cookies")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE meta (key TEXT, value TEXT)`,
		`CREATE TABLE cookies (host_key TEXT, name TEXT, value TEXT, encrypted_value BLOB)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO meta VALUES ('version', ?)`, metaVersion); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO cookies VALUES (?, ?, ?, ?)`, r.host, r.name, r.value, r.enc); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// Keychain を読めた後の失敗は「このプロファイル固有の問題」（profileDataError）として返し、
// 全件の復号失敗を「Cookie 0 件」に化けさせないこと。
func TestReadProfileCookiesClassifiesProfileProblems(t *testing.T) {
	key, err := deriveKey([]byte("right"))
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := deriveKey([]byte("wrong"))
	if err != nil {
		t.Fatal(err)
	}
	plain := withHostHash(".newrelic.com", "session-value")

	t.Run("正しい鍵なら読める（対照）", func(t *testing.T) {
		home := isolateCookieIO(t)
		makeCookieDB(t, home, "Default", 24, []fakeCookieRow{
			{host: ".newrelic.com", name: "session", enc: encryptForTest(t, key, plain)},
		})
		got, err := readProfileCookies("Default", key)
		if err != nil {
			t.Fatalf("readProfileCookies: %v", err)
		}
		if len(got) != 1 || got[0].value != "session-value" {
			t.Errorf("復号結果が違う: %+v", got)
		}
	})

	t.Run("全件復号できないのは 0 件ではない", func(t *testing.T) {
		home := isolateCookieIO(t)
		makeCookieDB(t, home, "Default", 24, []fakeCookieRow{
			{host: ".newrelic.com", name: "session", enc: encryptForTest(t, wrong, []byte("v"))},
			{host: ".newrelic.com", name: "other", enc: encryptForTest(t, wrong, []byte("w"))},
		})
		got, err := readProfileCookies("Default", key)
		var pd *profileDataError
		if !errors.As(err, &pd) {
			t.Fatalf("profileDataError を返すべき: got %d 件, err=%v", len(got), err)
		}
		if !strings.Contains(err.Error(), "2 件") {
			t.Errorf("試した件数が案内に無い: %v", err)
		}
	})

	t.Run("鍵違いで PKCS7 が偶然通る Cookie が混ざっても全件復号失敗と分かる", func(t *testing.T) {
		// 🚨 短い平文だけの fixture では、鍵違いの復号結果が 32 バイトに届かず必ず失敗するので、
		// ドメインハッシュを照合しない実装でも通ってしまう（検査が壊れない形）。実際の Chrome の値は
		// 長く、件数も多いので、PKCS7 が偶然通る Cookie が必ず混ざる。それを決定的に 1 件入れる。
		home := isolateCookieIO(t)
		rows := []fakeCookieRow{{host: ".newrelic.com", name: "lucky", enc: luckyWrongKeyCiphertext(t, wrong, key, ".newrelic.com")}}
		long := strings.Repeat("x", 200)
		for i := 0; i < 50; i++ {
			rows = append(rows, fakeCookieRow{host: ".newrelic.com", name: fmt.Sprintf("c%d", i),
				enc: encryptForTest(t, wrong, withHostHash(".newrelic.com", long))})
		}
		makeCookieDB(t, home, "Default", 24, rows)
		got, err := readProfileCookies("Default", key)
		var pd *profileDataError
		if !errors.As(err, &pd) {
			t.Fatalf("全件鍵違いなのに profileDataError にならない: got %d 件, err=%v", len(got), err)
		}
	})

	t.Run("1 件だけの復号失敗は続行する", func(t *testing.T) {
		home := isolateCookieIO(t)
		makeCookieDB(t, home, "Default", 24, []fakeCookieRow{
			{host: ".newrelic.com", name: "bad", enc: encryptForTest(t, wrong, []byte("v"))},
			{host: ".newrelic.com", name: "session", enc: encryptForTest(t, key, plain)},
		})
		got, err := readProfileCookies("Default", key)
		if err != nil {
			t.Fatalf("1 件の失敗で全体を止めてはいけない: %v", err)
		}
		if len(got) != 1 || got[0].name != "session" {
			t.Errorf("復号できた Cookie だけ返すべき: %+v", got)
		}
	})

	t.Run("sqlite でないファイル", func(t *testing.T) {
		home := isolateCookieIO(t)
		dir := filepath.Join(home, "Library", "Application Support", chromeSupportSubdir, "Default", "Network")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Cookies"), []byte("not a database, just text......................................................................................................"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := readProfileCookies("Default", key)
		var pd *profileDataError
		if !errors.As(err, &pd) {
			t.Fatalf("profileDataError を返すべき: %T %v", err, err)
		}
	})
}

// luckyWrongKeyCiphertext は encKey で暗号化された長い Cookie のうち、readKey で復号すると
// PKCS7 の末尾が偶然正しく見える（最終バイトが 0x01）ものを 1 つ返す。
// 鍵と候補の並びが固定なので結果は決定的（期待値 256 回程度で見つかる）。
// 判定は production の pkcs7Unpad を使わず、AES の生の復号結果で独立に行う。
func luckyWrongKeyCiphertext(t *testing.T, encKey, readKey []byte, hostKey string) []byte {
	t.Helper()
	block, err := aes.NewCipher(readKey)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100000; i++ {
		enc := encryptForTest(t, encKey, withHostHash(hostKey, fmt.Sprintf("%0200d", i)))
		ct := enc[3:]
		plain := make([]byte, len(ct))
		cipher.NewCBCDecrypter(block, []byte("                ")).CryptBlocks(plain, ct)
		if plain[len(plain)-1] == 0x01 {
			return enc
		}
	}
	t.Fatal("PKCS7 が偶然通る暗号文が見つからない（fixture の前提が崩れた）")
	return nil
}

// -wal / -shm は ENOENT のときだけ無視し、それ以外で読めないのを続行しないこと。
//
// 🚨 続行すると、WAL にしか無いセッション Cookie が落ちて「0 件」になり、自動検出が黙って飛ばす。
func TestCopyCookieDBDoesNotIgnoreUnreadableWAL(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root では権限で読み取りを失敗させられない")
	}
	key, err := deriveKey([]byte("right"))
	if err != nil {
		t.Fatal(err)
	}
	newDB := func(t *testing.T) string {
		home := isolateCookieIO(t)
		return makeCookieDB(t, home, "Default", 0, []fakeCookieRow{
			{host: ".newrelic.com", name: "session", value: "plain"},
		})
	}

	t.Run("WAL が無いのは正常", func(t *testing.T) {
		newDB(t)
		if _, err := readProfileCookies("Default", key); err != nil {
			t.Fatalf("WAL が無いだけでエラーにしている: %v", err)
		}
	})

	t.Run("WAL が権限で読めないのはプロファイル固有の問題（権限と分かる形で）", func(t *testing.T) {
		src := newDB(t)
		if err := os.WriteFile(src+"-wal", []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
		_, err := readProfileCookies("Default", key)
		if err == nil {
			t.Fatal("読めない WAL を無視している")
		}
		var pd *profileDataError
		if !errors.As(err, &pd) {
			t.Errorf("権限はプロファイル固有の問題（profileDataError）として返すべき: %T %v", err, err)
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("権限であることを辿れない: %v", err)
		}
		if !strings.Contains(err.Error(), "フルディスクアクセス") || !strings.Contains(err.Error(), "パーミッション") {
			t.Errorf("フルディスクアクセスとパーミッションの両方の案内が無い: %v", err)
		}
	})

	t.Run("WAL が権限以外で読めないのはプロファイル固有の問題", func(t *testing.T) {
		src := newDB(t)
		if err := os.Mkdir(src+"-wal", 0o700); err != nil { // ReadFile が EISDIR で失敗する
			t.Fatal(err)
		}
		_, err := readProfileCookies("Default", key)
		var pd *profileDataError
		if !errors.As(err, &pd) {
			t.Fatalf("profileDataError を返すべき: %T %v", err, err)
		}
	})
}
