#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
root="$PWD"
arch=$(go env GOHOSTARCH)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Pin the independent REALITY server and Browser/VLESS interoperability tests.
revision=437d58e02ff2c41795b91415fbf0bfc49936ffaa
mkdir -p "$work/sing-box" "$work/all"
curl -fsSL --retry 3 \
  "https://codeload.github.com/justinwoo280/sing-box/tar.gz/$revision" |
  tar -xz --strip-components=1 -C "$work/sing-box"

# package --local supplies the archive through the root Cronet module. Avoid
# also linking a published (older) native archive through cronet-go/all.
printf 'module github.com/sagernet/cronet-go/all\n\ngo 1.24.0\n' > "$work/all/go.mod"
printf 'package all\n' > "$work/all/all.go"
(
  cd "$work"
  go work init "$root" ./sing-box ./all
)
export GOWORK="$work/go.work"
export LD_LIBRARY_PATH="$root/lib/linux_$arch${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
eval "$(go run ./cmd/build-naive --target=linux/"$arch" env --export)"

tests='^TestBrowser(Interop|MetadataInterop|RealityInterop|RealityVLESS|ResetActiveConnection|ConnectionLossRecovery)$'
packages=(./transport/v2rayxhttp ./protocol/vless)
go -C "$work/sing-box" test -race -tags 'with_cronet with_cronet_test with_utls' \
  -count=1 -timeout=5m -run "$tests" "${packages[@]}"
go -C "$work/sing-box" test -race \
  -tags 'with_cronet with_cronet_test with_utls with_purego' \
  -count=1 -timeout=5m -run "$tests" "${packages[@]}"

if [[ -n ${CRONET_TEST_FEATURE_OBJECT:-} ]]; then
  symbol=_ZN4base11FeatureList12InitInstanceERKNSt4__Cr12basic_stringIcNS1_11char_traitsIcEENS1_9allocatorIcEEEES9_
  go -C "$work/sing-box" test -race -tags 'with_cronet with_cronet_test with_utls' \
    -count=1 -timeout=5m -run "$tests" \
    -ldflags "-extldflags '$CRONET_TEST_FEATURE_OBJECT -Wl,--wrap=$symbol'" \
    "${packages[@]}"
fi
