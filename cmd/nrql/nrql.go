package main

import (
	"encoding/json"
	"fmt"
)

// buildNRQLDocument は NRQL を GraphQL ドキュメントへ組み立てる。
//
// 🚨 クエリは GraphQL 変数ではなくインラインで埋め込む。nrql(query:) の型は
// カスタムスカラー Nrql で、実測で通ることを確認したのがこの形。変数渡しへ
// 「改善」しないこと（未検証の形になる）。
// 文字列のエスケープ（" と \ と制御文字）は json.Marshal に任せる。GraphQL の
// 文字列リテラルは JSON 文字列と同じ規則なので、これで正しく閉じる。
func buildNRQLDocument(accountID int, query string) string {
	quoted, err := json.Marshal(query)
	if err != nil {
		// string の Marshal は失敗しない。到達したら呼び出し側のバグ。
		panic(fmt.Sprintf("NRQL のエスケープに失敗: %v", err))
	}
	return fmt.Sprintf(
		"{ actor { account(id: %d) { nrql(query: %s) { results } } } }",
		accountID, quoted)
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
		Account *struct {
			NRQL *struct {
				Results []json.RawMessage `json:"results"`
			} `json:"nrql"`
		} `json:"account"`
	} `json:"actor"`
}

// runNRQL は NRQL を実行して結果行を返す。
func (c *client) runNRQL(accountID int, query string) ([]resultRow, error) {
	var resp nrqlResponse
	if err := c.graphQL(buildNRQLDocument(accountID, query), &resp); err != nil {
		return nil, err
	}
	if resp.Actor == nil || resp.Actor.Account == nil {
		return nil, fmt.Errorf(
			"アカウント %d を参照できませんでした。次のいずれかです:\n"+
				"  - アカウント ID の誤り（nrql accounts で一覧を確認してください）\n"+
				"  - そのアカウントへの権限が無い\n"+
				"  - リージョンが違う（EU のアカウントなら -region eu）",
			accountID)
	}
	if resp.Actor.Account.NRQL == nil {
		return nil, fmt.Errorf("NRQL の実行結果が返りませんでした（アカウント %d）", accountID)
	}
	rows := make([]resultRow, 0, len(resp.Actor.Account.NRQL.Results))
	for i, raw := range resp.Actor.Account.NRQL.Results {
		r, err := decodeResultRow(raw)
		if err != nil {
			return nil, fmt.Errorf("結果 %d 行目の解釈に失敗: %w", i+1, err)
		}
		rows = append(rows, r)
	}
	return rows, nil
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
