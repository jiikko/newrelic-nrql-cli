package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// アカウント ID の既定値の決まり方を固定する（環境変数 > config.yml > 未設定）。
//
// 壊れた環境変数を黙って 0 に落とすと「アカウント ID が未設定です」と誤案内してしまい、
// 「値が壊れている」ことに利用者が気づけない。警告を出して config.yml 側へ退くのが正。
func TestResolveAccountDefault(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		file     int
		want     int
		wantWarn bool
	}{
		{name: "環境変数が config より優先", env: "222", file: 111, want: 222},
		{name: "環境変数が無ければ config", env: "", file: 111, want: 111},
		{name: "どちらも無ければ未設定", env: "", file: 0, want: 0},
		{name: "壊れた環境変数は警告して config へ退く", env: "abc", file: 111, want: 111, wantWarn: true},
		{name: "壊れた環境変数で config も無ければ未設定", env: "abc", file: 0, want: 0, wantWarn: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, warn := resolveAccountDefault(c.env, c.file)
			if got != c.want {
				t.Errorf("アカウント ID: got %d, want %d", got, c.want)
			}
			if (warn != "") != c.wantWarn {
				t.Errorf("警告の有無が違う: warn=%q, wantWarn=%v", warn, c.wantWarn)
			}
		})
	}
}

// config.yml の account が「数値」「文字列」の両方で読めることを固定する。
//
// v0.1.0 は文字列として書き出していた（account: "1234567"）。int だけを受ける実装に
// すると、その設定ファイルは解析に失敗し、account だけでなく region / profile まで
// 丸ごと無視される（実測で踏んだ）。
func TestFileConfigAcceptsAccountAsIntAndString(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want int
	}{
		{name: "数値", yaml: "account: 1234567\nregion: us\n", want: 1234567},
		{name: "文字列（v0.1.0 が書いた形）", yaml: "account: \"1234567\"\nregion: us\n", want: 1234567},
		{name: "空文字列", yaml: "account: \"\"\nregion: us\n", want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var fc fileConfig
			if err := yaml.Unmarshal([]byte(c.yaml), &fc); err != nil {
				t.Fatalf("解析に失敗: %v", err)
			}
			if int(fc.Account) != c.want {
				t.Errorf("account: got %d, want %d", int(fc.Account), c.want)
			}
			// account の解析で失敗すると、同じファイルの他の項目まで失われる。
			if fc.Region != "us" {
				t.Errorf("region が失われている: got %q", fc.Region)
			}
		})
	}
}
