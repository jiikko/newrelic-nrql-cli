package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 🚨 検証で確定した接続条件（2026-09 時点の実測。US リージョンで確認）。
// ここを変えると動かなくなる。
//
//   - ブラウザセッションが通るのは UI 側のプロキシ one.newrelic.com/graphql だけ。
//     公開 NerdGraph の api.newrelic.com/graphql にセッションを投げると 401 になる。
//   - 逆に API キー (Api-Key ヘッダ) は api.newrelic.com/graphql 側で使う。
//     → ホストは「認証方式の一部」であり、付け替えてはいけない。
//   - セッション認証時は requestingServicesHeader が必須。無いと 403。
//     値は任意でよく（存在だけをチェックしている）、Origin / Referer は見られていない。
//     New Relic 側の CSRF 対策が変わったとき、このツールで最初に壊れるのはここ。
const (
	requestingServicesHeader = "newrelic-requesting-services"
	requestingServicesValue  = "nrql-cli"

	userAgent = "newrelic-nrql-cli (browser session; read-only NRQL)"
)

// endpoints は 1 リージョンぶんの接続先。
// 認証方式ごとにホストが違うため（上のコメント参照）、対で持つ。
type endpoints struct {
	session string // ブラウザセッション用（UI のプロキシ）
	apiKey  string // User API key 用（公開 NerdGraph）
}

// regions は New Relic のデータセンターごとの接続先。
// アカウントがどちらに属するかは契約時に決まり、ホストが違う。
//
// us は実測済み。**eu は未検証**（EU のアカウントを持っていないため）。
// EU で動かない報告があれば、まずここのホスト名を疑う。
var regions = map[string]endpoints{
	"us": {
		session: "https://one.newrelic.com/graphql",
		apiKey:  "https://api.newrelic.com/graphql",
	},
	"eu": {
		session: "https://one.eu.newrelic.com/graphql",
		apiKey:  "https://api.eu.newrelic.com/graphql",
	},
}

// regionEndpoints はリージョン名から接続先を引く。
func regionEndpoints(region string) (endpoints, error) {
	ep, ok := regions[strings.ToLower(strings.TrimSpace(region))]
	if !ok {
		names := make([]string, 0, len(regions))
		for k := range regions {
			names = append(names, k)
		}
		sort.Strings(names)
		return endpoints{}, fmt.Errorf("未対応のリージョン %q（対応: %s）", region, strings.Join(names, ", "))
	}
	return ep, nil
}

// sessionHost は「どのホスト宛てのセッションを読むか」を返す（Cookie の絞り込みに使う）。
func (e endpoints) sessionHost() (string, error) {
	u, err := url.Parse(e.session)
	if err != nil {
		return "", err
	}
	return u.Host, nil
}

// authMode は認証方式。エンドポイントとヘッダの組み合わせを決める。
type authMode int

const (
	authCookie authMode = iota // ブラウザのログインセッション Cookie
	authAPIKey                 // User API key（CI 等の無人環境向け）
)

// client は NerdGraph に GraphQL を投げる。
type client struct {
	http     *http.Client
	mode     authMode
	endpoint string
	cookie   string // authCookie のとき: Cookie ヘッダ値
	apiKey   string // authAPIKey のとき: User API key
	profile  string // 診断メッセージ用（どの Chrome プロファイル由来か）
}

func newCookieClient(ep endpoints, cookieHeader, profile string) *client {
	return &client{
		http:     &http.Client{Timeout: 60 * time.Second},
		mode:     authCookie,
		endpoint: ep.session,
		cookie:   cookieHeader,
		profile:  profile,
	}
}

func newAPIKeyClient(ep endpoints, key string) *client {
	return &client{
		http:     &http.Client{Timeout: 60 * time.Second},
		mode:     authAPIKey,
		endpoint: ep.apiKey,
		apiKey:   key,
	}
}

// errSessionExpired はブラウザのログインセッションが切れたことを示す。
// New Relic はアイドルでセッションが切れる（login_idle_session_timeout Cookie）ので、
// これは異常ではなく日常的に起きる。
type errSessionExpired struct {
	status  int
	profile string
	host    string // one.newrelic.com / one.eu.newrelic.com
}

func (e *errSessionExpired) Error() string {
	msg := fmt.Sprintf(
		"セッションが無効です（HTTP %d / Chrome のプロファイル %q）。\n"+
			"  New Relic はアイドル時間でセッションが切れます。%s で https://%s を\n"+
			"  開き直してログイン状態にしてから、もう一度実行してください。\n"+
			"  無人環境（CI 等）では NEW_RELIC_API_KEY に User API key を設定してください。",
		e.status, e.profile, chromeName, e.host)
	if e.status == http.StatusForbidden {
		// 403 は 2 つの原因を持つ。実験で確認済み（2026-09-15）:
		// requestingServicesHeader を送らずに実行すると、ログイン済みでも 403 になった。
		// つまり「ログインし直しても 403 のまま」なら New Relic 側の CSRF 対策変更を疑う。
		msg += "\n  ログインし直しても 403 が続く場合は、New Relic 側の CSRF 対策が変わった可能性があります\n" +
			"    （このツールは " + requestingServicesHeader + " ヘッダの存在チェックに依存しています）。"
	}
	return msg
}

// graphQLError は NerdGraph が返す errors 配列。
type graphQLError struct {
	messages []string
}

func (e *graphQLError) Error() string {
	return "NerdGraph エラー: " + strings.Join(e.messages, " / ")
}

// graphQL は GraphQL ドキュメントを POST し、data を out にデコードする。
// 数値は json.Number で受けるため、out 側の map[string]any には指数表記でなく
// 元の表記のまま入る（NRQL の count が 1.23456e+06 になるのを避ける）。
func (c *client) graphQL(document string, out any) error {
	payload, err := json.Marshal(map[string]string{"query": document})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	switch c.mode {
	case authCookie:
		req.Header.Set("Cookie", c.cookie)
		req.Header.Set(requestingServicesHeader, requestingServicesValue)
	case authAPIKey:
		req.Header.Set("Api-Key", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("リクエスト失敗（%s）: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	if err != nil {
		return err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// 本文検査へ
	case http.StatusUnauthorized, http.StatusForbidden:
		if c.mode == authAPIKey {
			return fmt.Errorf("API キーが拒否されました（HTTP %d）。NEW_RELIC_API_KEY が User API key か確認してください", resp.StatusCode)
		}
		// 403 は「ヘッダ欠落」でも起きるが、このクライアントは必ず付けているので
		// 実際に起きるのはセッション切れ（または New Relic 側の CSRF 対策変更）。
		return &errSessionExpired{
			status:  resp.StatusCode,
			profile: c.profile,
			host:    hostOf(c.endpoint),
		}
	case http.StatusTooManyRequests:
		return fmt.Errorf("レート制限（429）。しばらく待って再実行してください")
	default:
		return fmt.Errorf("予期しないステータス %d: %s\n%s", resp.StatusCode, c.endpoint, truncate(string(body), 500))
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&envelope); err != nil {
		// HTML が返るのは典型的にログインページへのリダイレクト。
		return fmt.Errorf("レスポンスを JSON として解釈できません（%s）: %w\n%s",
			c.endpoint, err, truncate(string(body), 300))
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return &graphQLError{messages: msgs}
	}
	if out == nil {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(envelope.Data))
	d.UseNumber()
	return d.Decode(out)
}

// ping は認証が通るかだけを確かめる軽いクエリ（プロファイル自動検出で使う）。
func (c *client) ping() error {
	var v struct {
		Actor struct {
			User struct {
				Name string `json:"name"`
			} `json:"user"`
		} `json:"actor"`
	}
	return c.graphQL("{ actor { user { name } } }", &v)
}

// hostOf は URL からホスト部だけを取り出す（診断メッセージ用。失敗しても案内を止めない）。
func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return u.Host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
