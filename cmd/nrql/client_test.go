package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 送信するヘッダを固定する。
//
// 🚨 このツールで最も load-bearing な事実は「newrelic-requesting-services ヘッダが
// 無いとログイン済みでも 403」であること（実測）。にもかかわらず、このヘッダを
// 削除する変異が全テスト green で通る状態だった。認証方式ごとに「付けるもの」と
// 「付けてはいけないもの」の両方を固定する。
func TestGraphQLSendsRequiredHeaders(t *testing.T) {
	cases := []struct {
		name       string
		newClient  func(srv *httptest.Server) *client
		wantHeader map[string]string // 値まで見るもの（空文字は「存在すること」だけ見る）
		wantAbsent []string
	}{
		{
			name: "ブラウザセッション",
			newClient: func(srv *httptest.Server) *client {
				c := newCookieClient(regions["us"], "session=SECRET", "Default")
				c.http = srv.Client()
				c.endpoint = srv.URL
				return c
			},
			wantHeader: map[string]string{
				"Cookie":                 "session=SECRET",
				requestingServicesHeader: requestingServicesValue,
				"Content-Type":           "application/json",
				"User-Agent":             userAgent,
			},
			wantAbsent: []string{"Api-Key"},
		},
		{
			name: "API キー",
			newClient: func(srv *httptest.Server) *client {
				c := newAPIKeyClient(regions["us"], "NRAK-SECRET")
				c.http = srv.Client()
				c.endpoint = srv.URL
				return c
			},
			wantHeader: map[string]string{
				"Api-Key":      "NRAK-SECRET",
				"Content-Type": "application/json",
			},
			// API キー経路はブラウザのセッションを送らない（送ると 401 になるだけでなく、
			// 無人環境へセッションを持ち込むことになる）。
			wantAbsent: []string{"Cookie", requestingServicesHeader},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got http.Header
			var method, body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				method = r.Method
				buf := make([]byte, 1024)
				n, _ := r.Body.Read(buf)
				body = string(buf[:n])
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"actor":{"user":{"name":"x"}}}}`))
			}))
			defer srv.Close()

			if err := c.newClient(srv).ping(); err != nil {
				t.Fatalf("ping: %v", err)
			}
			if method != http.MethodPost {
				t.Errorf("メソッドが違う: %s", method)
			}
			if !strings.Contains(body, `"query"`) {
				t.Errorf("GraphQL の query が本文に無い: %q", body)
			}
			for k, want := range c.wantHeader {
				if v := got.Get(k); v != want {
					t.Errorf("ヘッダ %s = %q, want %q", k, v, want)
				}
			}
			for _, k := range c.wantAbsent {
				if v := got.Get(k); v != "" {
					t.Errorf("ヘッダ %s が付いている（付けてはいけない）: %q", k, v)
				}
			}
		})
	}
}

// HTTP ステータスごとの扱いを固定する。
// ここが畳まれると、セッション切れの案内もレート制限の案内も出なくなる。
func TestGraphQLStatusHandling(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		mode      authMode
		wantErrIs func(error) bool
		wantSub   string
	}{
		{
			name: "200 は成功", status: 200, mode: authCookie,
			body: `{"data":{"actor":{"user":{"name":"x"}}}}`,
		},
		{
			name: "401 はセッション切れ", status: 401, mode: authCookie,
			wantErrIs: isSessionExpired, wantSub: "セッションが無効",
		},
		{
			name: "403 はセッション切れ + CSRF の可能性", status: 403, mode: authCookie,
			wantErrIs: isSessionExpired, wantSub: "CSRF",
		},
		{
			name: "API キー経路の 401 はキーの問題として案内", status: 401, mode: authAPIKey,
			wantSub: "API キーが拒否されました",
		},
		{
			name: "429 はレート制限", status: 429, mode: authCookie,
			wantSub: "レート制限",
		},
		{
			name: "500 は予期しないステータス", status: 500, mode: authCookie,
			body: "boom", wantSub: "予期しないステータス 500",
		},
		{
			name: "200 でも JSON でなければエラー", status: 200, mode: authCookie,
			body: "<html>login</html>", wantSub: "JSON として解釈できません",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			cl := &client{http: srv.Client(), endpoint: srv.URL, mode: c.mode, profile: "Default"}
			err := cl.ping()

			if c.wantSub == "" {
				if err != nil {
					t.Fatalf("エラーになってはいけない: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("エラーになるべき（status=%d）", c.status)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("案内文が違う: %v", err)
			}
			if c.wantErrIs != nil && !c.wantErrIs(err) {
				t.Errorf("エラーの型が違う: %T", err)
			}
		})
	}
}

// NerdGraph が errors 配列を返したら、それを握り潰さないこと。
func TestGraphQLSurfacesServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"NRQL Syntax Error: boom"},{"message":"second"}]}`))
	}))
	defer srv.Close()

	cl := &client{http: srv.Client(), endpoint: srv.URL, mode: authCookie}
	err := cl.ping()
	if err == nil {
		t.Fatal("GraphQL のエラーを握り潰している")
	}
	var ge *graphQLError
	if !errors.As(err, &ge) {
		t.Fatalf("graphQLError であるべき: %T", err)
	}
	for _, want := range []string{"NRQL Syntax Error: boom", "second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("メッセージが落ちている（%q）: %v", want, err)
		}
	}
}

// 巨大なレスポンスは「大きすぎる」と伝えること。
// 切り詰めるだけだと JSON の解析エラーに化け、ログインページが返ったという
// 見当違いの診断へ誘導する。
func TestGraphQLRejectsOversizedResponse(t *testing.T) {
	orig := maxResponseBytes
	maxResponseBytes = 64
	defer func() { maxResponseBytes = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"x":"` + strings.Repeat("a", 200) + `"}}`))
	}))
	defer srv.Close()

	cl := &client{http: srv.Client(), endpoint: srv.URL, mode: authCookie}
	err := cl.ping()
	if err == nil {
		t.Fatal("エラーになるべき")
	}
	if !strings.Contains(err.Error(), "大きすぎます") {
		t.Errorf("案内文が違う: %v", err)
	}
	if strings.Contains(err.Error(), "JSON として解釈できません") {
		t.Errorf("JSON 解析エラーに化けている: %v", err)
	}
}

// 上限ちょうどは通ること（境界を 1 バイトずらす変異を検出する）。
func TestGraphQLAcceptsResponseAtLimit(t *testing.T) {
	payload := `{"data":{"actor":{"user":{"name":"x"}}}}`
	orig := maxResponseBytes
	maxResponseBytes = int64(len(payload))
	defer func() { maxResponseBytes = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, payload)
	}))
	defer srv.Close()

	cl := &client{http: srv.Client(), endpoint: srv.URL, mode: authCookie}
	if err := cl.ping(); err != nil {
		t.Errorf("上限ちょうどは通るべき: %v", err)
	}
}

func isSessionExpired(err error) bool {
	var e *errSessionExpired
	return errors.As(err, &e)
}
