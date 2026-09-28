package main

import (
	"bytes"
	"strings"
	"testing"
)

// decodeResultRow / columnsOf が「応答の並び順」を保つことを固定する。
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

// 表の桁揃えを「ルーン数」ではなく「表示幅」で行うことを固定する。
//
// NRQL の FACET 値には日本語が普通に入る（店舗名・エラーメッセージ）。
// ルーン数で詰めると全角 1 文字 = 1 カラムと数えてしまい、端末では列がずれる。
func TestRenderTableAlignsByDisplayWidth(t *testing.T) {
	rows := []resultRow{
		{keys: []string{"name", "count"}, values: map[string]any{"name": "東京店舗", "count": "1"}},
		{keys: []string{"name", "count"}, values: map[string]any{"name": "abcdefgh", "count": "22"}},
	}
	var buf bytes.Buffer
	if err := renderTable(&buf, rows); err != nil {
		t.Fatalf("renderTable: %v", err)
	}

	// 🚨 期待値を displayWidth で組み立てないこと。production と同じ式で測ると、
	// 幅の数え方を壊す変異でテストの測り方も一緒に変わり、何も検出できなくなる
	// （実際にそれで変異が素通りした）。ここは端末での見え方をそのまま literal で固定する。
	//
	// "東京店舗" は端末で 8 カラム、"abcdefgh" も 8 カラム。よって両方の直後に
	// カラム間の空き 2 つだけが入り、2 列目は同じ位置から始まる。
	want := "" +
		"name      count\n" +
		"----      -----\n" +
		"東京店舗  1\n" +
		"abcdefgh  22\n"
	if buf.String() != want {
		t.Errorf("表の桁揃えが表示幅になっていない:\ngot:\n%s\nwant:\n%s", buf.String(), want)
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

// -format json も応答の並び順（TSV / table と同じ）でキーを出すことを固定する。
//
// 🚨 map[string]any を encoding/json に渡すとキーがアルファベット順に並び替えられ、
// resultRow が保っている列順が JSON だけ失われる（TSV / table は保つのに）。
// fixture のキーはアルファベット順と逆にしてある（並び替える実装なら必ず落ちる）。
// 2 行目は count を欠く（欠けたキーは出さない＝ null にしない）。
// 値の < > & は HTML エスケープしない（既存出力の SetEscapeHTML(false) と同じ）。
func TestRenderJSONPreservesColumnOrder(t *testing.T) {
	raws := [][]byte{
		[]byte(`{"zone":"a<b>&c","count":653517,"average.duration":0.25,"nested":{"k":[1,2]},"none":null}`),
		[]byte(`{"zone":"w","average.duration":1.5}`),
	}
	var rows []resultRow
	for _, raw := range raws {
		r, err := decodeResultRow(raw)
		if err != nil {
			t.Fatalf("decodeResultRow: %v", err)
		}
		rows = append(rows, r)
	}
	var buf bytes.Buffer
	if err := renderJSON(&buf, rows); err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	want := `[
  {
    "zone": "a<b>&c",
    "count": 653517,
    "average.duration": 0.25,
    "nested": {
      "k": [
        1,
        2
      ]
    },
    "none": null
  },
  {
    "zone": "w",
    "average.duration": 1.5
  }
]
`
	if buf.String() != want {
		t.Errorf("JSON 出力が違う:\ngot:\n%s\nwant:\n%s", buf.String(), want)
	}

	// 0 件は空配列（null にしない）。
	var empty bytes.Buffer
	if err := renderJSON(&empty, nil); err != nil {
		t.Fatalf("renderJSON(nil): %v", err)
	}
	if empty.String() != "[]\n" {
		t.Errorf("0 件の JSON が違う: %q", empty.String())
	}
}
