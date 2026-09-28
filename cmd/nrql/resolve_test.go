package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jiikko/dotfiles/src/chromecookie"
	"github.com/jiikko/dotfiles/src/chromecookie/chromecookietest"
)

// プロファイル選択の優先順位を固定する。
//
// 🚨 ここが入れ替わると「意図しないアカウントのデータを見ている」ことになる。
// しかも結果は普通に返るので、見ている対象が違うことに気づけない。
func TestResolveClientPriority(t *testing.T) {
	// 本物は Keychain と Chrome の保存領域を読むので、seam を差し替える。
	var asked []string
	restore := func() func() {
		orig := extractCookiesFn
		extractCookiesFn = func(profile string) (chromecookie.Result, error) {
			asked = append(asked, profile)
			return chromecookie.Result{Cookies: []cookieEntry{{Host: ".newrelic.com", Name: "session", Value: "v-" + profile}}}, nil
		}
		return func() { extractCookiesFn = orig }
	}()
	defer restore()

	t.Run("API キーがあればブラウザを読まない", func(t *testing.T) {
		asked = nil
		t.Setenv("NEW_RELIC_API_KEY", "NRAK-XXX")

		c, err := resolveClient(config{region: "us", profile: profileAuto})
		if err != nil {
			t.Fatalf("resolveClient: %v", err)
		}
		if c.mode != authAPIKey {
			t.Errorf("API キー方式になるべき: mode=%v", c.mode)
		}
		if c.endpoint != regions["us"].apiKey {
			t.Errorf("公開 NerdGraph を使うべき: %s", c.endpoint)
		}
		if len(asked) != 0 {
			t.Errorf("ブラウザの保存領域を読んでいる: %v", asked)
		}
	})

	t.Run("明示したプロファイルをそのまま使う", func(t *testing.T) {
		asked = nil
		t.Setenv("NEW_RELIC_API_KEY", "")

		c, err := resolveClient(config{region: "us", profile: "Profile 3"})
		if err != nil {
			t.Fatalf("resolveClient: %v", err)
		}
		if c.profile != "Profile 3" {
			t.Errorf("指定したプロファイルが使われていない: %q", c.profile)
		}
		if len(asked) != 1 || asked[0] != "Profile 3" {
			t.Errorf("読んだプロファイルが違う: %v", asked)
		}
		if c.mode != authCookie {
			t.Errorf("セッション方式になるべき: mode=%v", c.mode)
		}
	})

	t.Run("リージョンで接続先が変わる", func(t *testing.T) {
		t.Setenv("NEW_RELIC_API_KEY", "")
		c, err := resolveClient(config{region: "eu", profile: "Default"})
		if err != nil {
			t.Fatalf("resolveClient: %v", err)
		}
		if c.endpoint != regions["eu"].session {
			t.Errorf("EU の接続先になっていない: %s", c.endpoint)
		}
	})

	t.Run("未対応のリージョンは弾く", func(t *testing.T) {
		t.Setenv("NEW_RELIC_API_KEY", "")
		if _, err := resolveClient(config{region: "jp", profile: "Default"}); err == nil {
			t.Error("エラーになるべき")
		}
	})

	t.Run("対象ホスト宛てのセッションが無ければエラー", func(t *testing.T) {
		t.Setenv("NEW_RELIC_API_KEY", "")
		orig := extractCookiesFn
		extractCookiesFn = func(profile string) (chromecookie.Result, error) {
			return chromecookie.Result{Cookies: []cookieEntry{{Host: "example.com", Name: "x", Value: "y"}}}, nil
		}
		defer func() { extractCookiesFn = orig }()

		if _, err := resolveClient(config{region: "us", profile: "Default"}); err == nil {
			t.Error("無関係な Cookie しか無ければエラーになるべき")
		}
	})
}

// 候補が複数あるときは「認証が通る最初のもの」を選び、選んだことを stderr に出すこと。
func TestPickAuthenticatedClient(t *testing.T) {
	newFake := func(profile string, ok bool) *client {
		c := &client{
			mode:     authCookie,
			endpoint: "https://example.invalid/graphql",
			profile:  profile,
			http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if !ok {
					return &http.Response{
						StatusCode: http.StatusUnauthorized,
						Body:       http.NoBody,
						Request:    r,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       nopCloser{strings.NewReader(`{"data":{"actor":{"user":{"name":"x"}}}}`)},
					Request:    r,
				}, nil
			})},
		}
		return c
	}

	t.Run("認証が通る候補を選ぶ", func(t *testing.T) {
		// 🚨 正解を先頭に置かない（先頭だと「最初の候補を返す」実装でも通る）。
		got, err := pickAuthenticatedClient([]*client{
			newFake("Profile 1", false),
			newFake("Profile 2", true),
		}, "one.newrelic.com", skipNotes{})
		if err != nil {
			t.Fatalf("pickAuthenticatedClient: %v", err)
		}
		if got.profile != "Profile 2" {
			t.Errorf("認証が通る候補を選ぶべき: %q", got.profile)
		}
	})

	t.Run("全部失敗したらエラー", func(t *testing.T) {
		_, err := pickAuthenticatedClient([]*client{
			newFake("Profile 1", false),
			newFake("Profile 2", false),
		}, "one.newrelic.com", skipNotes{})
		if err == nil {
			t.Fatal("エラーになるべき")
		}
		if !strings.Contains(err.Error(), "2 件") {
			t.Errorf("試した件数が案内に無い: %v", err)
		}
	})
}

// 終了コードの出し分け。スクリプトから使うときの契約。
func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "成功", err: nil, want: exitOK},
		{name: "使い方の誤り", err: &usageError{"bad flag"}, want: exitUsage},
		{name: "実行時エラー", err: errors.New("boom"), want: exitRuntime},
		{name: "セッション切れは実行時エラー", err: &errSessionExpired{status: 401}, want: exitRuntime},
		{name: "NerdGraph のエラーは実行時エラー", err: &graphQLError{messages: []string{"x"}}, want: exitRuntime},
		{name: "包まれた使い方の誤りも 2", err: fmt.Errorf("wrapped: %w", &usageError{"bad"}), want: exitUsage},
	}
	for _, c := range cases {
		if got := exitCodeFor(c.err); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// -format の選択が実際に出力形式を変えること。
func TestRenderSelectsFormat(t *testing.T) {
	rows := []resultRow{{keys: []string{"count"}, values: map[string]any{"count": "42"}}}
	cases := []struct {
		format   string
		noHeader bool
		wantSub  string
		wantNot  string
	}{
		{format: "tsv", wantSub: "count\n42\n"},
		{format: "tsv", noHeader: true, wantSub: "42\n", wantNot: "count"},
		{format: "table", wantSub: "-----"},
		{format: "json", wantSub: `"count": "42"`},
		{format: "", wantSub: "count\n42\n"}, // 既定は TSV
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := render(&buf, config{format: c.format, noHeader: c.noHeader}, rows); err != nil {
			t.Fatalf("format=%q: %v", c.format, err)
		}
		if !strings.Contains(buf.String(), c.wantSub) {
			t.Errorf("format=%q noHeader=%v: %q に %q が無い", c.format, c.noHeader, buf.String(), c.wantSub)
		}
		if c.wantNot != "" && strings.Contains(buf.String(), c.wantNot) {
			t.Errorf("format=%q noHeader=%v: %q が出ている", c.format, c.noHeader, c.wantNot)
		}
	}
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

// -timeout / NRQL_TIMEOUT が実際にクライアントへ届くこと。
//
// 広い TIMESERIES / FACET で 60 秒に当たったとき、利用者が伸ばせる必要がある。
// 「フラグは在るが効いていない」を検出するため、クライアントの Timeout を直接見る。
func TestTimeoutReachesClient(t *testing.T) {
	orig := extractCookiesFn
	extractCookiesFn = func(profile string) (chromecookie.Result, error) {
		return chromecookie.Result{Cookies: []cookieEntry{{Host: ".newrelic.com", Name: "session", Value: "v"}}}, nil
	}
	defer func() { extractCookiesFn = orig }()
	t.Setenv("NEW_RELIC_API_KEY", "")

	c, err := resolveClient(config{region: "us", profile: "Default", timeout: 123})
	if err != nil {
		t.Fatalf("resolveClient: %v", err)
	}
	if c.http.Timeout != 123*time.Second {
		t.Errorf("セッション経路にタイムアウトが届いていない: %v", c.http.Timeout)
	}

	t.Setenv("NEW_RELIC_API_KEY", "NRAK-X")
	c2, err := resolveClient(config{region: "us", timeout: 77})
	if err != nil {
		t.Fatalf("resolveClient: %v", err)
	}
	if c2.http.Timeout != 77*time.Second {
		t.Errorf("API キー経路にタイムアウトが届いていない: %v", c2.http.Timeout)
	}

	// 0 や未指定は既定へ落ちること（フラグ未指定でも壊れない）。
	if got := newHTTPClient(0).Timeout; got != defaultTimeoutSeconds*time.Second {
		t.Errorf("既定にならない: %v", got)
	}
}

// プロファイルに依存しない失敗（ネットワーク断・タイムアウト・429・5xx）では即座に返し、
// それ以外（セッション切れ・user が null・200 + GraphQL エラー / 非 JSON）は次の候補へ進むこと。
//
// 🚨 次へ進むと、ネットワーク断では 候補数 × timeout 待った末に「いずれも認証が
// 通りませんでした」と誤案内する。fake は 2 番目の候補が呼ばれたかを数え、
// 「即座に返した」ことを呼び出し回数で見る（時間では測らない）。
func TestPickAuthenticatedClientStopsOnNonAuthErrors(t *testing.T) {
	type reply struct {
		status int
		body   string
		err    error
	}
	newFake := func(profile string, rep reply, calls *int) *client {
		return &client{
			mode:     authCookie,
			endpoint: "https://example.invalid/graphql",
			profile:  profile,
			http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				*calls++
				if rep.err != nil {
					return nil, rep.err
				}
				return &http.Response{
					StatusCode: rep.status,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       nopCloser{strings.NewReader(rep.body)},
					Request:    r,
				}, nil
			})},
		}
	}
	okReply := reply{status: http.StatusOK, body: `{"data":{"actor":{"user":{"name":"x"}}}}`}

	stop := []struct {
		name string
		rep  reply
		want string // エラー文に含まれるべき元の原因
	}{
		{name: "ネットワーク断", rep: reply{err: errors.New("dial tcp: connection refused")}, want: "connection refused"},
		{name: "429", rep: reply{status: http.StatusTooManyRequests}, want: "429"},
		{name: "5xx", rep: reply{status: http.StatusBadGateway, body: "bad gateway"}, want: "502"},
	}
	for _, c := range stop {
		t.Run("即座に返す: "+c.name, func(t *testing.T) {
			var first, second int
			_, err := pickAuthenticatedClient([]*client{
				newFake("Profile 1", c.rep, &first),
				newFake("Profile 2", okReply, &second),
			}, "one.newrelic.com", skipNotes{})
			if err == nil {
				t.Fatal("エラーを返すべき（認証以外の失敗を「次の候補」で覆い隠している）")
			}
			if second != 0 {
				t.Errorf("認証以外の失敗なのに次の候補へ進んだ（2 番目への ping %d 回）", second)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("元の原因が案内に無い: %v", err)
			}
			if strings.Contains(err.Error(), "いずれも認証が通りませんでした") {
				t.Errorf("認証の問題として誤案内している: %v", err)
			}
		})
	}

	next := []struct {
		name string
		rep  reply
	}{
		{name: "401", rep: reply{status: http.StatusUnauthorized}},
		{name: "302（ログインページ）", rep: reply{status: http.StatusFound}},
		{name: "user が null", rep: reply{status: http.StatusOK, body: `{"data":{"actor":{"user":null}}}`}},
		// 200 の形は未実測。旧実装どおり「このプロファイルは未認証」として次へ進む。
		{name: "200 + GraphQL エラー", rep: reply{status: http.StatusOK, body: `{"errors":[{"message":"boom"}]}`}},
		{name: "200 + HTML（ログインページ）", rep: reply{status: http.StatusOK, body: "<html>login</html>"}},
		// 即停止は 429 / 5xx だけ。想定外の 4xx はプロファイル（のセッション）由来かもしれないので次へ進む。
		{name: "400", rep: reply{status: http.StatusBadRequest}},
		{name: "404", rep: reply{status: http.StatusNotFound}},
		{name: "431", rep: reply{status: http.StatusRequestHeaderFieldsTooLarge}},
	}
	for _, c := range next {
		t.Run("次へ進む: "+c.name, func(t *testing.T) {
			var first, second int
			got, err := pickAuthenticatedClient([]*client{
				newFake("Profile 1", c.rep, &first),
				newFake("Profile 2", okReply, &second),
			}, "one.newrelic.com", skipNotes{})
			if err != nil {
				t.Fatalf("未認証の候補は飛ばして次を試すべき: %v", err)
			}
			if got.profile != "Profile 2" {
				t.Errorf("認証が通る候補を選ぶべき: %q", got.profile)
			}
		})
	}
}

// fakeChromeHome は隔離した HOME に、指定したプロファイルを列挙する Local State を置く。
// 🚨 本物の ~/Library/Application Support/Google/Chrome を読ませない。
func fakeChromeHome(t *testing.T, profiles ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Dir(chromecookietest.ProfileDir(home, "Default"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := map[string]any{}
	for _, p := range profiles {
		cache[p] = map[string]any{}
	}
	data, err := json.Marshal(map[string]any{"profile": map[string]any{"info_cache": cache}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Local State"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// 自動検出は「このプロファイルにセッションが無い」だけを飛ばし、プロファイルに依存しない
// 環境エラー（Keychain の拒否・フルディスクアクセス不足）は具体的な案内のまま即座に返すこと。
//
// 🚨 環境エラーまで飛ばすと、プロファイル数だけ security を起動した末に
// 「ログインしているか確認してください」と誤案内する（Keychain の許可が要る、に辿り着けない）。
func TestResolveClientAutoStopsOnEnvironmentErrors(t *testing.T) {
	t.Setenv("NEW_RELIC_API_KEY", "")
	fakeChromeHome(t, "Default", "Profile 1", "Profile 2")

	orig := extractCookiesFn
	defer func() { extractCookiesFn = orig }()

	t.Run("環境エラーは 1 回で返す", func(t *testing.T) {
		var asked []string
		envErr := errors.New("Keychain から暗号化キーを取得できませんでした（テスト）")
		extractCookiesFn = func(profile string) (chromecookie.Result, error) {
			asked = append(asked, profile)
			return chromecookie.Result{}, envErr
		}
		_, err := resolveClient(config{region: "us", profile: profileAuto})
		if !errors.Is(err, envErr) {
			t.Fatalf("環境エラーをそのまま返すべき: %v", err)
		}
		if len(asked) != 1 {
			t.Errorf("環境エラーなのにプロファイルを %d 件読んだ（1 件で止まるべき）: %v", len(asked), asked)
		}
	})

	t.Run("セッションの無いプロファイルは飛ばす", func(t *testing.T) {
		var asked []string
		extractCookiesFn = func(profile string) (chromecookie.Result, error) {
			asked = append(asked, profile)
			switch profile {
			case "Default":
				// 本物の「Cookie DB が無い」経路のエラーを使う（HOME 配下に DB を置いていない）。
				return chromecookie.Result{}, mustCookieDBMissing(t, profile)
			case "Profile 1":
				return chromecookie.Result{Cookies: []cookieEntry{{Host: "example.com", Name: "x", Value: "y"}}}, nil // 別ホスト宛てだけ
			}
			return chromecookie.Result{Cookies: []cookieEntry{{Host: ".newrelic.com", Name: "session", Value: "v"}}}, nil
		}
		c, err := resolveClient(config{region: "us", profile: profileAuto})
		if err != nil {
			t.Fatalf("セッションの無いプロファイルを飛ばして見つけるべき: %v", err)
		}
		if c.profile != "Profile 2" {
			t.Errorf("選んだプロファイルが違う: %q", c.profile)
		}
		if len(asked) != 3 {
			t.Errorf("全プロファイルを試していない: %v", asked)
		}
	})
}

// mustCookieDBMissing は本物の読み取り経路（readProfileCookies）が返す「DB が無い」エラーを取る。
func mustCookieDBMissing(t *testing.T, profile string) error {
	t.Helper()
	_, err := readProfileCookies(profile, []byte("k"))
	if err == nil {
		t.Fatalf("隔離した HOME に Cookie DB は無いはず（%s）", profile)
	}
	return err
}

// Cookie DB の有無を確かめられない（stat が ENOENT 以外で失敗する）ときは、
// 「DB が無い」（= 自動検出で黙って飛ばす）にせず、プロファイル固有の問題として記録させること。
func TestCookieDBSourcePathSeparatesUnreadableFromMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root では権限で stat を失敗させられない")
	}
	home := fakeChromeHome(t, "Default")
	base := filepath.Dir(chromecookietest.ProfileDir(home, "Default"))

	// 無いプロファイルは「セッションが無い」型。
	_, err := readProfileCookies("Profile 9", []byte("k"))
	var absent *profileWithoutSessionError
	if !errors.As(err, &absent) {
		t.Fatalf("DB の無いプロファイルは profileWithoutSessionError であるべき: %T %v", err, err)
	}

	// 実在するが読めない（x 権限が無く stat できない）プロファイルは環境エラー。
	prof := filepath.Join(base, "Default")
	if err := os.MkdirAll(filepath.Join(prof, "Network"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prof, "Network", "Cookies"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(prof, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(prof, 0o700) }) // t.TempDir の削除が失敗しないように戻す

	_, err = readProfileCookies("Default", []byte("k"))
	if err == nil {
		t.Fatal("stat できないのに成功した")
	}
	// 権限はプロファイル固有（chmod 000 / root 所有はフルディスクアクセスでは直らない）なので
	// 記録して飛ばす側。ただし「DB が無い」（黙って飛ばす）にはしない。
	var broken *profileDataError
	if errors.As(err, &absent) {
		t.Errorf("stat できない（権限）ことを「DB が無い」にしている: %v", err)
	}
	if !errors.As(err, &broken) {
		t.Errorf("権限はプロファイル固有の問題（profileDataError）として返すべき: %T %v", err, err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("権限であることを辿れない（最終エラーの案内に使う）: %v", err)
	}
}

// 使っていないプロファイルの壊れた DB 1 つで自動検出全体を止めないこと。
// 飛ばした理由はプロファイル名つきで、最終的に見つからなかったときの案内に添えること。
// 即座に返す環境エラーにもプロファイル名を付けること。
func TestResolveClientAutoSkipsBrokenProfilesButReportsThem(t *testing.T) {
	t.Setenv("NEW_RELIC_API_KEY", "")
	fakeChromeHome(t, "Default", "Profile 1", "Profile 2")
	orig := extractCookiesFn
	defer func() { extractCookiesFn = orig }()
	broken := func() error {
		return &profileDataError{err: errors.New("cookies テーブルの読み取りに失敗: file is not a database")}
	}

	t.Run("壊れた DB は飛ばして次を使う", func(t *testing.T) {
		extractCookiesFn = func(profile string) (chromecookie.Result, error) {
			if profile == "Profile 2" {
				return chromecookie.Result{Cookies: []cookieEntry{{Host: ".newrelic.com", Name: "session", Value: "v"}}}, nil
			}
			return chromecookie.Result{}, broken()
		}
		c, err := resolveClient(config{region: "us", profile: profileAuto})
		if err != nil {
			t.Fatalf("壊れた DB で自動検出全体を止めている: %v", err)
		}
		if c.profile != "Profile 2" {
			t.Errorf("選んだプロファイルが違う: %q", c.profile)
		}
	})

	t.Run("見つからなければ飛ばした理由を添える", func(t *testing.T) {
		extractCookiesFn = func(profile string) (chromecookie.Result, error) {
			if profile == "Profile 1" {
				return chromecookie.Result{}, mustCookieDBMissing(t, profile) // DB が無いだけ（理由は添えない）
			}
			return chromecookie.Result{}, broken()
		}
		_, err := resolveClient(config{region: "us", profile: profileAuto})
		if err == nil {
			t.Fatal("エラーになるべき")
		}
		msg := err.Error()
		for _, want := range []string{`"Default": cookies テーブル`, `"Profile 2": cookies テーブル`, "-profile"} {
			if !strings.Contains(msg, want) {
				t.Errorf("案内に %q が無い:\n%s", want, msg)
			}
		}
		if strings.Contains(msg, `"Profile 1"`) {
			t.Errorf("DB が無いだけのプロファイルまで理由に並べている:\n%s", msg)
		}
	})

	t.Run("環境エラーにはプロファイル名を付ける", func(t *testing.T) {
		extractCookiesFn = func(profile string) (chromecookie.Result, error) {
			return chromecookie.Result{}, errors.New("Keychain から暗号化キーを取得できませんでした")
		}
		_, err := resolveClient(config{region: "us", profile: profileAuto})
		if err == nil || !strings.Contains(err.Error(), `プロファイル "Default": Keychain`) {
			t.Errorf("プロファイル名が付いていない: %v", err)
		}
	})
}

// 全候補が未認証のとき、候補ごとの理由を最終エラーに添えること。
func TestPickAuthenticatedClientReportsReasons(t *testing.T) {
	html := func(profile string) *client {
		return &client{
			mode: authCookie, endpoint: "https://example.invalid/graphql", profile: profile,
			http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: nopCloser{strings.NewReader("<html>login</html>")}, Request: r}, nil
			})},
		}
	}
	_, err := pickAuthenticatedClient([]*client{html("Profile 1"), html("Profile 2")}, "one.newrelic.com",
		skipNotes{lines: []string{`"Profile 9": 壊れた DB`}})
	if err == nil {
		t.Fatal("エラーになるべき")
	}
	for _, want := range []string{"いずれも認証が通りませんでした", `"Profile 1": レスポンスを JSON として解釈できません`, `"Profile 2": `, `"Profile 9": 壊れた DB`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("案内に %q が無い:\n%v", want, err)
		}
	}
}

// 権限（EACCES）で読めないプロファイルは記録して飛ばし、後ろの正常なプロファイルを使うこと。
// 全プロファイルが権限で読めなければ、フルディスクアクセスとパーミッションの両方の案内を添えること。
//
// 🚨 権限を「即停止」にすると、chmod 000 や root 所有（sudo で起動した Chrome が作ったもの）の
// プロファイル 1 つで自動検出全体が止まる。フルディスクアクセスを付けても直らない。
// Keychain は読まない: extractCookiesFn を本物の readProfileCookies（固定の鍵）へ繋ぐ。
func TestResolveClientAutoSkipsPermissionDeniedProfiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root では権限で読み取りを失敗させられない")
	}
	t.Setenv("NEW_RELIC_API_KEY", "")
	orig := extractCookiesFn
	extractCookiesFn = func(profile string) (chromecookie.Result, error) { return readProfileCookies(profile, []byte("k")) }
	defer func() { extractCookiesFn = orig }()

	// lock は プロファイルのディレクトリを 0000 にする（t.Cleanup で戻す）。
	lock := func(t *testing.T, home, profile string) {
		dir := chromecookietest.ProfileDir(home, profile)
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}
	session := []fakeCookieRow{{Host: ".newrelic.com", Name: "session", Value: "v"}}
	setup := func(t *testing.T) string {
		home := fakeChromeHome(t, "Default", "Profile 1")
		makeCookieDB(t, home, "Default", 0, session)
		makeCookieDB(t, home, "Profile 1", 0, session)
		return home
	}

	t.Run("権限で読めないプロファイルの後ろを使う", func(t *testing.T) {
		home := setup(t)
		lock(t, home, "Default") // listChromeProfiles の順で先頭（sort 済み）
		c, err := resolveClient(config{region: "us", profile: profileAuto})
		if err != nil {
			t.Fatalf("権限で読めないプロファイル 1 つで自動検出全体を止めている: %v", err)
		}
		if c.profile != "Profile 1" {
			t.Errorf("後ろの正常なプロファイルを使うべき: %q", c.profile)
		}
	})

	t.Run("全部読めなければ権限の案内付きで返す", func(t *testing.T) {
		home := setup(t)
		lock(t, home, "Default")
		lock(t, home, "Profile 1")
		_, err := resolveClient(config{region: "us", profile: profileAuto})
		if err == nil {
			t.Fatal("エラーになるべき")
		}
		msg := err.Error()
		for _, want := range []string{`"Default": `, `"Profile 1": `, "フルディスクアクセス", "パーミッション"} {
			if !strings.Contains(msg, want) {
				t.Errorf("案内に %q が無い:\n%s", want, msg)
			}
		}
	})
}

// -profile を明示したとき、権限で stat できなければ直し方（FDA とパーミッション）を添えること。
func TestExplicitProfilePermissionErrorHasHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root では権限で stat を失敗させられない")
	}
	t.Setenv("NEW_RELIC_API_KEY", "")
	home := fakeChromeHome(t, "Default")
	makeCookieDB(t, home, "Default", 0, []fakeCookieRow{{Host: ".newrelic.com", Name: "session", Value: "v"}})
	dir := chromecookietest.ProfileDir(home, "Default")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	orig := extractCookiesFn
	extractCookiesFn = func(profile string) (chromecookie.Result, error) { return readProfileCookies(profile, []byte("k")) }
	defer func() { extractCookiesFn = orig }()

	_, err := resolveClient(config{region: "us", profile: "Default"})
	if err == nil {
		t.Fatal("エラーになるべき")
	}
	for _, want := range []string{"フルディスクアクセス", "パーミッション"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("案内に %q が無い: %v", want, err)
		}
	}
}
