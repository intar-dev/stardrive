class Stardrive < Formula
  desc "Manage Hetzner-hosted Talos clusters with Infisical-backed GitOps"
  homepage "https://github.com/intar-dev/stardrive"
  version "0.1.4"

  on_macos do
    on_arm do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.4/stardrive_0.1.4_darwin_arm64.tar.gz"
      sha256 "745adba8d4be743cc9a3c803d2c0a30b4c3c0a710fbcb61e34162767e156eb33"
    end

    on_intel do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.4/stardrive_0.1.4_darwin_amd64.tar.gz"
      sha256 "a5f22c76f10f55f925f470963855267e0fb9bc7dd526931fe7698811160e5650"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.4/stardrive_0.1.4_linux_arm64.tar.gz"
      sha256 "e8b9667a54bfa907f8c07fa3f3e8c3a7816cd13c5d226b40f1fb7d924216a0d3"
    end

    on_intel do
      url "https://github.com/intar-dev/stardrive/releases/download/v0.1.4/stardrive_0.1.4_linux_amd64.tar.gz"
      sha256 "2756af6876d9f37d9420262144439bcb07a166098866dd3a2737d9ad4771420f"
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
