package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// NRQL を GraphQL ドキュメントへ埋め込むときのエスケープを固定する。
// NRQL には文字列リテラルが普通に入る（WHERE name = 'x' / LIKE '%"y"%'）ので、
// 素朴な文字列連結だとドキュメントが壊れる、または注入になる。
func TestBuildNRQLDocumentEscapesQuotes(t *testing.T) {
	query := `SELECT count(*) FROM Log WHERE message LIKE '%"boom"%' AND path = 'C:\tmp'`
	doc := buildNRQLDocument(1234567, query)

	if !strings.Contains(doc, "account(id: 1234567)") {
		t.Fatalf("アカウント ID が埋まっていない: %s", doc)
	}

	// nrql(query: ...) の引数部分を取り出して、JSON 文字列として読めることを確かめる。
	const marker = "nrql(query: "
	i := strings.Index(doc, marker)
	if i < 0 {
		t.Fatalf("nrql(query: ...) が見つからない: %s", doc)
	}
	rest := doc[i+len(marker):]
	j := strings.Index(rest, ") { results }")
	if j < 0 {
		t.Fatalf("クエリ引数の終端が見つからない: %s", doc)
	}
	literal := rest[:j]

	var decoded string
	if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
		t.Fatalf("クエリリテラルが文字列として閉じていない（エスケープ漏れ）: %q: %v", literal, err)
	}
	if decoded != query {
		t.Errorf("エスケープを戻すと元の NRQL に一致すべき:\ngot  %q\nwant %q", decoded, query)
	}
}

// リージョンごとの接続先を固定する。
// EU のアカウントは US のホストでは認証が通らないため、取り違えると
// 「ログインしているのに動かない」形で壊れる。
func TestRegionEndpoints(t *testing.T) {
	eu, err := regionEndpoints("eu")
	if err != nil {
		t.Fatalf("eu を引けない: %v", err)
	}
	if !strings.Contains(eu.session, "one.eu.newrelic.com") || !strings.Contains(eu.apiKey, "api.eu.newrelic.com") {
		t.Errorf("eu のホストが EU 用でない: session=%s apikey=%s", eu.session, eu.apiKey)
	}

	us, err := regionEndpoints("US") // 大文字・前後の空白も受ける
	if err != nil {
		t.Fatalf("US を引けない: %v", err)
	}
	if us.session == eu.session {
		t.Errorf("us と eu が同じホストになっている: %s", us.session)
	}

	host, err := us.sessionHost()
	if err != nil {
		t.Fatalf("sessionHost: %v", err)
	}
	if host != "one.newrelic.com" {
		t.Errorf("セッションを読む対象ホストが違う: got %q", host)
	}

	if _, err := regionEndpoints("jp"); err == nil {
		t.Error("未対応のリージョンはエラーにすべき")
	}
}

// 認証方式ごとのエンドポイントが入れ替わらないことを固定する。
// ここは実測で確定した組み合わせで、崩すと 401/403 になる。
func TestAuthModeDeterminesEndpoint(t *testing.T) {
	us := regions["us"]
	if c := newCookieClient(us, "SESSION=x", "Default"); c.endpoint != us.session {
		t.Errorf("ブラウザセッションは UI 側のホストを使うべき: got %s", c.endpoint)
	}
	if c := newAPIKeyClient(us, "NRAK-xxx"); c.endpoint != us.apiKey {
		t.Errorf("API キーは公開 NerdGraph を使うべき: got %s", c.endpoint)
	}
	if !strings.Contains(us.session, "one.newrelic.com") || !strings.Contains(us.apiKey, "api.newrelic.com") {
		t.Errorf("エンドポイントが入れ替わっている: session=%s apikey=%s", us.session, us.apiKey)
	}
}
