package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// -a の解析（単一 / 複数 / 不正）。
func TestParseAccountSpec(t *testing.T) {
	cases := []struct {
		spec    string
		want    []int
		wantErr bool
	}{
		{spec: "1234567", want: []int{1234567}},
		{spec: "1234567,2345678", want: []int{1234567, 2345678}},
		{spec: " 1234567 , 2345678 ", want: []int{1234567, 2345678}}, // 空白を許す
		{spec: "1234567,1234567", want: []int{1234567}},              // 重複は畳む
		{spec: "1234567,,2345678", want: []int{1234567, 2345678}},    // 空要素は飛ばす
		{spec: "", want: nil},
		{spec: "abc", wantErr: true},
		{spec: "0", wantErr: true},
		{spec: "-5", wantErr: true},
		{spec: "1234567,abc", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseAccountSpec(c.spec)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err=%v, wantErr=%v", c.spec, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%q: got %v, want %v", c.spec, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("%q: 並びが違う got %v, want %v（指定順を保つこと）", c.spec, got, c.want)
				break
			}
		}
	}
}

// 単一と複数で GraphQL の形が変わること。
//
// 🚨 1 件のときは account(id:) の形を使い続ける。実測で通ることを確認したのはこちらで、
// accounts: [A] に一本化してよいかは未検証。検証済みの形を壊さないことを固定する。
func TestBuildNRQLDocumentSingleVsMulti(t *testing.T) {
	single := buildNRQLDocument([]int{1234567}, "SELECT count(*) FROM T")
	if !strings.Contains(single, "account(id: 1234567)") {
		t.Errorf("単一は account(id:) の形であるべき: %s", single)
	}
	if strings.Contains(single, "accounts:") {
		t.Errorf("単一で accounts: を使っている: %s", single)
	}

	multi := buildNRQLDocument([]int{1234567, 2345678}, "SELECT count(*) FROM T")
	if !strings.Contains(multi, "accounts: [1234567, 2345678]") {
		t.Errorf("複数は accounts: [..] の形であるべき: %s", multi)
	}
	if strings.Contains(multi, "account(id:") {
		t.Errorf("複数で account(id:) を使っている: %s", multi)
	}
	// どちらの形でもクエリ本文は同じエスケープを通ること。
	for _, doc := range []string{single, multi} {
		if !strings.Contains(doc, `query: "SELECT count(*) FROM T"`) {
			t.Errorf("NRQL の埋め込みが壊れている: %s", doc)
		}
	}
}

// レスポンスの形が単一 / 複数で違うので、両方から結果を取り出せること。
// どちらの形でも「引けなかった」と「0 件」を区別すること。
func TestRunNRQLHandlesBothResponseShapes(t *testing.T) {
	cases := []struct {
		name     string
		ids      []int
		body     string
		wantRows int
		wantErr  string
	}{
		{
			name: "単一: 結果あり", ids: []int{1},
			body:     `{"data":{"actor":{"account":{"nrql":{"results":[{"count":3}]}}}}}`,
			wantRows: 1,
		},
		{
			name: "複数: 結果あり", ids: []int{1, 2},
			body:     `{"data":{"actor":{"nrql":{"results":[{"count":5}]}}}}`,
			wantRows: 1,
		},
		{
			name: "複数: 本当に 0 件", ids: []int{1, 2},
			body:     `{"data":{"actor":{"nrql":{"results":[]}}}}`,
			wantRows: 0,
		},
		{
			name: "複数: nrql が null は参照できない", ids: []int{1, 2},
			body:    `{"data":{"actor":{"nrql":null}}}`,
			wantErr: "参照できませんでした",
		},
		{
			name: "複数: actor が null", ids: []int{1, 2},
			body:    `{"data":{"actor":null}}`,
			wantErr: "参照できませんでした",
		},
		{
			// 複数指定なのに単一の形で返ってきた場合も「引けなかった」に落とす
			// （黙って 0 件にしない）。
			name: "複数: 単一の形で返ってきた", ids: []int{1, 2},
			body:    `{"data":{"actor":{"account":{"nrql":{"results":[{"count":1}]}}}}}`,
			wantErr: "参照できませんでした",
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
			rows, err := cl.runNRQL(c.ids, "SELECT count(*) FROM T")
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("エラーになるべき（rows=%d）", len(rows))
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("案内文が違う: %v", err)
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
