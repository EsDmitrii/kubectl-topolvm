#!/usr/bin/env bash
# Render the krew manifest from krew/topolvm.yaml.tmpl with the release version and the sha256 of
# each platform archive built by goreleaser.
set -euo pipefail

VERSION="${1:?version required (no leading v)}"
DIST="${2:?dist dir required}"
OUT="${3:?output file required}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMPL="${SCRIPT_DIR}/topolvm.yaml.tmpl"

# sha256 of an archive, portable across sha256sum (Linux) and shasum (macOS).
sum() {
  local f="${DIST}/kubectl-topolvm_${VERSION}_${1}"
  if [ ! -f "$f" ]; then
    echo "missing archive: $f" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$f" | awk '{print $1}'
  else
    shasum -a 256 "$f" | awk '{print $1}'
  fi
}

# Assigned first, not inlined into sed: a failing $(sum ...) inside sed's arguments does not trip
# set -e.
linux_amd64=$(sum linux_amd64.tar.gz)
linux_arm64=$(sum linux_arm64.tar.gz)
darwin_amd64=$(sum darwin_amd64.tar.gz)
darwin_arm64=$(sum darwin_arm64.tar.gz)
windows_amd64=$(sum windows_amd64.zip)
windows_arm64=$(sum windows_arm64.zip)

sed \
  -e "s/{{VERSION}}/${VERSION}/g" \
  -e "s/{{SHA256_LINUX_AMD64}}/${linux_amd64}/g" \
  -e "s/{{SHA256_LINUX_ARM64}}/${linux_arm64}/g" \
  -e "s/{{SHA256_DARWIN_AMD64}}/${darwin_amd64}/g" \
  -e "s/{{SHA256_DARWIN_ARM64}}/${darwin_arm64}/g" \
  -e "s/{{SHA256_WINDOWS_AMD64}}/${windows_amd64}/g" \
  -e "s/{{SHA256_WINDOWS_ARM64}}/${windows_arm64}/g" \
  "$TMPL" > "$OUT"

echo "rendered $OUT for v${VERSION}"
