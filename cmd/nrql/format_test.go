package main

import (
	"bytes"
	"strings"
	"testing"
)

// decodeResultRow / columnsOf が「NRQL の SELECT の並び順」を保つことを固定する。
//
// fixture は意図的に次の形にしてある:
//   - キーがアルファベット順でない（sort してしまう実装なら落ちる）
//   - 2 行目に 1 行目のキーが 1 つ無く、1 行目に無いキーが 1 つある
//     （行ごとの map を素朴に使う実装・和集合を取らない実装なら落ちる）
func TestColumnsPreserveNRQLOrderAcrossRows(t *testing.T) {
	raws := [][]byte{
		[]byte(`{"name":"web","count":653517,"average.duration":0.25}`),
		[]byte(`{"name":"worker","average.duration":1.5,"errorRate":0.01}`),
	}
	var rows []resultRow
	for i, raw := range raws {
		r, err := decodeResultRow(raw)
		if err != nil {
			t.Fatalf("%d 行目のデコードに失敗: %v", i+1, err)
		}
		rows = append(rows, r)
	}

	got := columnsOf(rows)
	want := []string{"name", "count", "average.duration", "errorRate"}
	if len(got) != len(want) {
		t.Fatalf("カラム数が違う: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("カラム順が違う: got %v, want %v", got, want)
		}
	}

	// 欠けたセルは空文字（"<nil>" や 0 にしない）。
	if c := rows[1].cell("count"); c != "" {
		t.Errorf("欠けたセルは空であるべき: got %q", c)
	}
	// 数値は指数表記にしない（count が 6.53517e+05 になるのを防ぐ）。
	if c := rows[0].cell("count"); c != "653517" {
		t.Errorf("数値の表記が壊れている: got %q, want %q", c, "653517")
	}
}

func TestRenderTSVUsesColumnOrderAndBlankForMissing(t *testing.T) {
	rows := []resultRow{
		{keys: []string{"name", "count"}, values: map[string]any{"name": "web", "count": "1"}},
		{keys: []string{"name"}, values: map[string]any{"name": "worker"}},
	}
	var buf bytes.Buffer
	if err := renderTSV(&buf, rows, true); err != nil {
		t.Fatalf("renderTSV: %v", err)
	}
	want := "name\tcount\nweb\t1\nworker\t\n"
	if buf.String() != want {
		t.Errorf("TSV 出力が違う:\ngot  %q\nwant %q", buf.String(), want)
	}
}

// 値にタブや改行が入っても TSV の列がずれないことを固定する。
func TestRenderTSVFlattensTabsAndNewlines(t *testing.T) {
	rows := []resultRow{
		{keys: []string{"message"}, values: map[string]any{"message": "a\tb\nc"}},
	}
	var buf bytes.Buffer
	if err := renderTSV(&buf, rows, false); err != nil {
		t.Fatalf("renderTSV: %v", err)
	}
	line := strings.TrimRight(buf.String(), "\n")
	if strings.Count(line, "\t") != 0 || strings.Contains(line, "\n") {
		t.Errorf("値のタブ・改行が潰されていない: %q", line)
	}
	if line != "a b c" {
		t.Errorf("got %q, want %q", line, "a b c")
	}
}
