#!/usr/bin/env bash

set -euo pipefail

formula_path="Formula/stardrive.rb"
tag=""
checksums=""

usage() {
  cat <<'EOF'
Usage:
  scripts/update-homebrew-formula.sh --tag v0.1.0 --checksums dist/stardrive_0.1.0_checksums.txt
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      tag="${2:-}"
      shift 2
      ;;
    --checksums)
      checksums="${2:-}"
      shift 2
      ;;
    --output)
      formula_path="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

mkdir -p "$(dirname "$formula_path")"

lookup_checksum() {
  local filename="$1"
  awk -v target="$filename" '$2 == target { print $1 }' "$checksums"
}

if [[ -z "$tag" || -z "$checksums" ]]; then
  echo "--tag and --checksums are required" >&2
  usage >&2
  exit 1
fi

if [[ ! -f "$checksums" ]]; then
  echo "checksum file not found: $checksums" >&2
  exit 1
fi

version="${tag#v}"
darwin_arm64_archive="stardrive_${version}_darwin_arm64.tar.gz"
darwin_amd64_archive="stardrive_${version}_darwin_amd64.tar.gz"
linux_arm64_archive="stardrive_${version}_linux_arm64.tar.gz"
linux_amd64_archive="stardrive_${version}_linux_amd64.tar.gz"

darwin_arm64_sha="$(lookup_checksum "$darwin_arm64_archive")"
darwin_amd64_sha="$(lookup_checksum "$darwin_amd64_archive")"
linux_arm64_sha="$(lookup_checksum "$linux_arm64_archive")"
linux_amd64_sha="$(lookup_checksum "$linux_amd64_archive")"

require_checksum() {
  local archive="$1"
  local checksum="$2"

  if [[ -z "$checksum" ]]; then
    echo "missing required archive checksum for $archive in $checksums" >&2
    exit 1
  fi
}

require_checksum "$darwin_arm64_archive" "$darwin_arm64_sha"
require_checksum "$darwin_amd64_archive" "$darwin_amd64_sha"
require_checksum "$linux_arm64_archive" "$linux_arm64_sha"
require_checksum "$linux_amd64_archive" "$linux_amd64_sha"

cat >"$formula_path" <<EOF
class Stardrive < Formula
  desc "Manage Hetzner-hosted Talos clusters with Infisical-backed GitOps"
  homepage "https://github.com/intar-dev/stardrive"
  version "${version}"

  on_macos do
    on_arm do
      url "https://github.com/intar-dev/stardrive/releases/download/${tag}/${darwin_arm64_archive}"
      sha256 "${darwin_arm64_sha}"
    end

    on_intel do
      url "https://github.com/intar-dev/stardrive/releases/download/${tag}/${darwin_amd64_archive}"
      sha256 "${darwin_amd64_sha}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/intar-dev/stardrive/releases/download/${tag}/${linux_arm64_archive}"
      sha256 "${linux_arm64_sha}"
    end

    on_intel do
      url "https://github.com/intar-dev/stardrive/releases/download/${tag}/${linux_amd64_archive}"
      sha256 "${linux_amd64_sha}"
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
EOF
