class Stardrive < Formula
  desc "Manage Hetzner-hosted Talos clusters with Infisical-backed GitOps"
  homepage "https://github.com/intar-dev/stardrive"
  version "0.1.2"

  on_macos do
    on_arm do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.2/stardrive_0.1.2_darwin_arm64.tar.gz"
      sha256 "cb35646f507171aaca0b8f8de6b608b5f04d36653e24929030e3c173688e6688"
    end

    on_intel do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.2/stardrive_0.1.2_darwin_amd64.tar.gz"
      sha256 "27987ff0341929be65a53a6d82f33a1f298fa37b14787bb116b2274bd1bb228d"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.2/stardrive_0.1.2_linux_arm64.tar.gz"
      sha256 "2d029b66503f558b08b2d9b0f22fbfc328313b0e0232af402faa3a856584ab79"
    end

    on_intel do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.2/stardrive_0.1.2_linux_amd64.tar.gz"
      sha256 "69c470eeca93bdadacab90f9e60a29fa1f1f7495eb8f2feda041e93015e72985"
    end
  end

  def install
    bin.install "stardrive"
  end

  test do
    output = shell_output("#{bin}/stardrive version")
    assert_match "stardrive", output
  end
end
