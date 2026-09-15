package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ① リダイレクトを追わないこと。
//
// 追うと、セッションは https→http のダウングレードで、API キーは別ドメインへ、
// それぞれ持ち越される（stdlib の仕様。実測で確認した）。
// 「2 回目のリクエストが発生しないこと」で固定する（資格情報の比較ではなく、
// そもそも送られないことを見る）。
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var reqs []*http.Request
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reqs = append(reqs, req)
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://one.newrelic.com/login"}},
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})

	us := regions["us"]
	c := newCookieClient(us, "session=SECRET", "Default", 0)
	c.http.Transport = rt

	err := c.ping()
	if len(reqs) != 1 {
		t.Errorf("リダイレクトを追っている: リクエストが %d 回発生した（1 回であるべき）", len(reqs))
	}
	var expired *errSessionExpired
	if !errors.As(err, &expired) {
		t.Errorf("3xx はセッション切れとして案内すべき: got %T (%v)", err, err)
	}
	if !strings.Contains(err.Error(), "リダイレクト") {
		t.Errorf("案内文にリダイレクトされた事実が無い: %v", err)
	}
}

// ② アカウントが引けなかったことを「0 件」にしないこと。
//
// 値型の struct だと account: null がゼロ値になり、ID の誤り・権限不足・
// リージョン違いがすべて rc=0 の「0 件」になる（実測で踏んだ）。
func TestRunNRQLDistinguishesMissingAccountFromEmptyResult(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantErr   bool
		wantRows  int
		errSubstr string
	}{
		{
			name:      "account が null",
			body:      `{"data":{"actor":{"account":null}}}`,
			wantErr:   true,
			errSubstr: "参照できませんでした",
		},
		{
			name:      "actor が null",
			body:      `{"data":{"actor":null}}`,
			wantErr:   true,
			errSubstr: "参照できませんでした",
		},
		{
			name:      "nrql が null",
			body:      `{"data":{"actor":{"account":{"nrql":null}}}}`,
			wantErr:   true,
			errSubstr: "実行結果が返りませんでした",
		},
		{
			name:     "本物の 0 件",
			body:     `{"data":{"actor":{"account":{"nrql":{"results":[]}}}}}`,
			wantErr:  false,
			wantRows: 0,
		},
		{
			name:     "1 件",
			body:     `{"data":{"actor":{"account":{"nrql":{"results":[{"count":3}]}}}}}`,
			wantErr:  false,
			wantRows: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			cl := &client{http: srv.Client(), endpoint: srv.URL, mode: authCookie}
			rows, err := cl.runNRQL([]int{1234567}, "SELECT count(*) FROM T")
			if c.wantErr {
				if err == nil {
					t.Fatalf("エラーになるべきだが rows=%d err=nil（「0 件」に化けている）", len(rows))
				}
				if !strings.Contains(err.Error(), c.errSubstr) {
					t.Errorf("エラー文が違う: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("エラーになってはいけない: %v", err)
			}
			if len(rows) != c.wantRows {
				t.Errorf("行数: got %d, want %d", len(rows), c.wantRows)
			}
		})
	}
}

// ③ 端末へ出す出力の無害化（値・カラム名の両方、TSV・table の両方）。
func TestOutputSanitization(t *testing.T) {
	rows := []resultRow{
		{keys: []string{"host", "msg"}, values: map[string]any{"host": "a", "msg": "line1\nline2"}},
		{keys: []string{"host", "msg"}, values: map[string]any{"host": "bbbbbbbb", "msg": "\x1b]0;PWNED\x07x"}},
	}

	var table, tsv bytes.Buffer
	if err := renderTable(&table, rows); err != nil {
		t.Fatal(err)
	}
	if err := renderTSV(&tsv, rows, true); err != nil {
		t.Fatal(err)
	}

	// 表は「ヘッダ + 区切り + データ 2 行」の 4 行でなければならない。
	if n := strings.Count(strings.TrimRight(table.String(), "\n"), "\n") + 1; n != 4 {
		t.Errorf("table の行数が %d（改行が潰されていない）:\n%s", n, table.String())
	}
	for name, out := range map[string]string{"table": table.String(), "TSV": tsv.String()} {
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("%s に ESC が残っている: %q", name, out)
		}
		if strings.ContainsRune(out, 0x07) {
			t.Errorf("%s に BEL が残っている: %q", name, out)
		}
	}

	// カラム名側（TSV のヘッダ）にタブが入っても列がずれないこと。
	hdr := []resultRow{{keys: []string{"a\tb"}, values: map[string]any{"a\tb": "1"}}}
	var h bytes.Buffer
	if err := renderTSV(&h, hdr, true); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(h.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("行数が違う: %q", h.String())
	}
	if hc, dc := strings.Count(lines[0], "\t"), strings.Count(lines[1], "\t"); hc != dc {
		t.Errorf("ヘッダとデータのカラム数が食い違う: header=%d data=%d (%q)", hc+1, dc+1, h.String())
	}

	// JSON は生の値を保つ（encoding/json が \uXXXX へ逃がすので端末は操作されない）。
	var j bytes.Buffer
	if err := renderJSON(&j, rows); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(j.String(), 0x1b) {
		t.Errorf("JSON に生の ESC が出ている: %s", j.String())
	}
	if !strings.Contains(j.String(), "line1\\nline2") {
		t.Errorf("JSON は元の値を保つべき: %s", j.String())
	}
}

// ④ config.yml の account を 10 進として読むこと（YAML の暗黙変換を封じる）。
func TestAccountIDRejectsImplicitYAMLConversions(t *testing.T) {
	cases := []struct {
		yaml    string
		want    int
		wantErr bool
	}{
		{yaml: `account: 1234567`, want: 1234567},
		{yaml: `account: "1234567"`, want: 1234567}, // v0.1.0 が書いた形
		{yaml: `account: 0123456`, want: 123456},    // 8 進解釈させない（42798 にしない）
		{yaml: `account: " 123 "`, want: 123},
		{yaml: `account: ""`, want: 0},
		{yaml: `account: 0x1F`, wantErr: true},  // 16 進を黙って受けない
		{yaml: `account: 1.5e7`, wantErr: true}, // 浮動小数を黙って切り捨てない
		{yaml: `account: true`, wantErr: true},
		{yaml: `account: [1]`, wantErr: true},
	}
	for _, c := range cases {
		var fc fileConfig
		err := yaml.Unmarshal([]byte(c.yaml), &fc)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: エラーになるべき（got account=%d）", c.yaml, int(fc.Account))
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 解析に失敗: %v", c.yaml, err)
			continue
		}
		if int(fc.Account) != c.want {
			t.Errorf("%s: got %d, want %d", c.yaml, int(fc.Account), c.want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ⑤ クエリの後ろに置かれたフラグを検出すること。
func TestCheckNoTrailingFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "NRQL だけ", args: []string{"SELECT count(*) FROM Transaction"}},
		{name: "複数語の NRQL", args: []string{"SELECT", "count(*)", "FROM", "T"}},
		{name: "後ろにフラグ", args: []string{"SELECT 1", "-format", "json"}, wantErr: true},
		{name: "後ろに長いフラグ", args: []string{"SELECT 1", "--format=json"}, wantErr: true},
		{name: "負の数は NRQL の一部としてありうる", args: []string{"SELECT 1 WHERE x > -1"}},
		{name: "単独のハイフンは許す", args: []string{"SELECT 1", "-"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkNoTrailingFlags(c.args)
			if (err != nil) != c.wantErr {
				t.Errorf("args=%q: err=%v, wantErr=%v", c.args, err, c.wantErr)
			}
			if c.wantErr {
				var ue *usageError
				if !errors.As(err, &ue) {
					t.Errorf("使い方エラー（rc=2）にすべき: %T", err)
				}
			}
		})
	}
}

// ④ 壊れた config.yml でも、読めた項目は失わないこと。
//
// account の書式が不正なだけで region / profile まで失うと、利用者が気づかないまま
// 別リージョンへ繋ぎに行く（issues/004 の症状を設定ファイル側から作る）。
func TestParseFileConfigKeepsReadableFieldsOnError(t *testing.T) {
	data := []byte("account: true\nregion: eu\nprofile: Profile 7\n")
	fc, err := parseFileConfig(data)
	if err == nil {
		t.Fatal("account: true は解析エラーになるべき")
	}
	if fc.Region != "eu" {
		t.Errorf("region が失われた: %q", fc.Region)
	}
	if fc.Profile != "Profile 7" {
		t.Errorf("profile が失われた: %q", fc.Profile)
	}
	if int(fc.Account) != 0 {
		t.Errorf("読めなかった account は 0 であるべき: %d", int(fc.Account))
	}

	// 正常なファイルではエラーを返さないこと（この検査が常にエラーを返す実装だと無意味）。
	ok, err := parseFileConfig([]byte("account: 1234567\nregion: us\n"))
	if err != nil {
		t.Fatalf("正常な設定でエラーになった: %v", err)
	}
	if int(ok.Account) != 1234567 || ok.Region != "us" {
		t.Errorf("正常な設定を読めていない: %+v", ok)
	}
}
