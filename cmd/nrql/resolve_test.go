package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// プロファイル選択の優先順位を固定する。
//
// 🚨 ここが入れ替わると「意図しないアカウントのデータを見ている」ことになる。
// しかも結果は普通に返るので、見ている対象が違うことに気づけない。
func TestResolveClientPriority(t *testing.T) {
	// 本物は Keychain と Chrome の保存領域を読むので、seam を差し替える。
	var asked []string
	restore := func() func() {
		orig := extractCookiesFn
		extractCookiesFn = func(profile string) ([]cookieEntry, error) {
			asked = append(asked, profile)
			return []cookieEntry{{host: ".newrelic.com", name: "session", value: "v-" + profile}}, nil
		}
		return func() { extractCookiesFn = orig }
	}()
	defer restore()

	t.Run("API キーがあればブラウザを読まない", func(t *testing.T) {
		asked = nil
		t.Setenv("NEW_RELIC_API_KEY", "NRAK-XXX")

		c, err := resolveClient(config{region: "us", profile: profileAuto})
		if err != nil {
			t.Fatalf("resolveClient: %v", err)
		}
		if c.mode != authAPIKey {
			t.Errorf("API キー方式になるべき: mode=%v", c.mode)
		}
		if c.endpoint != regions["us"].apiKey {
			t.Errorf("公開 NerdGraph を使うべき: %s", c.endpoint)
		}
		if len(asked) != 0 {
			t.Errorf("ブラウザの保存領域を読んでいる: %v", asked)
		}
	})

	t.Run("明示したプロファイルをそのまま使う", func(t *testing.T) {
		asked = nil
		t.Setenv("NEW_RELIC_API_KEY", "")

		c, err := resolveClient(config{region: "us", profile: "Profile 3"})
		if err != nil {
			t.Fatalf("resolveClient: %v", err)
		}
		if c.profile != "Profile 3" {
			t.Errorf("指定したプロファイルが使われていない: %q", c.profile)
		}
		if len(asked) != 1 || asked[0] != "Profile 3" {
			t.Errorf("読んだプロファイルが違う: %v", asked)
		}
		if c.mode != authCookie {
			t.Errorf("セッション方式になるべき: mode=%v", c.mode)
		}
	})

	t.Run("リージョンで接続先が変わる", func(t *testing.T) {
		t.Setenv("NEW_RELIC_API_KEY", "")
		c, err := resolveClient(config{region: "eu", profile: "Default"})
		if err != nil {
			t.Fatalf("resolveClient: %v", err)
		}
		if c.endpoint != regions["eu"].session {
			t.Errorf("EU の接続先になっていない: %s", c.endpoint)
		}
	})

	t.Run("未対応のリージョンは弾く", func(t *testing.T) {
		t.Setenv("NEW_RELIC_API_KEY", "")
		if _, err := resolveClient(config{region: "jp", profile: "Default"}); err == nil {
			t.Error("エラーになるべき")
		}
	})

	t.Run("対象ホスト宛てのセッションが無ければエラー", func(t *testing.T) {
		t.Setenv("NEW_RELIC_API_KEY", "")
		orig := extractCookiesFn
		extractCookiesFn = func(profile string) ([]cookieEntry, error) {
			return []cookieEntry{{host: "example.com", name: "x", value: "y"}}, nil
		}
		defer func() { extractCookiesFn = orig }()

		if _, err := resolveClient(config{region: "us", profile: "Default"}); err == nil {
			t.Error("無関係な Cookie しか無ければエラーになるべき")
		}
	})
}

// 候補が複数あるときは「認証が通る最初のもの」を選び、選んだことを stderr に出すこと。
func TestPickAuthenticatedClient(t *testing.T) {
	newFake := func(profile string, ok bool) *client {
		c := &client{
			mode:     authCookie,
			endpoint: "https://example.invalid/graphql",
			profile:  profile,
			http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if !ok {
					return &http.Response{
						StatusCode: http.StatusUnauthorized,
						Body:       http.NoBody,
						Request:    r,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       nopCloser{strings.NewReader(`{"data":{"actor":{"user":{"name":"x"}}}}`)},
					Request:    r,
				}, nil
			})},
		}
		return c
	}

	t.Run("認証が通る候補を選ぶ", func(t *testing.T) {
		// 🚨 正解を先頭に置かない（先頭だと「最初の候補を返す」実装でも通る）。
		got, err := pickAuthenticatedClient([]*client{
			newFake("Profile 1", false),
			newFake("Profile 2", true),
		}, "one.newrelic.com")
		if err != nil {
			t.Fatalf("pickAuthenticatedClient: %v", err)
		}
		if got.profile != "Profile 2" {
			t.Errorf("認証が通る候補を選ぶべき: %q", got.profile)
		}
	})

	t.Run("全部失敗したらエラー", func(t *testing.T) {
		_, err := pickAuthenticatedClient([]*client{
			newFake("Profile 1", false),
			newFake("Profile 2", false),
		}, "one.newrelic.com")
		if err == nil {
			t.Fatal("エラーになるべき")
		}
		if !strings.Contains(err.Error(), "2 件") {
			t.Errorf("試した件数が案内に無い: %v", err)
		}
	})
}

// 終了コードの出し分け。スクリプトから使うときの契約。
func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "成功", err: nil, want: exitOK},
		{name: "使い方の誤り", err: &usageError{"bad flag"}, want: exitUsage},
		{name: "実行時エラー", err: errors.New("boom"), want: exitRuntime},
		{name: "セッション切れは実行時エラー", err: &errSessionExpired{status: 401}, want: exitRuntime},
		{name: "NerdGraph のエラーは実行時エラー", err: &graphQLError{messages: []string{"x"}}, want: exitRuntime},
		{name: "包まれた使い方の誤りも 2", err: fmt.Errorf("wrapped: %w", &usageError{"bad"}), want: exitUsage},
	}
	for _, c := range cases {
		if got := exitCodeFor(c.err); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// -format の選択が実際に出力形式を変えること。
func TestRenderSelectsFormat(t *testing.T) {
	rows := []resultRow{{keys: []string{"count"}, values: map[string]any{"count": "42"}}}
	cases := []struct {
		format   string
		noHeader bool
		wantSub  string
		wantNot  string
	}{
		{format: "tsv", wantSub: "count\n42\n"},
		{format: "tsv", noHeader: true, wantSub: "42\n", wantNot: "count"},
		{format: "table", wantSub: "-----"},
		{format: "json", wantSub: `"count": "42"`},
		{format: "", wantSub: "count\n42\n"}, // 既定は TSV
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := render(&buf, config{format: c.format, noHeader: c.noHeader}, rows); err != nil {
			t.Fatalf("format=%q: %v", c.format, err)
		}
		if !strings.Contains(buf.String(), c.wantSub) {
			t.Errorf("format=%q noHeader=%v: %q に %q が無い", c.format, c.noHeader, buf.String(), c.wantSub)
		}
		if c.wantNot != "" && strings.Contains(buf.String(), c.wantNot) {
			t.Errorf("format=%q noHeader=%v: %q が出ている", c.format, c.noHeader, c.wantNot)
		}
	}
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }
