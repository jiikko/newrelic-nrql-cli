package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// 後始末の機構が production の経路から実際に呼ばれていることを固定する。
//
// 🚨 これが無いと「機構は在るが誰も呼んでいない」を検出できない。実測で踏んだ:
// installCleanupOnSignal / sweepStaleCookieDirs の呼び出しを消す変異を当てても、
// 関数を直接呼ぶテストは全部 green のままだった（機構の単体テストは
// 「呼ばれていること」を 1 mm も守らない）。
//
// grep ではなく AST で見る。grep だとコメントや文字列リテラル（この検査自身の
// ソースを含む）に一致して、本物の呼び出しが消えても緑のままになりうる。
func TestCleanupIsWiredIntoProductionPaths(t *testing.T) {
	cases := []struct {
		caller string // この関数の本体から
		callee string // これが呼ばれていること
		why    string
	}{
		{"main", "installCleanupOnSignal", "シグナル経路の後始末（層②）が仕掛けられない"},
		{"extractCookies", "sweepStaleCookieDirs", "前回の残骸の掃除（層③）が走らない"},
		{"cmdQuery", "checkNoTrailingFlags", "クエリの後ろに置かれたフラグが NRQL 本文に吸収される"},
		{"cmdConfig", "fileConfigProblem", "壊れた config.yml を黙って上書きしてしまう"},
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		// テストファイルは対象外（production の配線だけを見る）
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("ソースの解析に失敗: %v", err)
	}

	for _, c := range cases {
		body := findFuncBody(pkgs, c.caller)
		if body == nil {
			t.Errorf("関数 %s が見つからない（改名された？ この検査を更新すること）", c.caller)
			continue
		}
		if !callsFunc(body, c.callee) {
			t.Errorf("%s から %s が呼ばれていない: %s", c.caller, c.callee, c.why)
		}
	}
}

func findFuncBody(pkgs map[string]*ast.Package, name string) *ast.BlockStmt {
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Recv == nil && fn.Name.Name == name {
					return fn.Body
				}
			}
		}
	}
	return nil
}

func callsFunc(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}
