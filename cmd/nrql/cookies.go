// Google Chrome の Cookie を macOS Keychain 経由で復号して取り出す。
//
// 復号・一時コピーの後始末（3 段構え）・エラーの分類・プロファイルの列挙は
// github.com/jiikko/dotfiles/src/chromecookie が持つ（slack-cli / esa-cli と共有）。
// 直すときはあちらを直し、ここへコピーを戻さないこと。ここに残すのは newrelic-nrql-cli 固有の部分
// （どの Cookie を送るか・自動検出の分類 profileWithoutSessionError / profileDataError への写し替え・案内の文言）だけ。
//
// 🚨 このツールは Google Chrome 専用。対応表の値（Keychain のサービス名・Application Support 配下の
// ディレクトリ名）は実機で確認しないと正しいか分からないため。esa-cli はかつて Brave / Chromium / Edge /
// Vivaldi も対応表に持っていたが、同じ理由で落とした（あちらの issue 003）。**対応表を復活させる提案は
// 両者で却下済み**（chromecookie の doc も同じ方針）。他のブラウザに広げるなら、実機で
// `security find-generic-password` と Application Support 配下のディレクトリ名を確認してから足すこと（issue 005）。
package main

import (
	"fmt"
	"strings"

	"github.com/jiikko/dotfiles/src/chromecookie"
)

// ws は nrql の作業領域（~/Library/Caches/newrelic-nrql-cli/extract）。Cookie DB の一時コピーはここを通る。
var ws = chromecookie.NewWorkspace("newrelic-nrql-cli")

// chromeName はエラーメッセージ用の表示名。
const chromeName = chromecookie.ChromeName

// cookieEntry は復号済みの 1 Cookie。
type cookieEntry = chromecookie.Cookie

// permissionHint は Cookie DB が権限で読めないときの案内。
// フルディスクアクセスで直るのは端末アプリ側の制限だけで、chmod 000 や root 所有の
// プロファイル（sudo で起動した Chrome が作ったもの）はパーミッション／所有者を直す必要がある。
const permissionHint = "  次の両方を確認してください:\n" +
	"    - ターミナル（またはこのツールを起動しているアプリ）の「フルディスクアクセス」\n" +
	"      （システム設定 → プライバシーとセキュリティ → フルディスクアクセス）\n" +
	"    - そのプロファイルのディレクトリと Cookies ファイルのパーミッション／所有者（ls -le で確認）\n"

// installCleanupOnSignal は②（シグナルで終わっても一時コピーを消す）を仕掛ける。main の先頭で 1 回。
func installCleanupOnSignal() { ws.InstallCleanupOnSignal() }

// sweepStaleCookieDirs は③（前回の実行が SIGKILL 等で残した一時コピーを消す）。
func sweepStaleCookieDirs() { ws.SweepStaleTempDirs() }

// runAllCleanups は登録済みの一時コピーをすべて消す。
func runAllCleanups() { ws.RunAllCleanups() }

// extractCookies は指定プロファイルから全 Cookie を復号して返す。
func extractCookies(profile string) (chromecookie.Result, error) {
	sweepStaleCookieDirs() // ③: 前回の実行が強制終了で残したものを先に片付ける

	password, err := chromecookie.KeychainPassword()
	if err != nil {
		return chromecookie.Result{}, err // EnvError = 環境エラー（プロファイルに依存しない）
	}
	return readProfileCookies(profile, password)
}

// readProfileCookies は Keychain から得たパスワードで、プロファイルの Cookie DB を読んで復号する。
// Keychain を読まないので、テストから本物の sqlite で実行できる。
//
// エラーは自動検出の分類へ写し替えて返す:
//   - Cookie DB が無い: profileWithoutSessionError（黙って飛ばす）
//   - 読めない・壊れた DB（chromecookie.ReadError）: profileDataError（記録して飛ばす）
//   - それ以外（作業領域の異常 = chromecookie.EnvError など）: そのまま（即座に返す）
//
// 目的の Cookie が無かったときの原因（全件の復号失敗・-wal を読めない）は、
// cookieClientForProfile が戻り値の Result.Diagnose で確かめる。
func readProfileCookies(profile string, password []byte) (chromecookie.Result, error) {
	res, err := ws.ReadCookies(profile, password)
	if err != nil {
		return chromecookie.Result{}, classifyReadError(err)
	}
	return res, nil
}

// classifyReadError は chromecookie の読み取りエラーを自動検出の分類へ写す。
func classifyReadError(err error) error {
	if chromecookie.IsMissing(err) {
		return &profileWithoutSessionError{msg: err.Error() + "\n  - プロファイル名が正しいか確認してください（-profile / NRQL_CHROME_PROFILE）。"}
	}
	if re, ok := chromecookie.AsProfileIssue("", err); ok {
		if re.Err.Kind == chromecookie.ReadDenied {
			// 原因はフルディスクアクセス不足か、ファイルのパーミッション／所有者のどちらか。
			return &profileDataError{err: fmt.Errorf("%w\n%s", re.Err, strings.TrimRight(permissionHint, "\n"))}
		}
		return &profileDataError{err: re.Err}
	}
	return err
}

// buildCookieHeader は対象ホスト宛ての Cookie ヘッダ文字列を組み立てる。
// 同名 Cookie が複数ある場合は、より具体的な host（長い host_key）を優先する。
func buildCookieHeader(cookies []cookieEntry, reqHost string) (string, int) {
	type pick struct {
		value string
		rank  int // 大きいほど具体的
	}
	best := map[string]pick{}
	for _, c := range cookies {
		if !chromecookie.HostMatches(c.Host, reqHost) {
			continue
		}
		rank := len(strings.TrimPrefix(c.Host, "."))
		if !strings.HasPrefix(c.Host, ".") {
			rank += 1000 // host-only を最優先
		}
		if cur, ok := best[c.Name]; !ok || rank > cur.rank {
			best[c.Name] = pick{value: c.Value, rank: rank}
		}
	}
	if len(best) == 0 {
		return "", 0
	}
	parts := make([]string, 0, len(best))
	for name, p := range best {
		parts = append(parts, name+"="+p.value)
	}
	return strings.Join(parts, "; "), len(best)
}
