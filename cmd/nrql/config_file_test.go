package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 壊れた項目だけを落とし、他の項目は読めた値のまま使うこと。落とした項目は名前で返すこと。
//
// 🚨 救済を「特定の項目を除いてもう一度読む」形にすると、除いていない項目が壊れたとき
// 全部が落ちる（timeout: abc があるだけで正しい account が消えた）。項目ごとに独立に読む。
func TestParseFileConfigDropsOnlyBrokenFields(t *testing.T) {
	cases := []struct {
		name       string
		yaml       string
		want       fileConfig
		wantFields []string // nil = エラーなし
	}{
		{
			name:       "timeout だけ壊れている",
			yaml:       "account: 1234567\ntimeout: abc\nregion: eu\nprofile: Profile 7\n",
			want:       fileConfig{Account: 1234567, Region: "eu", Profile: "Profile 7"},
			wantFields: []string{"timeout"},
		},
		{
			name:       "account だけ壊れている",
			yaml:       "account: true\ntimeout: 90\nregion: eu\n",
			want:       fileConfig{Timeout: 90, Region: "eu"},
			wantFields: []string{"account"},
		},
		{
			name:       "region と timeout が壊れている",
			yaml:       "region: [eu]\naccount: 42\ntimeout: 0x10\n",
			want:       fileConfig{Account: 42},
			wantFields: []string{"region", "timeout"},
		},
		{
			// どちらが意図した値か分からないので、その項目は使わない。
			name:       "重複したキー",
			yaml:       "region: us\naccount: 42\nregion: eu\n",
			want:       fileConfig{Account: 42},
			wantFields: []string{"region"},
		},
		{
			name: "正常",
			yaml: "account: \"1234567\"\nregion: us\nprofile: Default\ntimeout: 060\n",
			want: fileConfig{Account: 1234567, Region: "us", Profile: "Default", Timeout: 60},
		},
		{name: "空", yaml: "", want: fileConfig{}},
		{name: "コメントだけ", yaml: "# only\n", want: fileConfig{}},
		{name: "null 文書", yaml: "~\n", want: fileConfig{}},
		{name: "知らないキーは無視", yaml: "color: red\nregion: eu\n", want: fileConfig{Region: "eu"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFileConfig([]byte(c.yaml))
			if got != c.want {
				t.Errorf("読めた値が違う:\ngot  %+v\nwant %+v", got, c.want)
			}
			if c.wantFields == nil {
				if err != nil {
					t.Errorf("エラーになってはいけない: %v", err)
				}
				return
			}
			var fe *fileConfigFieldError
			if !errors.As(err, &fe) {
				t.Fatalf("項目ごとのエラーを返すべき: %T %v", err, err)
			}
			if !reflect.DeepEqual(fe.fieldNames(), c.wantFields) {
				t.Errorf("落とした項目の名前が違う: got %v, want %v", fe.fieldNames(), c.wantFields)
			}
		})
	}
}

// 文書として壊れている（構文エラー / マッピングでない）ときは何も読めたことにしない。
func TestParseFileConfigRejectsUnreadableDocuments(t *testing.T) {
	for _, body := range []string{"account: [\n", "- a\n- b\n", "just a string\n"} {
		got, err := parseFileConfig([]byte(body))
		if err == nil {
			t.Errorf("%q: エラーになるべき", body)
		}
		if got != (fileConfig{}) {
			t.Errorf("%q: 読めていないのに値がある: %+v", body, got)
		}
	}
}

// 一部の項目が壊れた config.yml を config set が上書きしないこと（既存のガードとの関係）。
//
// 🚨 項目ごとに救済するようになっても、「完全には読めていない」ことは error で伝え続ける。
// ここが nil になると、壊れた項目を消した内容で書き戻してしまう。
func TestConfigSetRefusesPartiallyBrokenFile(t *testing.T) {
	const body = "account: 1234567\ntimeout: abc\n"
	writeConfig(t, body)
	path, err := configFilePath()
	if err != nil {
		t.Fatal(err)
	}

	err = cmdConfig([]string{"set", "region", "eu"})
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("書き込みを拒む usageError を返すべき: %v", err)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(data) != body {
		t.Errorf("壊れた設定ファイルを書き換えた:\n%s", data)
	}
	// 読めた項目は使われていること。
	if got := int(loadFileConfig().Account); got != 1234567 {
		t.Errorf("読めた account が捨てられた: %d", got)
	}
}

// config.yml が「在るが読めない」（ENOENT 以外の失敗）ときは、無いことにしないこと。
//
// 🚨 無い扱いにすると fileConfigErr が nil のままで config set の上書きガードを素通りし、
// 書き込み権限だけがあるファイル（0200）をゼロ値から書き直して中身を消す。
func TestUnreadableConfigFileIsNotTreatedAsMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root では権限で読み取りを失敗させられない")
	}
	const body = "region: eu\nprofile: Profile 7\n"
	writeConfig(t, body)
	path, err := configFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o200); err != nil { // 書けるが読めない
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if err := fileConfigProblem(); err == nil {
		t.Fatal("読めない config.yml を「無い」扱いにしている（fileConfigProblem が nil）")
	}
	err = cmdConfig([]string{"set", "account", "42"})
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("書き込みを拒む usageError を返すべき: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(data) != body {
		t.Errorf("読めなかった設定ファイルを上書きした:\n%s", data)
	}

	// 無いファイルは従来どおりエラーにしない（初回の config set を拒まない）。
	writeConfig(t, "")
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "newrelic-nrql-cli", "config.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("前提: config.yml は無いはず: %v", err)
	}
	if err := fileConfigProblem(); err != nil {
		t.Errorf("config.yml が無いだけでエラーにしている: %v", err)
	}
}

// YAML のマージキー（<<）を展開すること（旧実装が通していた 3 形）。
//
// 🚨 項目ごとの読み込みだけにすると << が「知らないキー」として黙って捨てられ、
// err=nil のまま region: eu が us に戻る（2 本のレビューが独立に再現した退行）。
func TestParseFileConfigExpandsMergeKeys(t *testing.T) {
	for name, body := range map[string]string{
		"エイリアス": "base: &b\n  region: eu\n  profile: Profile 7\n<<: *b\naccount: 42\n",
		"インライン": "<<: {region: eu, profile: Profile 7}\naccount: 42\n",
		"シーケンス": "a: &a {region: eu}\nb: &b {profile: Profile 7}\n<<: [*a, *b]\naccount: 42\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseFileConfig([]byte(body))
			if err != nil {
				t.Fatalf("エラーになってはいけない: %v", err)
			}
			want := fileConfig{Account: 42, Region: "eu", Profile: "Profile 7"}
			if got != want {
				t.Errorf("マージが展開されていない:\ngot  %+v\nwant %+v", got, want)
			}
		})
	}

	// 別の項目が壊れて救済経路に落ちたとき。
	cases := []struct {
		name       string
		yaml       string
		want       fileConfig
		wantFields []string
	}{
		{
			// 🚨 HEAD（旧実装）は region: eu を残していた形。マージを捨てると us に戻る。
			name:       "トップレベルの項目が壊れてもマージは展開する",
			yaml:       "base: &b\n  region: eu\n<<: *b\naccount: abc\n",
			want:       fileConfig{Region: "eu"},
			wantFields: []string{"account"},
		},
		{
			name:       "インラインのマージ + 壊れた timeout",
			yaml:       "<<: {region: eu}\naccount: 42\ntimeout: abc\n",
			want:       fileConfig{Account: 42, Region: "eu"},
			wantFields: []string{"timeout"},
		},
		{
			// マージ元の中の壊れた値は、その項目だけ落とす（region は失わない）。
			name:       "マージ元の中の timeout が壊れている",
			yaml:       "base: &b {region: eu, timeout: abc}\n<<: *b\naccount: 42\n",
			want:       fileConfig{Account: 42, Region: "eu"},
			wantFields: []string{"timeout"},
		},
		// 以下は 3 周目レビューが再現した形（自前の近似では値が戻る / region を失う）。
		{
			// 🚨 トップレベルの壊れた account をマージ元の 123 で埋めない（警告では「使わない」と言っている）。
			name:       "トップレベルが壊れてもマージ元で埋めない",
			yaml:       "base: &b {account: 123, region: eu}\n<<: *b\naccount: bad\n",
			want:       fileConfig{Region: "eu"},
			wantFields: []string{"account"},
		},
		{
			name:       "重複キーもマージ元で埋めない",
			yaml:       "base: &b {account: 123, region: eu}\n<<: *b\naccount: 1\naccount: 2\n",
			want:       fileConfig{Region: "eu"},
			wantFields: []string{"account"},
		},
		{
			// トップレベルが優先されるので、マージ元の壊れた account は使われない（表に出さない）。
			name:       "マージ元とトップレベルの両方が壊れている",
			yaml:       "base: &b {account: bad, region: eu}\n<<: *b\naccount: bad2\n",
			want:       fileConfig{Region: "eu"},
			wantFields: []string{"account"},
		},
		{
			// マージそのものを展開できない（スカラーのマージ）。トップレベルの項目だけ読み、
			// 「<<」を実際のエラー付きで積む。
			name:       "展開できないマージ",
			yaml:       "<<: 5\nregion: eu\naccount: bad\n",
			want:       fileConfig{Region: "eu"},
			wantFields: []string{"<<", "account"},
		},
		{
			name:       "入れ子のマージの中の timeout が壊れている",
			yaml:       "x: &x {timeout: bad}\nbase: &b {<<: *x, region: eu}\n<<: *b\n",
			want:       fileConfig{Region: "eu"},
			wantFields: []string{"timeout"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFileConfig([]byte(c.yaml))
			var fe *fileConfigFieldError
			if !errors.As(err, &fe) {
				t.Fatalf("項目ごとのエラーを返すべき: %T %v", err, err)
			}
			if !reflect.DeepEqual(fe.fieldNames(), c.wantFields) {
				t.Errorf("落とした項目が違う: got %v, want %v", fe.fieldNames(), c.wantFields)
			}
			if got != c.want {
				t.Errorf("読めた値が違う:\ngot  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// 一括では失敗したのに項目単位では問題が見つからない形（知らないキーの重複）でも、
// error を返し続けること（config set が上書きを拒む根拠）。読めた値は使う。
func TestParseFileConfigKeepsErrorWhenNoFieldIsBroken(t *testing.T) {
	got, err := parseFileConfig([]byte("color: a\ncolor: b\nregion: eu\n"))
	if err == nil {
		t.Fatal("一括で読めなかったファイルを「完全に読めた」にしている")
	}
	if got.Region != "eu" {
		t.Errorf("読めた region を捨てた: %+v", got)
	}
}

// 解析に失敗した config.yml の警告が、使わない項目の名前を出すこと。
//
// 🚨 利用者が「どの設定が効いていないか」を知る手段はこの警告だけ（rc は変わらない）。
func TestLoadFileConfigWarnsDroppedFieldNames(t *testing.T) {
	writeConfig(t, "account: 1234567\ntimeout: abc\nregion: eu\n")
	var fc fileConfig
	out := captureStderr(t, func() { fc = loadFileConfig() })
	if !strings.Contains(out, "（timeout は使いません。それ以外の項目は読めた値を使います）") {
		t.Errorf("使わない項目の名前が警告に無い:\n%s", out)
	}
	if strings.Contains(out, "account は使いません") || strings.Contains(out, "region は使いません") {
		t.Errorf("読めた項目まで使わないと警告している:\n%s", out)
	}
	if int(fc.Account) != 1234567 || fc.Region != "eu" {
		t.Errorf("読めた値を使っていない: %+v", fc)
	}
}
