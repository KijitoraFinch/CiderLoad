class Ciderload < Formula
  desc "Accelerate Homebrew downloads with parallel Range requests"
  homepage "https://github.com/KijitoraFinch/CiderLoad"
  url "https://github.com/KijitoraFinch/CiderLoad.git",
      using: :git,
      tag: "v0.1.1"
  # Homebrew uses the MIT option; bundled components retain their licenses.
  license all_of: ["MIT", "BSD-3-Clause", "MPL-2.0"]

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-X ciderload/internal/app.Version=#{version}"), "./cmd/ciderload"
    system "go", "build", *std_go_args(output: bin/"cidl"), "./cmd/cidl"
    pkgshare.install "LICENSE", "LICENSE-MIT", "LICENSE-MIT-SUSHI-WARE", "THIRD_PARTY_NOTICES", "licenses"
  end

  test do
    ENV["HOMEBREW_NO_AUTO_UPDATE"] = "1"
    ENV["HOMEBREW_CACHE"] = testpath/"cache"
    ENV["HOMEBREW_CIDERLOAD_STATE_DIR"] = testpath/"state"
    ENV["XDG_CONFIG_HOME"] = testpath/"config"
    ENV["CIDL_BREW"] = HOMEBREW_PREFIX/"bin/brew"

    assert_equal "CiderLoad #{version}\n", shell_output("#{bin}/ciderload --ciderload-version")
    assert_equal (bin/"ciderload").realpath.to_s,
                 shell_output("#{bin}/cidl ruby -r utils/curl -e 'puts Utils::Curl.curl_path'").strip

    (testpath/"source").write "CiderLoad download test\n"
    system bin/"ciderload", "--fail", "--silent", "--show-error",
           "--output", testpath/"download", "file://#{testpath}/source"
    assert_equal (testpath/"source").read, (testpath/"download").read
  end
end
