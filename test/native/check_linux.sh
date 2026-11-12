#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
src="$PWD/naiveproxy/src"
arch=$(go env GOHOSTARCH)
case "$arch" in
  amd64) cpu=x64; triple=x86_64-linux-gnu ;;
  arm64) cpu=arm64; triple=aarch64-linux-gnu ;;
  *) printf 'Unsupported native test architecture: %s\n' "$arch" >&2; exit 1 ;;
esac
out="$src/out/cronet-linux-$cpu"
clang="$src/third_party/llvm-build/Release+Asserts/bin/clang++"
sysroot="$src/out/sysroot-build/bullseye/bullseye_${arch}_staging"
artifacts=$(mktemp -d)
trap 'rm -rf "$artifacts"' EXIT

ninja -C "$out" -j 3 third_party/brotli:enc

flags=(
  "--target=$triple" "--sysroot=$sysroot" -std=c++23 -fuse-ld=lld
  -D_FILE_OFFSET_BITS=64 -D_GNU_SOURCE -DNDEBUG
  -DBORINGSSL_IMPLEMENTATION -DOPENSSL_SMALL
  -D_LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_EXTENSIVE
  -D_LIBCPP_DISABLE_VISIBILITY_ANNOTATIONS
  -D_LIBCXXABI_DISABLE_VISIBILITY_ANNOTATIONS
  -I "$src" -I "$out/gen"
  -I "$src/third_party/brotli/include"
  -I "$src/third_party/boringssl/src"
  -I "$src/third_party/boringssl/src/include"
  -I "$src/buildtools/third_party/libc++"
  -fno-exceptions -fno-rtti -nostdinc++
  -isystem "$src/third_party/libc++/src/include"
  -isystem "$src/third_party/libc++abi/src/include"
)
libs=("$PWD/lib/linux_$arch/libcronet.a" -ldl -lpthread -lrt -lm -lresolv)

# The fixture's assertions must remain enabled even with release ABI defines.
"$clang" "${flags[@]}" -UNDEBUG test/native/strict_ech_test.cc \
  "${libs[@]}" -o "$artifacts/strict-ech"
"$artifacts/strict-ech"

"$clang" "${flags[@]}" test/native/reality_test.cc \
  "$out/obj/third_party/brotli/libenc.a" "${libs[@]}" \
  -o "$artifacts/reality"
"$artifacts/reality"

"$clang" "${flags[@]}" -c test/native/reality_features_wrap.cc \
  -o "$artifacts/features.o"
eval "$(go run ./cmd/build-naive --target=linux/"$arch" env --export)"
export CRONET_TEST_CONNECT_EVENT=TLS_STREAM_ATTEMPT_CONNECT
symbol=_ZN4base11FeatureList12InitInstanceERKNSt4__Cr12basic_stringIcNS1_11char_traitsIcEENS1_9allocatorIcEEEES9_
go test -race -tags with_cronet_test -count=1 -timeout=5m \
  -ldflags "-extldflags '$artifacts/features.o -Wl,--wrap=$symbol'" \
  -run '^Test(Reality|BrowserStrictECHReuse|BrowserDuplex)' .

CRONET_TEST_FEATURE_OBJECT="$artifacts/features.o" bash test/native/check_sing_box.sh
