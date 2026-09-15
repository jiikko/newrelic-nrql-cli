class Nrql < Formula
  desc "New Relic に NRQL を投げる読み取り専用 CLI（Chrome のログインセッションを利用）"
  homepage "https://github.com/jiikko/newrelic-nrql-cli"
  url "https://github.com/jiikko/newrelic-nrql-cli/archive/refs/tags/v0.1.2.tar.gz"
  sha256 "ae8f0420b70ac97577d9a62a6a043f5407648d827891e1330d13fa0ab6614ebd"
  license "MIT"
  head "https://github.com/jiikko/newrelic-nrql-cli.git", branch: "master"

  depends_on "go" => :build
  depends_on :macos

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w"), "./cmd/nrql"
  end

  test do
    assert_match "nrql - ", shell_output("#{bin}/nrql --help")
    # NRQL 未指定は終了コード 2（使い方エラー）
    output = shell_output("#{bin}/nrql query 2>&1", 2)
    assert_match "NRQL を指定してください", output
  end
end
