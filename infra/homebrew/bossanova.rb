class Bossanova < Formula
  desc "AI-powered pair programming workflow manager"
  homepage "https://github.com/recurser/bossanova"
  version "${VERSION}"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/recurser/bossanova/releases/download/v${VERSION}/boss-darwin-arm64"
      sha256 "${SHA256_DARWIN_ARM64_BOSS}"

      resource "bossd" do
        url "https://github.com/recurser/bossanova/releases/download/v${VERSION}/bossd-darwin-arm64"
        sha256 "${SHA256_DARWIN_ARM64_BOSSD}"
      end
    end

    on_intel do
      url "https://github.com/recurser/bossanova/releases/download/v${VERSION}/boss-darwin-amd64"
      sha256 "${SHA256_DARWIN_AMD64_BOSS}"

      resource "bossd" do
        url "https://github.com/recurser/bossanova/releases/download/v${VERSION}/bossd-darwin-amd64"
        sha256 "${SHA256_DARWIN_AMD64_BOSSD}"
      end
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/recurser/bossanova/releases/download/v${VERSION}/boss-linux-amd64"
      sha256 "${SHA256_LINUX_AMD64_BOSS}"

      resource "bossd" do
        url "https://github.com/recurser/bossanova/releases/download/v${VERSION}/bossd-linux-amd64"
        sha256 "${SHA256_LINUX_AMD64_BOSSD}"
      end
    end
  end

  def install
    bin.install buildpath/File.basename(stable.url) => "boss"
    resource("bossd").stage do
      bin.install Dir["bossd*"].first => "bossd"
    end
  end

  test do
    assert_match "bossanova", shell_output("#{bin}/boss version")
  end
end
