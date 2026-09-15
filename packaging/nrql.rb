class Nrql < Formula
  desc "New Relic に NRQL を投げる読み取り専用 CLI（Chrome のログインセッションを利用）"
  homepage "https://github.com/jiikko/newrelic-nrql-cli"
  url "https://github.com/jiikko/newrelic-nrql-cli/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "61a895e0d0df1ec3b9b51b1698c87dc5dc5131b7ace14af2404e536c3ea71cb1"
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
