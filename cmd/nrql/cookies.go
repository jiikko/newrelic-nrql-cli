// Google Chrome の Cookie を macOS Keychain 経由で復号して取り出す。
//
// 出典: github.com/jiikko/esa-cli の cmd/esa/cookies.go からの移植（同じ仕組みを
// New Relic 向けに使う。あちらは複数ブラウザ対応だが、こちらは Chrome 専用）。仕様（PBKDF2-SHA1 1003 回 / AES-128-CBC / IV=0x20*16 /
// Chrome 130+ = meta.version>=24 で復号後の先頭 32 バイトがホストハッシュ）は
// 両者で共通なので、修正が要る場合は両方に当てること。
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// 🚨 このツールは Google Chrome 専用。
//
// 移植元の esa-cli は Brave / Chromium / Edge / Vivaldi も対応表に持っているが、
// ここでは意図的に Chrome だけにしている。理由は、対応表の値（Keychain の
// サービス名・Application Support 配下のディレクトリ名）は**実機で確認しないと
// 正しいか分からない**ため。手元で確認できるのは Chrome だけで、未確認の値を
// 並べると「動くように見えて別ブラウザの領域を読みに行く」形の事故になる。
//
// 他のブラウザに広げるなら、実機で `security find-generic-password` と
// Application Support 配下のディレクトリ名を確認してから足すこと（issue 005）。
const (
	chromeKeychainAccount = "Chrome"              // security -a
	chromeKeychainService = "Chrome Safe Storage" // security -s
	chromeSupportSubdir   = "Google/Chrome"       // ~/Library/Application Support 配下
	chromeName            = "Google Chrome"       // エラーメッセージ用の表示名
)

// cookieEntry は復号済みの 1 Cookie。
type cookieEntry struct {
	host  string // host_key（先頭ドットを含む場合がある）
	name  string
	value string
}

// getKeychainPassword は Keychain から "Chrome Safe Storage" のパスワードを取得する。
func getKeychainPassword() ([]byte, error) {
	cmd := exec.Command("security", "find-generic-password",
		"-w",
		"-a", chromeKeychainAccount,
		"-s", chromeKeychainService)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf(
			"Keychain から暗号化キーを取得できませんでした（service=%q account=%q）。\n"+
				"  - ターミナルで次を実行し、表示される許可ダイアログで「常に許可」を押してください:\n"+
				"      security find-generic-password -w -a %q -s %q\n"+
				"  - %s がインストールされているか確認してください（このツールは Chrome 専用です）。\n"+
				"  元エラー: %w",
			chromeKeychainService, chromeKeychainAccount,
			chromeKeychainAccount, chromeKeychainService, chromeName, err)
	}
	return []byte(strings.TrimRight(string(out), "\n")), nil
}

// deriveKey は PBKDF2-SHA1（macOS: 1003 回, 16 バイト）で AES-128 鍵を導出する。
func deriveKey(password []byte) ([]byte, error) {
	return pbkdf2.Key(sha1.New, string(password), []byte("saltysalt"), 1003, 16)
}

// decryptValue は encrypted_value を復号する。
// v10 プレフィックスなら AES-128-CBC（IV=0x20*16, PKCS7）で復号し、
// metaVersion>=24 なら復号後の先頭 32 バイト（ハッシュプレフィックス）を落とす。
func decryptValue(enc, key []byte, metaVersion int) (string, error) {
	if len(enc) == 0 {
		return "", nil
	}
	if len(enc) < 3 || string(enc[:3]) != "v10" {
		// 古い Chrome の平文データ
		return string(enc), nil
	}
	ciphertext := enc[3:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", errors.New("暗号文の長さが不正です")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	iv := []byte("                ") // 0x20 * 16
	mode := cipher.NewCBCDecrypter(block, iv)
	plain := make([]byte, len(ciphertext))
	mode.CryptBlocks(plain, ciphertext)

	// PKCS7 unpad
	plain, err = pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		return "", err
	}
	if metaVersion >= 24 {
		if len(plain) < 32 {
			return "", errors.New("復号結果がハッシュプレフィックスより短いです")
		}
		plain = plain[32:]
	}
	return string(plain), nil
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("PKCS7: データ長が不正です")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > blockSize || pad > len(data) {
		return nil, errors.New("PKCS7: パディングが不正です")
	}
	return data[:len(data)-pad], nil
}

// cookieDBSourcePath は Cookie DB の絶対パスを返す（cwd 非依存: HOME 起点）。
func cookieDBSourcePath(profile string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// 新しい Chrome は Cookies を Network/ サブディレクトリに置く。両方を候補にする。
	base := filepath.Join(home, "Library", "Application Support", chromeSupportSubdir, profile)
	candidates := []string{
		filepath.Join(base, "Network", "Cookies"),
		filepath.Join(base, "Cookies"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf(
		"Cookie DB が見つかりませんでした（プロファイル=%q）。探した場所:\n  %s\n"+
			"  - プロファイル名が正しいか確認してください（-profile / NRQL_CHROME_PROFILE）。\n"+
			"  - ~/Library/Application Support/%s/ 配下のディレクトリ名がプロファイル名です（既定は Default）。",
		profile, strings.Join(candidates, "\n  "), chromeSupportSubdir)
}

// copyCookieDB は Cookie DB を一時ディレクトリへコピーする。
// WAL に未反映のセッション Cookie を取りこぼさないよう、-wal / -shm も同名でコピーする。
// 返り値: 一時 DB パスと後始末関数。
func copyCookieDB(src string) (string, func(), error) {
	tmpdir, err := os.MkdirTemp("", "nrql-cookie-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmpdir) }

	for _, suffix := range []string{"", "-wal", "-shm"} {
		s := src + suffix
		data, err := os.ReadFile(s)
		if err != nil {
			if suffix == "" {
				cleanup()
				if os.IsPermission(err) {
					return "", nil, fmt.Errorf(
						"Cookie DB を読み取れませんでした（アクセス拒否）。\n"+
							"  ターミナル（またはこのツールを起動しているアプリ）に「フルディスクアクセス」を付与してください:\n"+
							"    システム設定 → プライバシーとセキュリティ → フルディスクアクセス\n"+
							"  対象ファイル: %s", src)
				}
				return "", nil, fmt.Errorf("Cookie DB の読み取りに失敗: %w", err)
			}
			continue // -wal / -shm は存在しないこともある
		}
		dst := filepath.Join(tmpdir, "Cookies"+suffix)
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return filepath.Join(tmpdir, "Cookies"), cleanup, nil
}

// extractCookies は指定プロファイルから全 Cookie を復号して返す。
func extractCookies(profile string) ([]cookieEntry, error) {
	password, err := getKeychainPassword()
	if err != nil {
		return nil, err
	}
	key, err := deriveKey(password)
	if err != nil {
		return nil, err
	}

	src, err := cookieDBSourcePath(profile)
	if err != nil {
		return nil, err
	}
	dbPath, cleanup, err := copyCookieDB(src)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var metaVersion int
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'version'`).Scan(&metaVersion); err != nil {
		// meta が読めない場合は 0 扱い（トリム無し）で続行
		metaVersion = 0
	}

	rows, err := db.Query(`SELECT host_key, name, value, encrypted_value FROM cookies`)
	if err != nil {
		return nil, fmt.Errorf("cookies テーブルの読み取りに失敗: %w", err)
	}
	defer rows.Close()

	var out []cookieEntry
	for rows.Next() {
		var host, name, plainValue string
		var enc []byte
		if err := rows.Scan(&host, &name, &plainValue, &enc); err != nil {
			return nil, err
		}
		value := plainValue
		if value == "" && len(enc) > 0 {
			v, derr := decryptValue(enc, key, metaVersion)
			if derr != nil {
				continue // 1 件の復号失敗で全体を止めない
			}
			value = v
		}
		out = append(out, cookieEntry{host: host, name: name, value: value})
	}
	return out, rows.Err()
}

// cookieHostMatches は Cookie の host_key が対象ホストに送信されるべきか判定する。
// 標準の Cookie ドメインマッチ（ドット境界）を用い、suffix 文字列比較の誤爆を避ける。
func cookieHostMatches(hostKey, reqHost string) bool {
	if hostKey == "" {
		return false
	}
	if strings.HasPrefix(hostKey, ".") {
		d := hostKey[1:] // domain cookie
		return reqHost == d || strings.HasSuffix(reqHost, "."+d)
	}
	return hostKey == reqHost // host-only cookie は完全一致のみ
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
		if !cookieHostMatches(c.host, reqHost) {
			continue
		}
		rank := len(strings.TrimPrefix(c.host, "."))
		if !strings.HasPrefix(c.host, ".") {
			rank += 1000 // host-only を最優先
		}
		if cur, ok := best[c.name]; !ok || rank > cur.rank {
			best[c.name] = pick{value: c.value, rank: rank}
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
