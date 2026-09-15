package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// buildNRQLDocument は NRQL を GraphQL ドキュメントへ組み立てる。
//
// 🚨 クエリは GraphQL 変数ではなくインラインで埋め込む。nrql(query:) の型は
// カスタムスカラー Nrql で、実測で通ることを確認したのがこの形。変数渡しへ
// 「改善」しないこと（未検証の形になる）。
// 文字列のエスケープ（" と \ と制御文字）は json.Marshal に任せる。GraphQL の
// 文字列リテラルは JSON 文字列と同じ規則なので、これで正しく閉じる。
func buildNRQLDocument(accountIDs []int, query string) string {
	quoted, err := json.Marshal(query)
	if err != nil {
		// string の Marshal は失敗しない。到達したら呼び出し側のバグ。
		panic(fmt.Sprintf("NRQL のエスケープに失敗: %v", err))
	}
	if len(accountIDs) == 1 {
		// 🚨 1 件のときは従来の形を使い続ける。実測で通ることを確認したのはこちらで、
		// accounts: [A] に一本化してよいかは未検証。検証済みの形を壊さない。
		return fmt.Sprintf(
			"{ actor { account(id: %d) { nrql(query: %s) { results } } } }",
			accountIDs[0], quoted)
	}
	// 複数アカウントの同時クエリ。actor 直下の nrql(accounts:) を使う
	// （実測で合算値が返ることを確認済み。issues/003）。
	ids := make([]string, len(accountIDs))
	for i, id := range accountIDs {
		ids[i] = strconv.Itoa(id)
	}
	return fmt.Sprintf(
		"{ actor { nrql(accounts: [%s], query: %s) { results } } }",
		strings.Join(ids, ", "), quoted)
}

// nrqlResponse は buildNRQLDocument に対する data の形。
//
// 🚨 各階層をポインタにするのは「引けなかった」と「0 件だった」を区別するため。
// 値型にすると、NerdGraph が account: null を返しても json.Decode はエラーを出さず
// ゼロ値を作り、**アカウント ID の誤り・権限不足・リージョン違いがすべて「0 件」**に
// なって rc=0 で成功終了する（実測で踏んだ。issues/004 の症状の正体）。
//
// results の各要素は「キー順を保ちたい」ので RawMessage のまま受ける
// （map に入れるとカラム順が失われる）。
type nrqlResponse struct {
	Actor *struct {
		// 単一アカウント（account(id:) 経由）
		Account *struct {
			NRQL *nrqlResult `json:"nrql"`
		} `json:"account"`
		// 複数アカウント（nrql(accounts:) 経由）
		NRQL *nrqlResult `json:"nrql"`
	} `json:"actor"`
}

type nrqlResult struct {
	Results []json.RawMessage `json:"results"`
}

// runNRQL は NRQL を実行して結果行を返す。
func (c *client) runNRQL(accountIDs []int, query string) ([]resultRow, error) {
	if len(accountIDs) == 0 {
		return nil, fmt.Errorf("アカウント ID が指定されていません")
	}
	var resp nrqlResponse
	if err := c.graphQL(buildNRQLDocument(accountIDs, query), &resp); err != nil {
		return nil, err
	}
	result, err := resp.result(accountIDs)
	if err != nil {
		return nil, err
	}
	rows := make([]resultRow, 0, len(result.Results))
	for i, raw := range result.Results {
		r, err := decodeResultRow(raw)
		if err != nil {
			return nil, fmt.Errorf("結果 %d 行目の解釈に失敗: %w", i+1, err)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// result は単一 / 複数のどちらの形で返ってきたかを吸収して結果を取り出す。
//
// 🚨 「引けなかった」と「0 件だった」を必ず分ける。値型の struct にすると
// account: null がゼロ値になり、ID の誤り・権限不足・リージョン違いがすべて
// 「0 件」rc=0 に化ける（実測で踏んだ。issues/done/004）。
func (r nrqlResponse) result(accountIDs []int) (*nrqlResult, error) {
	ids := formatAccountIDs(accountIDs)
	if r.Actor == nil {
		return nil, accountUnreachableError(ids)
	}
	if len(accountIDs) == 1 {
		if r.Actor.Account == nil {
			return nil, accountUnreachableError(ids)
		}
		if r.Actor.Account.NRQL == nil {
			return nil, fmt.Errorf("NRQL の実行結果が返りませんでした（アカウント %s）", ids)
		}
		return r.Actor.Account.NRQL, nil
	}
	if r.Actor.NRQL == nil {
		return nil, accountUnreachableError(ids)
	}
	return r.Actor.NRQL, nil
}

func accountUnreachableError(ids string) error {
	return fmt.Errorf(
		"アカウント %s を参照できませんでした。次のいずれかです:\n"+
			"  - アカウント ID の誤り（nrql accounts で一覧を確認してください）\n"+
			"  - そのアカウントへの権限が無い（複数指定のときは 1 つでも権限が無いと失敗します）\n"+
			"  - リージョンが違う（EU のアカウントなら -region eu）",
		ids)
}

// formatAccountIDs は案内文用にアカウント ID を並べる。
func formatAccountIDs(ids []int) string {
	ss := make([]string, len(ids))
	for i, id := range ids {
		ss[i] = strconv.Itoa(id)
	}
	return strings.Join(ss, ", ")
}

// account は NerdGraph が返すアカウント。
type account struct {
	ID   json.Number `json:"id"`
	Name string      `json:"name"`
}

// accounts はアクセスできるアカウント一覧を返す。
func (c *client) accounts() ([]account, error) {
	var resp struct {
		Actor struct {
			Accounts []account `json:"accounts"`
		} `json:"actor"`
	}
	if err := c.graphQL("{ actor { accounts { id name } } }", &resp); err != nil {
		return nil, err
	}
	return resp.Actor.Accounts, nil
}
