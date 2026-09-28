package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/jiikko/dotfiles/src/chromecookie"
)

// profileAuto は「New Relic にログイン済みのプロファイルを自動検出する」予約値。
const profileAuto = "auto"

// extractCookiesFn は Cookie の取り出し口。テストから差し替えるための seam。
//
// 本物は macOS の Keychain と Chrome の保存領域を読むため、テストから実行できない。
// ここを 1 箇所にしておくと、プロファイル選択の優先順位（API キー > 明示指定 > 自動検出）を
// 実機なしで固定できる。
var extractCookiesFn = extractCookies

// resolveClient は設定から実行用クライアントを 1 つ決める。
//
// 優先順位:
//  1. NEW_RELIC_API_KEY があれば API キー方式（無人環境向け。ブラウザを一切読まない）
//  2. -profile が明示されていればそのプロファイル
//  3. auto: New Relic のセッションを持つ Chrome プロファイルを列挙し、認証が通る最初のものを使う
func resolveClient(cfg config) (*client, error) {
	ep, err := regionEndpoints(cfg.region)
	if err != nil {
		return nil, err
	}
	if key := os.Getenv("NEW_RELIC_API_KEY"); key != "" {
		return newAPIKeyClient(ep, key, cfg.timeout), nil
	}
	host, err := ep.sessionHost()
	if err != nil {
		return nil, err
	}
	if cfg.profile != "" && cfg.profile != profileAuto {
		return cookieClientForProfile(ep, host, cfg.profile, cfg.timeout)
	}

	profiles := listChromeProfiles()
	var candidates []*client
	var skipped skipNotes // 飛ばしたが「無い」ではなかったプロファイルと理由（最終エラーに添える）
	for _, p := range profiles {
		c, err := cookieClientForProfile(ep, host, p, cfg.timeout)
		if err == nil {
			candidates = append(candidates, c)
			continue
		}
		// 🚨 分類を崩さないこと:
		//   - セッションが無い（DB 無し / 宛先 Cookie 0 件）→ 黙って飛ばす
		//   - このプロファイル固有の問題（壊れた DB・全件復号失敗・権限 EACCES/EPERM 等）→
		//     記録して飛ばす。使っていないプロファイルの壊れた DB や chmod 000 / root 所有の
		//     プロファイル 1 つで自動検出全体を止めない（権限はフルディスクアクセスでは直らない
		//     ことがあるので、環境エラー扱いにしない）
		//   - それ以外（Keychain の拒否・一時ディレクトリを作れない 等の自プロセス側の異常）→
		//     即座に返す。プロファイルに依存しないので、飛ばすとプロファイル数だけ security を
		//     起動した末に「ログインしているか確認」と誤案内する
		var absent *profileWithoutSessionError
		var broken *profileDataError
		switch {
		case errors.As(err, &absent):
		case errors.As(err, &broken):
			skipped.add(p, broken)
		default:
			return nil, fmt.Errorf("プロファイル %q: %w", p, err)
		}
	}

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf(
			"%s のセッションを持つ %s のプロファイルが見つかりませんでした。\n"+
				"  %s で https://%s にログインしているか確認してください。\n"+
				"  リージョンが EU のアカウントなら -region eu が要ります（既定は us）。\n"+
				"  CI など無人環境では NEW_RELIC_API_KEY に User API key を設定してください。%s",
			host, chromeName, chromeName, host, skipped.note())
	case 1:
		return candidates[0], nil
	}

	// 複数ある場合だけ、実際に認証が通るものを選ぶ（1 件なら往復を省く）。
	return pickAuthenticatedClient(candidates, host, skipped)
}

// skipNotes は自動検出で飛ばしたプロファイルの理由（最終エラーに添える）。
type skipNotes struct {
	lines      []string
	permission bool // 権限（EACCES/EPERM）で読めなかったプロファイルがあった
}

func (n *skipNotes) add(profile string, err error) {
	n.lines = append(n.lines, fmt.Sprintf("%q: %s", profile, firstLine(err.Error())))
	if errors.Is(err, fs.ErrPermission) {
		n.permission = true
	}
}

// note は最終エラーに添える文を返す（何も飛ばしていなければ空）。
func (n skipNotes) note() string {
	if len(n.lines) == 0 {
		return ""
	}
	out := "\n  飛ばしたプロファイル（-profile <name> で固定すると、そのプロファイルのエラーをそのまま確認できます）:\n    " +
		strings.Join(n.lines, "\n    ")
	if n.permission {
		out += "\n  権限で読めなかったプロファイルがあります。\n" + strings.TrimRight(permissionHint, "\n")
	}
	return out
}

// pickAuthenticatedClient は候補のうち認証が通る最初のものを返す。
//
// 候補が複数あるのは「仕事用と個人用の Chrome プロファイルが両方 New Relic の
// セッションを持っている」状況。どれを使ったかは stderr に出す（黙って選ぶと、
// 意図しないアカウントのデータを見ていることに気づけない）。
//
// 🚨 即座に返すのはプロファイルに依存しない失敗（stopsAutoDetection: ネットワーク断・
// タイムアウト・429・5xx）だけ。次へ進むと、同じ失敗を 候補数 × timeout ぶん繰り返した末に
// 「認証が通らない」と誤案内する。それ以外（セッション切れ・200 + GraphQL エラー・
// 200 + 非 JSON・user が null）は「このプロファイルは未認証」として理由を記録して次へ進む。
// skipped は resolveClient が飛ばしたプロファイルの理由（最終エラーに添える）。
func pickAuthenticatedClient(candidates []*client, host string, skipped skipNotes) (*client, error) {
	for _, c := range candidates {
		err := c.ping()
		if err == nil {
			fmt.Fprintf(os.Stderr,
				"nrql: ログイン済みプロファイル %q を自動検出しました（nrql config set profile %q で固定できます）\n",
				c.profile, c.profile)
			return c, nil
		}
		if stopsAutoDetection(err) {
			return nil, fmt.Errorf("プロファイル %q の認証を確かめる途中で失敗しました: %w", c.profile, err)
		}
		skipped.add(c.profile, err)
	}
	return nil, fmt.Errorf(
		"New Relic のセッションを持つプロファイルは %d 件ありましたが、いずれも認証が通りませんでした。\n"+
			"  %s で https://%s を開き直してログイン状態にしてから再実行してください。%s",
		len(candidates), chromeName, host, skipped.note())
}

// stopsAutoDetection は ping の失敗がプロファイルに依存しない（次の候補でも同じになる）かを返す。
//
// 🚨 200 + GraphQL エラー / 200 + 非 JSON を「未認証」側に数えるのは旧実装の挙動を保つため。
// セッション切れが 3xx / 401 / 403 で返ることは実測済みだが、200 でどの形が返るかは未実測
// （graphQL のデコード失敗箇所のコメントどおり、HTML はログインページの可能性が高い）。
func stopsAutoDetection(err error) bool {
	var reqErr *requestError
	if errors.As(err, &reqErr) {
		return true // ネットワーク断・タイムアウト
	}
	var st *statusError
	if errors.As(err, &st) {
		return st.status == http.StatusTooManyRequests || st.status >= 500
	}
	return false
}

// firstLine は案内文の 1 行目だけを返す（飛ばした理由を 1 行ずつ並べるため）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// profileWithoutSessionError は「このプロファイルには New Relic のセッションが無い」
// （Cookie DB が無い / 対象ホスト宛ての Cookie が無い）ことを表す。自動検出は黙って飛ばす。
type profileWithoutSessionError struct{ msg string }

func (e *profileWithoutSessionError) Error() string { return e.msg }

// profileDataError は Keychain を読めた後の、このプロファイル固有の問題（Cookie DB が
// sqlite でない・読み取りに失敗した・全件復号できない 等）。自動検出は理由を記録して飛ばす。
type profileDataError struct{ err error }

func (e *profileDataError) Error() string { return e.err.Error() }
func (e *profileDataError) Unwrap() error { return e.err }

// cookieClientForProfile は指定プロファイルのセッションでクライアントを作る。
// New Relic 宛てのものが 1 つも無ければ profileWithoutSessionError（自動検出時は次の候補へ進む合図）。
func cookieClientForProfile(ep endpoints, host, profile string, timeoutSeconds int) (*client, error) {
	res, err := extractCookiesFn(profile)
	if err != nil {
		return nil, err
	}
	header, n := buildCookieHeader(res.Cookies, host)
	if n == 0 {
		// 🚨 「セッションが無い」に化けさせない: 全件の復号失敗（鍵違い）・-wal を読めない、は記録して飛ばす。
		if derr := res.Diagnose(host + " 宛ての Cookie "); derr != nil {
			return nil, classifyReadError(derr)
		}
		return nil, &profileWithoutSessionError{msg: fmt.Sprintf("%s のセッションがプロファイル %q にありません", host, profile)}
	}
	return newCookieClient(ep, header, profile, timeoutSeconds), nil
}

// listChromeProfiles は Chrome の Local State からプロファイルのディレクトリ名を列挙する（直近に使ったものが先頭）。
// 読み取れない場合は実在するディレクトリを走査する（chromecookie.ListProfiles）。
func listChromeProfiles() []string {
	ps := chromecookie.ListProfiles()
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Dir)
	}
	return out
}
