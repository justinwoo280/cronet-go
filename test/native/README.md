# Native TLS regression tests

Run from the cronet-go repository root after building and packaging the Linux
static library. `strict_ech_test.cc` uses the public API; its compile command is
in [BROWSER_TLS.md](../../BROWSER_TLS.md).

`reality_test.cc` uses internal BoringSSL interfaces to inject specific malformed
server flights. Match the native build's release defines and Chromium libc++
headers when compiling it. For the current local Linux/amd64 build:

```sh
naiveproxy/src/third_party/llvm-build/Release+Asserts/bin/clang++ \
  --target=x86_64-linux-gnu \
  --sysroot=naiveproxy/src/out/sysroot-build/bullseye/bullseye_amd64_staging \
  -std=c++23 -fuse-ld=lld \
  -D_FILE_OFFSET_BITS=64 -D_GNU_SOURCE -DNDEBUG \
  -DBORINGSSL_IMPLEMENTATION -DOPENSSL_SMALL \
  -D_LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_EXTENSIVE \
  -D_LIBCPP_DISABLE_VISIBILITY_ANNOTATIONS -D_LIBCXXABI_DISABLE_VISIBILITY_ANNOTATIONS \
  -I naiveproxy/src/third_party/boringssl/src \
  -I naiveproxy/src/third_party/boringssl/src/include \
  -I naiveproxy/src/buildtools/third_party/libc++ \
  -fno-exceptions -fno-rtti -nostdinc++ \
  -isystem naiveproxy/src/third_party/libc++/src/include \
  -isystem naiveproxy/src/third_party/libc++abi/src/include \
  test/native/reality_test.cc lib/linux_amd64/libcronet.a \
  -ldl -lpthread -lrt -lm -lresolv -o /tmp/opencode/cronet-reality-native-test
/tmp/opencode/cronet-reality-native-test
```

The fixture authenticates the emitted ClientHello with its server X25519 key,
then tests a complete TLS handshake and application transfer. Ten combinations
cover dual/hybrid-only key shares and success/bad certificate HMAC/bad
CertificateVerify/bad Finished/HRR. Bad Finished changes the server's transcript
after signing CertificateVerify; the record-layer encryption is still valid.
Assertions are explicitly enabled in the test despite the native release ABI.

The actual sing-box REALITY/XHTTP and VLESS interop tests live in the sibling
sing-box module. This fixture's server behavior is only for fault injection.
