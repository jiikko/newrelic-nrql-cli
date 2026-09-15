package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/mattn/go-runewidth"
)

// resultRow は NRQL の結果 1 行。
//
// キー順を保持するのが目的の型。NRQL は SELECT の並び（facet, count など）に
// 意味があるが、map[string]any へ入れるとその順序が失われ、出力カラムの順が
// 実行ごとに変わる。そのため JSON を Token 単位で読んで keys を別に持つ。
type resultRow struct {
	keys   []string // 出現順
	values map[string]any
}

// decodeResultRow は 1 行ぶんの JSON オブジェクトをキー順つきで読む。
func decodeResultRow(raw []byte) (resultRow, error) {
	row := resultRow{values: map[string]any{}}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 数値を指数表記に変えない

	tok, err := dec.Token()
	if err != nil {
		return row, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return row, fmt.Errorf("オブジェクトではありません: %s", truncate(string(raw), 100))
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return row, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return row, fmt.Errorf("キーが文字列ではありません: %v", keyTok)
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return row, err
		}
		if _, dup := row.values[key]; !dup {
			row.keys = append(row.keys, key)
		}
		row.values[key] = v
	}
	return row, nil
}

// columnsOf は全行のキーの和集合を「最初に現れた順」で返す。
// FACET 付きクエリなどで行ごとにキーが欠けることがあるため、和集合を取る。
func columnsOf(rows []resultRow) []string {
	var cols []string
	seen := map[string]bool{}
	for _, r := range rows {
		for _, k := range r.keys {
			if !seen[k] {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	return cols
}

// cell は 1 セルの表示文字列。キーが無い行は空文字にする。
func (r resultRow) cell(col string) string {
	v, ok := r.values[col]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		// ネストした配列・オブジェクトは JSON のまま出す。
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// renderTSV はタブ区切りで書き出す（awk / cut に流す用途）。
func renderTSV(w io.Writer, rows []resultRow, header bool) error {
	cols := columnsOf(rows)
	if len(cols) == 0 {
		return nil
	}
	if header {
		if _, err := fmt.Fprintln(w, strings.Join(cols, "\t")); err != nil {
			return err
		}
	}
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			// TSV を壊さないよう、値中のタブ・改行は空白へ潰す。
			cells[i] = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(r.cell(c))
		}
		if _, err := fmt.Fprintln(w, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return nil
}

// tableGap は表のカラム間の空き。
const tableGap = 2

// renderTable は桁を揃えた表で書き出す（人が読む用途）。
//
// 🚨 text/tabwriter は使わない。tabwriter はセル幅を**ルーン数**で数えるため、
// 全角文字（日本語のファセット値・エラーメッセージ）が入ると表示上ずれる
// （"東京店舗" は 4 ルーンだが端末では 8 カラム占める）。NRQL の結果には
// 日本語が普通に入るので、表示幅で詰める。
func renderTable(w io.Writer, rows []resultRow) error {
	cols := columnsOf(rows)
	if len(cols) == 0 {
		return nil
	}

	// 各カラムの表示幅を、ヘッダと全セルの最大値で決める。
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = displayWidth(c)
	}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = r.cell(c)
			if n := displayWidth(row[i]); n > widths[i] {
				widths[i] = n
			}
		}
		cells = append(cells, row)
	}

	header := make([]string, len(cols))
	seps := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c
		seps[i] = strings.Repeat("-", displayWidth(c))
	}
	if err := writeTableRow(w, header, widths); err != nil {
		return err
	}
	if err := writeTableRow(w, seps, widths); err != nil {
		return err
	}
	for _, row := range cells {
		if err := writeTableRow(w, row, widths); err != nil {
			return err
		}
	}
	return nil
}

// writeTableRow は 1 行を表示幅で詰めて書く（最終カラムは詰めない）。
func writeTableRow(w io.Writer, row []string, widths []int) error {
	var b strings.Builder
	for i, cell := range row {
		b.WriteString(cell)
		if i == len(row)-1 {
			break // 行末に余分な空白を残さない
		}
		b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)+tableGap))
	}
	_, err := fmt.Fprintln(w, b.String())
	return err
}

// displayWidth は端末上で占めるカラム数を返す（全角 = 2）。
func displayWidth(s string) int {
	return runewidth.StringWidth(s)
}

// renderJSON は結果をそのまま JSON 配列で書き出す。
func renderJSON(w io.Writer, rows []resultRow) error {
	cols := columnsOf(rows)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := make(map[string]any, len(r.keys))
		for _, c := range cols {
			if v, ok := r.values[c]; ok {
				m[c] = v
			}
		}
		out = append(out, m)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}
