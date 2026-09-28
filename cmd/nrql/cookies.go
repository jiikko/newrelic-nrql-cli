// Google Chrome の Cookie を macOS Keychain 経由で復号して取り出す。
//
// 出典: github.com/jiikko/esa-cli の cmd/esa/cookies.go からの移植（同じ仕組みを
// New Relic 向けに使う）。仕様（PBKDF2-SHA1 1003 回 / AES-128-CBC / IV=0x20*16 /
// Chrome 130+ = meta.version>=24 で復号後の先頭 32 バイトがホストハッシュ）は
// 両者で共通なので、修正が要る場合は両方に当てること。
//
// 移植時点では esa-cli が複数ブラウザ対応でこちらだけが Chrome 専用だったが、
// esa-cli も 3e93f3e で Chrome 専用になった（あちらの issue 003）。現在はどちらも
// macOS + Google Chrome 専用で、下の 🚨 の理由も両者で共通。
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	_ "modernc.org/sqlite"
)

// 🚨 このツールは Google Chrome 専用。
//
// 意図的に Chrome だけにしている。理由は、対応表の値（Keychain の
// サービス名・Application Support 配下のディレクトリ名）は**実機で確認しないと
// 正しいか分からない**ため。手元で確認できるのは Chrome だけで、未確認の値を
// 並べると「動くように見えて別ブラウザの領域を読みに行く」形の事故になる。
//
// 移植元の esa-cli はかつて Brave / Chromium / Edge / Vivaldi も対応表に持っていたが、
// 同じ理由で落とした（あちらの issue 003）。**対応表を復活させる提案は両者で却下済み**なので、
// 再提案する前にこの理由を読むこと。
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
// metaVersion>=24 なら復号後の先頭 32 バイト（ハッシュプレフィックス）を照合してから落とす。
// hostKey は cookies テーブルの host_key 列（先頭ドットを含むならそのまま）。
func decryptValue(enc, key []byte, metaVersion int, hostKey string) (string, error) {
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
		if len(plain) < sha256.Size {
			return "", errors.New("復号結果がハッシュプレフィックスより短いです")
		}
		// 🚨 先頭 32 バイトは SHA256(host_key) でなければならない。落とすだけにしない。
		// 鍵違いの復号でも PKCS7 の末尾は約 1/256 で偶然通り、v24 の値は長いので 32 バイトを
		// 落としても何かが残る。照合しないと、数千件の Cookie があれば鍵違いでも「復号できた」
		// ものが混ざり、全件復号失敗の検出（readProfileCookies）が働かない。
		//
		// 根拠: Chromium net/extras/sqlite/sqlite_persistent_cookie_store.cc は DB version 24 で
		// 暗号化前の値に SHA256(domain)（domain = host_key 列）を前置し、読み込み時に
		// 復号結果の先頭 crypto::kSHA256Length バイトがそのハッシュと一致しない Cookie を捨てる
		// （別ドメインへの Cookie の付け替え対策）。
		// ⚠ 実機の Chrome での照合結果は未確認（main agent が確認する）。ここを誤ると
		// 正常な Cookie が全部復号失敗になり「全件復号できない」エラーになる。
		want := sha256.Sum256([]byte(hostKey))
		if subtle.ConstantTimeCompare(plain[:sha256.Size], want[:]) != 1 {
			return "", errors.New("復号結果のドメインハッシュが host_key と一致しません（鍵違いの可能性）")
		}
		plain = plain[sha256.Size:]
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
	// 🚨 最終バイトだけでなく、パディング全体が同じ値であることを確かめる。
	// 最終バイトしか見ない実装は 1,2,3,4 のような不正なパディングを通し、
	// 復号結果の末尾にゴミが残った Cookie 値をそのまま送ることになる。
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, errors.New("PKCS7: パディングバイトが揃っていません")
		}
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
		_, err := os.Stat(c)
		if err == nil {
			return c, nil
		}
		// 🚨 ENOENT 以外を「DB が無い」に丸めない（黙って飛ばすと、確かめられなかった事実が消える）。
		// 権限（EACCES/EPERM）も含めてこのプロファイル固有の問題として記録する。chmod 000 や
		// root 所有のプロファイル（sudo で起動した Chrome が作ったもの）はフルディスクアクセスを
		// 付けても直らないので、自動検出全体を止めずに後ろの正常なプロファイルを使わせる。
		// 権限かどうかは errors.Is(err, fs.ErrPermission) で辿れる（%w で包むこと）。
		if !errors.Is(err, fs.ErrNotExist) {
			if errors.Is(err, fs.ErrPermission) {
				// -profile 明示時はこの文がそのまま利用者に届くので、直し方も添える。
				return "", &profileDataError{err: fmt.Errorf("Cookie DB を確認できませんでした（アクセス拒否: %s）。\n%s  元エラー: %w", c, permissionHint, err)}
			}
			return "", &profileDataError{err: fmt.Errorf("Cookie DB を確認できませんでした（%s）: %w", c, err)}
		}
	}
	// 自動検出では「このプロファイルは飛ばす」合図になる型で返す（profile.go の resolveClient）。
	return "", &profileWithoutSessionError{msg: fmt.Sprintf(
		"Cookie DB が見つかりませんでした（プロファイル=%q）。探した場所:\n  %s\n"+
			"  - プロファイル名が正しいか確認してください（-profile / NRQL_CHROME_PROFILE）。\n"+
			"  - ~/Library/Application Support/%s/ 配下のディレクトリ名がプロファイル名です（既定は Default）。",
		profile, strings.Join(candidates, "\n  "), chromeSupportSubdir)}
}

// permissionHint は Cookie DB が権限で読めないときの案内。
// フルディスクアクセスで直るのは端末アプリ側の制限だけで、chmod 000 や root 所有の
// プロファイル（sudo で起動した Chrome が作ったもの）はパーミッション／所有者を直す必要がある。
const permissionHint = "  次の両方を確認してください:\n" +
	"    - ターミナル（またはこのツールを起動しているアプリ）の「フルディスクアクセス」\n" +
	"      （システム設定 → プライバシーとセキュリティ → フルディスクアクセス）\n" +
	"    - そのプロファイルのディレクトリと Cookies ファイルのパーミッション／所有者（ls -le で確認）\n"

// --- 一時コピーの後始末 ---
//
// 🚨 「必ず消す」を defer だけに任せない。Go の既定ではシグナル（Ctrl-C）で
// プロセスが即終了して defer が走らず、Chrome の Cookie DB の完全なコピーが
// $TMPDIR に残る。macOS のフルディスクアクセスで守られた領域の中身を、
// 守られていない場所へ置き去りにすることになる。
//
// 3 段構え（段ごとにテストを持つ。cookies_cleanup_test.go）:
//  ① defer による即時削除 — 正常終了・エラー・panic を覆う
//  ② シグナル（SIGINT/SIGTERM/SIGHUP）を捕まえ、登録済みの削除を実行してから終了する
//  ③ ②でも間に合わない終わり方（SIGKILL・強制終了・電源断）に備え、起動時に
//     「自分が作った親ディレクトリ直下」「名前が <pid>-… の形」「その pid が生きていない」の
//     3 条件をすべて満たすものだけを消す
//
// ③ は破壊的操作なので、条件に合わないものは一切触らない（母集合を広げない）。

var (
	cleanupMu    sync.Mutex
	cleanupPaths = map[string]struct{}{}
)

// registerCleanup は「プロセスが終わる前に消すべきパス」を登録する（②が使う）。
func registerCleanup(path string) {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	cleanupPaths[path] = struct{}{}
}

// runAllCleanups は登録済みのパスをすべて削除する。
// ①の defer と②のシグナル経路の両方から呼ばれるが、os.RemoveAll は冪等なので二重呼び出しは無害。
func runAllCleanups() {
	cleanupMu.Lock()
	paths := make([]string, 0, len(cleanupPaths))
	for p := range cleanupPaths {
		paths = append(paths, p)
	}
	cleanupPaths = map[string]struct{}{}
	cleanupMu.Unlock()
	for _, p := range paths {
		_ = os.RemoveAll(p)
	}
}

// installCleanupOnSignal は②を仕掛ける。main の先頭で 1 回だけ呼ぶ。
func installCleanupOnSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig := <-ch
		runAllCleanups()
		if s, ok := sig.(syscall.Signal); ok {
			os.Exit(128 + int(s)) // シェルの慣習（SIGINT=130 / SIGTERM=143）
		}
		os.Exit(1)
	}()
}

// cookieTempRoot は一時コピーの親ディレクトリ。
// 自分だけが作る固定パスにすることで、③の掃除対象を「この配下」に閉じ込める。
func cookieTempRoot() string {
	return filepath.Join(os.TempDir(), "nrql-cookie")
}

// ensureCookieTempRoot は親ディレクトリを 0700 で用意する。
// 他人が用意した同名のディレクトリ・シンボリックリンクだった場合は使わずに失敗する
// （その配下を消しに行くのは③なので、所有者の確認をここで済ませる）。
func ensureCookieTempRoot() (string, error) {
	root := cookieTempRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("一時ディレクトリが通常のディレクトリではありません: %s", root)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("一時ディレクトリの所有者が自分ではありません: %s", root)
	}
	return root, nil
}

// sweepStaleCookieDirs は③。過去の実行が SIGKILL 等で残したものだけを消す。
// 条件に 1 つでも合わなければ触らない（判断できないものは残す方へ倒す）。
func sweepStaleCookieDirs() {
	root := cookieTempRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return // 親ディレクトリが無ければ何もしない
	}
	self := os.Getpid()
	for _, e := range entries {
		if !e.IsDir() {
			continue // ディレクトリ以外は対象外
		}
		pid, ok := pidFromTempDirName(e.Name())
		if !ok || pid == self {
			// 名前の形が違う / 自分のものは触らない。
			// 🚨 この !ok と processAlive の pid<=0 ガードは現状「互いを覆う」冗長な関係にある
			//（形が違う → pid 0 → 判定不能 → 消さない）。片方を外す変異は素通りするので、
			// 外すときは「もう片方が本当に同じものを守るか」を確かめること。意図的に両方残す。
			continue
		}
		if processAlive(pid) {
			continue // 生きているプロセスのものは触らない（並行実行）
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// pidFromTempDirName は "<pid>-<乱数>" 形式のディレクトリ名から pid を取り出す。
// この形式でないものは対象外（false を返す）。
func pidFromTempDirName(name string) (int, bool) {
	i := strings.IndexByte(name, '-')
	if i <= 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(name[:i])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processAlive は pid のプロセスが生きているかを返す。
// 判断できないとき（権限が無い等）は「生きている」に倒す = 消さない方へ倒す。
//
// 🚨 os.FindProcess + Process.Signal を使わないこと。消えたプロセスに対して
// ESRCH ではなく os.ErrProcessDone を返すため、ESRCH だけを見る判定は
// 「常に生きている」に落ちて掃除が 1 件も走らなくなる（実測で踏んだ）。
// kill(2) を直接呼び、errno をそのまま判定する。
func processAlive(pid int) bool {
	// 🚨 pid 0 / 負値を kill(2) に渡さない。0 は「自分のプロセスグループ全体」、
	// 負値は「プロセスグループ指定」を意味し、生死判定にならない（成功して
	// 「生きている」に見える）。ここでは判定不能として扱い、消さない方へ倒す。
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true // シグナルを送れた = 生きている
	}
	if errors.Is(err, syscall.ESRCH) {
		return false // そんなプロセスは無い = 死んでいる
	}
	return true // EPERM 等、判断できないときは消さない
}

// copyCookieDB は Cookie DB を一時ディレクトリへコピーする。
// WAL に未反映のセッション Cookie を取りこぼさないよう、-wal / -shm も同名でコピーする。
// 返り値: 一時 DB パスと後始末関数。
func copyCookieDB(src string) (string, func(), error) {
	root, err := ensureCookieTempRoot()
	if err != nil {
		return "", nil, err
	}
	// ディレクトリ名に pid を埋める（③がこれを見て「生きていない実行の残骸」を判定する）。
	tmpdir, err := os.MkdirTemp(root, fmt.Sprintf("%d-", os.Getpid()))
	if err != nil {
		return "", nil, err
	}
	registerCleanup(tmpdir)
	cleanup := func() { os.RemoveAll(tmpdir) }

	for _, suffix := range []string{"", "-wal", "-shm"} {
		s := src + suffix
		data, err := os.ReadFile(s)
		if err != nil {
			// -wal / -shm は存在しないこともある。
			// 🚨 ただし ENOENT 以外（権限・EIO）で読めないのを続行しない。WAL にしか無い
			// セッション Cookie が落ちて「0 件」になり、自動検出が黙って飛ばす。
			if suffix != "" && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			cleanup()
			if errors.Is(err, fs.ErrPermission) {
				// プロファイル固有として記録する（上の cookieDBSourcePath と同じ理由）。
				// 原因はフルディスクアクセス不足か、ファイルのパーミッション／所有者のどちらか。
				return "", nil, &profileDataError{err: fmt.Errorf(
					"Cookie DB を読み取れませんでした（アクセス拒否: %s）。\n%s  元エラー: %w", s, permissionHint, err)}
			}
			return "", nil, &profileDataError{err: fmt.Errorf("Cookie DB の読み取りに失敗（%s）: %w", s, err)}
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
	sweepStaleCookieDirs() // ③: 前回の実行が強制終了で残したものを先に片付ける

	password, err := getKeychainPassword()
	if err != nil {
		return nil, err // 型なし = 環境エラー（プロファイルに依存しない）
	}
	key, err := deriveKey(password)
	if err != nil {
		return nil, err
	}
	return readProfileCookies(profile, key)
}

// readProfileCookies は Keychain から得た鍵で、プロファイルの Cookie DB を読んで復号する。
// Keychain を読まないので、テストから本物の sqlite で実行できる。
//
// Keychain を読めた後の失敗（sqlite でない・テーブルが無い・全件復号できない）は
// このプロファイル固有の問題なので profileDataError で返す（自動検出は記録して次へ進む）。
func readProfileCookies(profile string, key []byte) ([]cookieEntry, error) {
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
		return nil, &profileDataError{err: fmt.Errorf("Cookie DB を開けません: %w", err)}
	}
	defer db.Close()

	var metaVersion int
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'version'`).Scan(&metaVersion); err != nil {
		// meta が読めない場合は 0 扱い（トリム無し）で続行
		metaVersion = 0
	}

	rows, err := db.Query(`SELECT host_key, name, value, encrypted_value FROM cookies`)
	if err != nil {
		return nil, &profileDataError{err: fmt.Errorf("cookies テーブルの読み取りに失敗: %w", err)}
	}
	defer rows.Close()

	var out []cookieEntry
	var tried, decrypted int
	for rows.Next() {
		var host, name, plainValue string
		var enc []byte
		if err := rows.Scan(&host, &name, &plainValue, &enc); err != nil {
			return nil, &profileDataError{err: fmt.Errorf("cookies テーブルの読み取りに失敗: %w", err)}
		}
		value := plainValue
		if value == "" && len(enc) > 0 {
			tried++
			v, derr := decryptValue(enc, key, metaVersion, host)
			if derr != nil {
				continue // 1 件の復号失敗で全体を止めない
			}
			decrypted++
			value = v
		}
		out = append(out, cookieEntry{host: host, name: name, value: value})
	}
	if err := rows.Err(); err != nil {
		return nil, &profileDataError{err: fmt.Errorf("cookies テーブルの読み取りに失敗: %w", err)}
	}
	// 🚨 全件の復号失敗を「Cookie 0 件」（= セッションが無い）に化けさせない。
	// 鍵がこの DB と合っていない（Keychain の項目が作り直された等）ことを示す。
	if tried > 0 && decrypted == 0 {
		return nil, &profileDataError{err: fmt.Errorf(
			"暗号化された Cookie を 1 件も復号できませんでした（%d 件）。Keychain の鍵がこのプロファイルと合っていない可能性があります", tried)}
	}
	return out, nil
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
