package main

import (
	"strings"
	"testing"

	"github.com/jiikko/dotfiles/src/chromecookie"
)

// 復号（鍵導出・PKCS7・v24 のホストハッシュ）のテストは chromecookie 側にある。
// ここは newrelic-nrql-cli が決めていること（どの Cookie を送るか）と、送信先の照合の回帰表だけ。

// 送信先ホストの判定。ここが緩むと **無関係なサイトのセッションを New Relic へ送る**、
// 逆に厳しすぎると正当な Cookie を取りこぼしてログイン済みなのに認証が通らない。
//
// 敵対的レビューが試した形（公開サフィックス・部分一致・多重ドット・大文字）を
// そのまま回帰テーブルにしてある。
func TestCookieHostMatches(t *testing.T) {
	const req = "one.newrelic.com"
	cases := []struct {
		hostKey string
		want    bool
		why     string
	}{
		{hostKey: "one.newrelic.com", want: true, why: "host-only の完全一致"},
		{hostKey: ".newrelic.com", want: true, why: "ドメイン Cookie（サブドメインへ送られる）"},
		{hostKey: ".one.newrelic.com", want: true, why: "自分自身を含むドメイン Cookie"},
		{hostKey: "newrelic.com", want: false, why: "host-only は完全一致のみ（親ドメインは送らない）"},
		{hostKey: "two.newrelic.com", want: false, why: "別のサブドメイン"},
		{hostKey: "newrelic.com.evil.jp", want: false, why: "接尾辞の偽装"},
		{hostKey: ".newrelic.com.evil.jp", want: false, why: "接尾辞の偽装（ドメイン Cookie）"},
		{hostKey: "evil-one.newrelic.com", want: false, why: "ドット境界を跨がない部分一致"},
		{hostKey: ".com", want: true, why: "公開サフィックスは検査していない（既知の緩さ。下の注記参照）"},
		{hostKey: "", want: false, why: "空"},
		{hostKey: ".", want: false, why: "ドットのみ"},
		{hostKey: "..", want: false, why: "ドット 2 つ"},
		{hostKey: "..com", want: false, why: "多重ドット"},
		{hostKey: ".COM", want: false, why: "大文字（host_key は小文字で保存される）"},
		{hostKey: ".jp", want: false, why: "無関係な TLD"},
		{hostKey: "one.newrelic.com.", want: false, why: "末尾ドット"},
	}
	for _, c := range cases {
		if got := chromecookie.HostMatches(c.hostKey, req); got != c.want {
			t.Errorf("HostMatches(%q, %q) = %v, want %v（%s）", c.hostKey, req, got, c.want, c.why)
		}
	}
}

// buildCookieHeader は同名 Cookie が複数あるとき「より具体的なホスト」を採る。
//
// 🚨 ここが壊れると、別サブドメインの古い値でセッションを上書きして
// 「ログインしているのに 401」になる。壊れ方が認証エラーとして出るので原因が分かりにくい。
func TestBuildCookieHeaderPrefersMoreSpecificHost(t *testing.T) {
	// 🚨 正解（host-only）を先頭に置かないこと。
	// 先頭に置くと「最初に見つけたものを採る」実装でも通ってしまい、
	// 優先順位のロジックを壊す変異を検出できない（実測で素通りした）。
	// ここでは同じ rank になりうる .one.newrelic.com を先に置いてある。
	entries := []cookieEntry{
		{Host: ".newrelic.com", Name: "session", Value: "domain-wide"},
		{Host: ".one.newrelic.com", Name: "session", Value: "sub-domain"},
		{Host: "one.newrelic.com", Name: "session", Value: "host-only"},
		{Host: "evil.example.jp", Name: "session", Value: "unrelated"},
		{Host: ".newrelic.com", Name: "other", Value: "keep"},
	}
	header, n := buildCookieHeader(entries, "one.newrelic.com")

	if n != 2 {
		t.Errorf("送るべき Cookie は 2 種類（session / other）: got %d（%q）", n, header)
	}
	if !strings.Contains(header, "session=host-only") {
		t.Errorf("host-only の Cookie を最優先すべき: %q", header)
	}
	for _, ng := range []string{"domain-wide", "sub-domain", "unrelated"} {
		if strings.Contains(header, ng) {
			t.Errorf("%q が混ざっている: %q", ng, header)
		}
	}
	if !strings.Contains(header, "other=keep") {
		t.Errorf("別名の Cookie が落ちている: %q", header)
	}

	// 無関係なホスト宛てには 1 つも送らない。
	if h, n := buildCookieHeader(entries, "example.com"); n != 0 || h != "" {
		t.Errorf("無関係なホストへ送ろうとしている: n=%d header=%q", n, h)
	}
}
