#!/usr/bin/env bash
# Builds the two orb recorders from this checkout.
#
#   build.sh image  <profile.json> <tag> [base image]   Path A: a self-hosted orb image
#   build.sh bundle <config.json> <out folder>          Path B: a personal Amp plugin for hosted orbs
#
# A: the profile is `livy install --config` input; its sink must be the local folder dispatch.sh
#    collects (/home/user/.livy-outbox). The base defaults to Amp's x64 orb image; pass the ARM64
#    one on an ARM64 host. B: the config becomes ~/.livy/config.json in each orb; its sink is
#    {"type": "git", "remote": "https://github.com/<owner>/<private sink repo>.git"}.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"

linux_livy() { # arch out
  (cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$1" go build -trimpath -ldflags "-s -w" -o "$2" ./cmd/livy)
}

case "${1:-}" in
image)
  profile="${2:?profile.json}" tag="${3:?image tag}"
  base="${4:-us-central1-docker.pkg.dev/amp-prod-0/amp-orb/amp-orb:latest}"
  arch=amd64
  [[ "$base" == *arm64* ]] && arch=arm64
  ctx="$(mktemp -d)"
  linux_livy "$arch" "$ctx/livy"
  cp "$here/Dockerfile" "$ctx/" && cp "$profile" "$ctx/profile.json"
  docker build --platform "linux/$arch" --build-arg "BASE=$base" -t "$tag" "$ctx"
  rm -rf "$ctx"
  ;;
bundle)
  config="${2:?config.json}" out="${3:?out folder}"
  mkdir -p "$out"
  # linux_livy builds from the repository root, so a relative out must not be read from there.
  out="$(cd "$out" && pwd)"
  cp "$here/orb-plugin.ts" "$out/index.ts"
  (cd "$root" && go run ./deploy/amp-orbs/render -bin /home/user/.livy/bin/livy -data /home/user/.livy) >"$out/recorder.ts"
  for arch in amd64 arm64; do
    linux_livy "$arch" "$out/livy-linux-$arch"
    gzip -9 -f "$out/livy-linux-$arch"
  done
  cp "$config" "$out/config.json"
  ls -l "$out" >&2
  ;;
*)
  sed -n '2,10p' "$0" >&2
  exit 2
  ;;
esac
