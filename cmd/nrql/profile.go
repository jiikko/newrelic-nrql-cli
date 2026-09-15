package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	for _, p := range profiles {
		c, err := cookieClientForProfile(ep, host, p, cfg.timeout)
		if err != nil {
			continue // New Relic のセッションが無い / DB が無いプロファイルは飛ばす
		}
		candidates = append(candidates, c)
	}

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf(
			"%s のセッションを持つ %s のプロファイルが見つかりませんでした。\n"+
				"  %s で https://%s にログインしているか確認してください。\n"+
				"  リージョンが EU のアカウントなら -region eu が要ります（既定は us）。\n"+
				"  CI など無人環境では NEW_RELIC_API_KEY に User API key を設定してください。",
			host, chromeName, chromeName, host)
	case 1:
		return candidates[0], nil
	}

	// 複数ある場合だけ、実際に認証が通るものを選ぶ（1 件なら往復を省く）。
	return pickAuthenticatedClient(candidates, host)
}

// pickAuthenticatedClient は候補のうち認証が通る最初のものを返す。
//
// 候補が複数あるのは「仕事用と個人用の Chrome プロファイルが両方 New Relic の
// セッションを持っている」状況。どれを使ったかは stderr に出す（黙って選ぶと、
// 意図しないアカウントのデータを見ていることに気づけない）。
func pickAuthenticatedClient(candidates []*client, host string) (*client, error) {
	for _, c := range candidates {
		if err := c.ping(); err == nil {
			fmt.Fprintf(os.Stderr,
				"nrql: ログイン済みプロファイル %q を自動検出しました（nrql config set profile %q で固定できます）\n",
				c.profile, c.profile)
			return c, nil
		}
	}
	return nil, fmt.Errorf(
		"New Relic のセッションを持つプロファイルは %d 件ありましたが、いずれも認証が通りませんでした。\n"+
			"  %s で https://%s を開き直してログイン状態にしてから再実行してください。",
		len(candidates), chromeName, host)
}

// cookieClientForProfile は指定プロファイルのセッションでクライアントを作る。
// New Relic 宛てのものが 1 つも無ければエラー（自動検出時は次の候補へ進む合図）。
func cookieClientForProfile(ep endpoints, host, profile string, timeoutSeconds int) (*client, error) {
	entries, err := extractCookiesFn(profile)
	if err != nil {
		return nil, err
	}
	header, n := buildCookieHeader(entries, host)
	if n == 0 {
		return nil, fmt.Errorf("%s のセッションがプロファイル %q にありません", host, profile)
	}
	return newCookieClient(ep, header, profile, timeoutSeconds), nil
}

// listChromeProfiles は Chrome の Local State からプロファイルのディレクトリ名を列挙する。
// 読み取れない場合は実在するディレクトリを走査する。
func listChromeProfiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return []string{"Default"}
	}
	lsPath := filepath.Join(home, "Library", "Application Support", chromeSupportSubdir, "Local State")
	data, err := os.ReadFile(lsPath)
	if err != nil {
		return fallbackProfiles(home)
	}
	// info_cache のキー（プロファイルのディレクトリ名）だけを取り出す。他のフィールドは読まない。
	var ls struct {
		Profile struct {
			InfoCache map[string]json.RawMessage `json:"info_cache"`
			LastUsed  string                     `json:"last_used"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(data, &ls); err != nil || len(ls.Profile.InfoCache) == 0 {
		return fallbackProfiles(home)
	}
	profiles := make([]string, 0, len(ls.Profile.InfoCache))
	for dir := range ls.Profile.InfoCache {
		profiles = append(profiles, dir)
	}
	sort.Strings(profiles)
	// 直近に使われたプロファイルを先頭へ寄せる（検出を速くする）。
	if lu := ls.Profile.LastUsed; lu != "" {
		for i, p := range profiles {
			if p == lu {
				profiles = append([]string{p}, append(profiles[:i:i], profiles[i+1:]...)...)
				break
			}
		}
	}
	return profiles
}

// fallbackProfiles は Local State が読めないときに、実在するディレクトリを走査する。
func fallbackProfiles(home string) []string {
	base := filepath.Join(home, "Library", "Application Support", chromeSupportSubdir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return []string{"Default"}
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "Default" || strings.HasPrefix(name, "Profile ") {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return []string{"Default"}
	}
	sort.Strings(out)
	return out
}
