class Bossanova < Formula
  desc "AI-powered pair programming workflow manager"
  homepage "https://github.com/bossanova-dev/bossanova"
  version "${VERSION}"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/boss-darwin-arm64"
      sha256 "${SHA256_DARWIN_ARM64_BOSS}"

      resource "bossd" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-darwin-arm64"
        sha256 "${SHA256_DARWIN_ARM64_BOSSD}"
      end

      resource "bossd-plugin-autopilot" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-plugin-autopilot-darwin-arm64"
        sha256 "${SHA256_DARWIN_ARM64_AUTOPILOT}"
      end

      resource "bossd-plugin-dependabot" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-plugin-dependabot-darwin-arm64"
        sha256 "${SHA256_DARWIN_ARM64_DEPENDABOT}"
      end

      resource "bossd-plugin-repair" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-plugin-repair-darwin-arm64"
        sha256 "${SHA256_DARWIN_ARM64_REPAIR}"
      end
    end

    on_intel do
      url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/boss-darwin-amd64"
      sha256 "${SHA256_DARWIN_AMD64_BOSS}"

      resource "bossd" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-darwin-amd64"
        sha256 "${SHA256_DARWIN_AMD64_BOSSD}"
      end

      resource "bossd-plugin-autopilot" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-plugin-autopilot-darwin-amd64"
        sha256 "${SHA256_DARWIN_AMD64_AUTOPILOT}"
      end

      resource "bossd-plugin-dependabot" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-plugin-dependabot-darwin-amd64"
        sha256 "${SHA256_DARWIN_AMD64_DEPENDABOT}"
      end

      resource "bossd-plugin-repair" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-plugin-repair-darwin-amd64"
        sha256 "${SHA256_DARWIN_AMD64_REPAIR}"
      end
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/boss-linux-amd64"
      sha256 "${SHA256_LINUX_AMD64_BOSS}"

      resource "bossd" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-linux-amd64"
        sha256 "${SHA256_LINUX_AMD64_BOSSD}"
      end

      resource "bossd-plugin-autopilot" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-plugin-autopilot-linux-amd64"
        sha256 "${SHA256_LINUX_AMD64_AUTOPILOT}"
      end

      resource "bossd-plugin-dependabot" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-plugin-dependabot-linux-amd64"
        sha256 "${SHA256_LINUX_AMD64_DEPENDABOT}"
      end

      resource "bossd-plugin-repair" do
        url "https://github.com/bossanova-dev/bossanova/releases/download/v${VERSION}/bossd-plugin-repair-linux-amd64"
        sha256 "${SHA256_LINUX_AMD64_REPAIR}"
      end
    end
  end

  def install
    bin.install buildpath/File.basename(stable.url) => "boss"
    resource("bossd").stage do
      bin.install Dir["bossd*"].first => "bossd"
    end
    (libexec/"plugins").mkpath
    %w[bossd-plugin-autopilot bossd-plugin-dependabot bossd-plugin-repair].each do |p|
      resource(p).stage { (libexec/"plugins").install Dir["#{p}*"].first => p }
    end
  end

  def post_install
    system bin/"boss", "config", "init", "--plugin-dir", libexec/"plugins"
  end

  test do
    assert_match "bossanova", shell_output("#{bin}/boss version")
  end
end
