class Stardrive < Formula
  desc "Manage Hetzner-hosted Talos clusters with Infisical-backed GitOps"
  homepage "https://github.com/intar-dev/stardrive"
  version "0.1.3"

  on_macos do
    on_arm do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.3/stardrive_0.1.3_darwin_arm64.tar.gz"
      sha256 "05662d57bec7f169bb55d86427b04d58d904873e913f28048cf9e40f1ed4ab0b"
    end

    on_intel do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.3/stardrive_0.1.3_darwin_amd64.tar.gz"
      sha256 "4533b8e974e18f5d32099635d0cba41c546db65d314a49de0a2fb5c7a5616132"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.3/stardrive_0.1.3_linux_arm64.tar.gz"
      sha256 "d983c859ac30f0e5795e8d8dcf8de5ea2443b9473b301b8244a2c2ced8470924"
    end

    on_intel do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.3/stardrive_0.1.3_linux_amd64.tar.gz"
      sha256 "bf789319e4081deb6da3b404dbb91dfdb2f674050364c4af93134aefa291f79a"
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
