#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
eval "$(go run ./cmd/build-naive --target=linux/mipsle --libc=musl env --export)"
src="$PWD/naiveproxy/src"
artifacts=$(mktemp -d)
trap 'rm -rf "$artifacts"' EXIT

"$src/third_party/llvm-build/Release+Asserts/bin/clang++" \
  --target=mipsel-openwrt-linux-musl "--sysroot=$QEMU_LD_PREFIX" \
  -march=mips32r2 -msoft-float -mabi=32 -std=c++23 -O2 -fPIC \
  -fno-exceptions -fno-rtti -nostdinc++ -nostdlib++ -DNDEBUG \
  -D_LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_EXTENSIVE \
  -D_LIBCPP_DISABLE_VISIBILITY_ANNOTATIONS \
  -D_LIBCXXABI_DISABLE_VISIBILITY_ANNOTATIONS \
  -I "$src/buildtools/third_party/libc++" \
  -I "$src/third_party/libc++/src" \
  -isystem "$src/third_party/libc++/src/include" \
  -isystem "$src/third_party/libc++abi/src/include" \
  test/native/override_detection_test.cc test/native/override_detection_override.cc \
  lib/linux_mipsle_musl/libcronet.a -fuse-ld=lld -Wl,-no-pie -static \
  -latomic -ldl -lpthread -lrt -lresolv -lm -o "$artifacts/override-detection"
timeout 30 qemu-mipsel "$artifacts/override-detection"
