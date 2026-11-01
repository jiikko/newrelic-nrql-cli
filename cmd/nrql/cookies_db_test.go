package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jiikko/dotfiles/src/chromecookie"
	"github.com/jiikko/dotfiles/src/chromecookie/chromecookietest"
)

// Cookie DB の読み取りの分類そのもの（壊れた DB・全件復号失敗・-wal の扱い）は chromecookie 側で
// テストしている。ここは newrelic-nrql-cli の写し替え（profileWithoutSessionError / profileDataError）と、
// セッションが見つからなかったときに chromecookie の診断を使うことを、本物の sqlite で固定する。

const testPassword = "right"

// isolateCookieIO は HOME を隔離する（作業領域 ~/Library/Caches/newrelic-nrql-cli もこの下になる）。
func isolateCookieIO(t *testing.T) string {
	t.Helper()
	return fakeChromeHome(t, "Default")
}

type fakeCookieRow = chromecookietest.Cookie

// makeCookieDB は Chrome と同じ列を持つ Cookie DB を profile の Network/Cookies に作る。
func makeCookieDB(t *testing.T, home, profile string, metaVersion int, rows []fakeCookieRow) string {
	t.Helper()
	return chromecookietest.WriteCookieDB(t, home, profile, strconv.Itoa(metaVersion), rows)
}

func encFor(t *testing.T, password, plain string) []byte {
	t.Helper()
	return chromecookietest.Encrypt(t, password, plain, "")
}

func clientFor(profile string) (*client, error) {
	ep, _ := regionEndpoints("us")
	return cookieClientForProfile(ep, "one.newrelic.com", profile, 0)
}

func TestCookieReadFailureClassification(t *testing.T) {
	orig := extractCookiesFn
	t.Cleanup(func() { extractCookiesFn = orig })
	extractCookiesFn = func(profile, host string) (chromecookie.Result, error) {
		return readProfileCookies(profile, host, []byte(testPassword))
	}

	t.Run("Cookie DB が無いのはセッション無し（黙って飛ばす）", func(t *testing.T) {
		isolateCookieIO(t)
		_, err := clientFor("NoSuch")
		var absent *profileWithoutSessionError
		if !errors.As(err, &absent) || !strings.Contains(err.Error(), "NRQL_CHROME_PROFILE") {
			t.Errorf("DB 無しが profileWithoutSessionError（nrql のフラグ名付き）にならない: %T %v", err, err)
		}
	})

	t.Run("壊れた DB は profileDataError（記録して飛ばす）", func(t *testing.T) {
		home := isolateCookieIO(t)
		dir := filepath.Join(chromecookietest.ProfileDir(home, "Default"), "Network")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Cookies"), []byte(strings.Repeat("garbage ", 100)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := clientFor("Default")
		var pd *profileDataError
		if !errors.As(err, &pd) {
			t.Errorf("壊れた DB が profileDataError にならない: %T %v", err, err)
		}
	})

	t.Run("全件の復号失敗は profileDataError（セッション無しに化けない）", func(t *testing.T) {
		home := isolateCookieIO(t)
		makeCookieDB(t, home, "Default", 0, []fakeCookieRow{
			{Host: ".newrelic.com", Name: "session", Enc: encFor(t, "wrong", "v")},
			{Host: ".newrelic.com", Name: "other", Enc: encFor(t, "wrong", "w")},
		})
		_, err := clientFor("Default")
		var pd *profileDataError
		if !errors.As(err, &pd) || !strings.Contains(err.Error(), "復号") {
			t.Errorf("全件の復号失敗が profileDataError にならない: %T %v", err, err)
		}
	})

	t.Run("1 件だけの復号失敗は続行する", func(t *testing.T) {
		home := isolateCookieIO(t)
		makeCookieDB(t, home, "Default", 0, []fakeCookieRow{
			{Host: ".newrelic.com", Name: "bad", Enc: encFor(t, "wrong", "v")},
			{Host: ".newrelic.com", Name: "session", Enc: encFor(t, testPassword, "good")},
		})
		c, err := clientFor("Default")
		if err != nil || !strings.Contains(c.cookie, "session=good") {
			t.Errorf("1 件の失敗で止めた / 正しい Cookie が入っていない: %v", err)
		}
	})

	t.Run("-wal が読めずセッションも無いなら profileDataError（WAL の Cookie を黙って落とさない）", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root では権限で読み取りを失敗させられない")
		}
		home := isolateCookieIO(t)
		src := makeCookieDB(t, home, "Default", 0, nil)
		if err := os.WriteFile(src+"-wal", []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
		_, err := clientFor("Default")
		var pd *profileDataError
		if !errors.As(err, &pd) || !strings.Contains(err.Error(), "Cookies-wal") {
			t.Errorf("読めない -wal を理由に添えていない: %T %v", err, err)
		}
	})

	t.Run("-wal が読めなくても本体にセッションがあれば続行", func(t *testing.T) {
		home := isolateCookieIO(t)
		src := makeCookieDB(t, home, "Default", 0, []fakeCookieRow{{Host: ".newrelic.com", Name: "session", Value: "plain"}})
		if err := os.Mkdir(src+"-wal", 0o700); err != nil { // ReadFile が EISDIR で失敗する
			t.Fatal(err)
		}
		if _, err := clientFor("Default"); err != nil {
			t.Errorf("取れたセッションがあるのに止めた: %v", err)
		}
	})

	t.Run("Cookie DB の読み取りの権限エラーは profileDataError（権限と分かる形で・案内付き）", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root では権限で読み取りを失敗させられない")
		}
		home := isolateCookieIO(t)
		src := makeCookieDB(t, home, "Default", 0, nil)
		if err := os.Chmod(src, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(src, 0o600) })
		_, err := clientFor("Default")
		var pd *profileDataError
		if !errors.As(err, &pd) || !errors.Is(err, fs.ErrPermission) {
			t.Errorf("権限エラーを辿れる profileDataError にしていない: %T %v", err, err)
		}
		if !strings.Contains(err.Error(), "フルディスクアクセス") || !strings.Contains(err.Error(), "パーミッション") {
			t.Errorf("フルディスクアクセスとパーミッションの両方の案内が無い: %v", err)
		}
	})
}

// 他サイトのセッション Cookie を復号しない: readProfileCookies は host 宛てに送られうる host_key の行だけを読む。
// 除外される行も正しい鍵で暗号化してあるので、絞らなければ結果に現れる。
func TestReadProfileCookiesReadsOnlyHostCookies(t *testing.T) {
	home := isolateCookieIO(t)
	enc := func(plain, host string) []byte { return chromecookietest.Encrypt(t, testPassword, plain, host) }
	makeCookieDB(t, home, "Default", 24, []fakeCookieRow{
		{Host: ".newrelic.com", Name: "session", Enc: enc("S", ".newrelic.com")},
		{Host: "notnewrelic.com", Name: "lookalike", Enc: enc("N", "notnewrelic.com")},
		{Host: ".slack.com", Name: "d", Enc: enc("SLACK", ".slack.com")},
	})
	res, err := readProfileCookies("Default", "one.newrelic.com", []byte(testPassword))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cookies) != 1 || res.Cookies[0].Host != ".newrelic.com" || res.Cookies[0].Name != "session" {
		t.Errorf("New Relic 宛ての Cookie だけを返すべき: %+v", res.Cookies)
	}
}

// cookieClientForProfile が送信先の host を取り出し口へ渡していること。
func TestCookieClientPassesHostToExtract(t *testing.T) {
	orig := extractCookiesFn
	t.Cleanup(func() { extractCookiesFn = orig })
	extractCookiesFn = func(profile, host string) (chromecookie.Result, error) {
		return readProfileCookies(profile, host, []byte(testPassword))
	}

	t.Run("host-only の one.newrelic.com の Cookie を読む（送信先を取り違えると読まれない）", func(t *testing.T) {
		home := isolateCookieIO(t)
		makeCookieDB(t, home, "Default", 24, []fakeCookieRow{
			{Host: "one.newrelic.com", Name: "session", Enc: chromecookietest.Encrypt(t, testPassword, "H", "one.newrelic.com")},
		})
		c, err := clientFor("Default")
		if err != nil || !strings.Contains(c.cookie, "session=H") {
			t.Errorf("one.newrelic.com の host-only の Cookie を読めない: err=%v", err)
		}
	})

	t.Run("他サイトの行しか無く鍵違いでも、復号失敗ではなくセッション無し", func(t *testing.T) {
		home := isolateCookieIO(t)
		makeCookieDB(t, home, "Default", 24, []fakeCookieRow{
			{Host: ".slack.com", Name: "d", Enc: chromecookietest.Encrypt(t, "wrong", "x", ".slack.com")},
		})
		_, err := clientFor("Default")
		var absent *profileWithoutSessionError
		if !errors.As(err, &absent) {
			t.Errorf("他サイトの行を復号しようとした（セッション無しになるべき）: %T %v", err, err)
		}
	})
}
