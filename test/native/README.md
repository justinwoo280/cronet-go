# Native TLS regression tests

Run from the cronet-go repository root after building the Linux static library.
The commands use the packaged archive by default. For an unpackaged local rebuild,
set `CRONET_TEST_STATIC_LIB` to
`naiveproxy/src/out/cronet-linux-x64/obj/components/cronet/libcronet_static.a`.
`strict_ech_test.cc` uses the public API; its compile command is in
[BROWSER_TLS.md](../../BROWSER_TLS.md).

Build the test server's Brotli encoder with the matching native configuration:

```sh
ninja -C naiveproxy/src/out/cronet-linux-x64 -j 4 third_party/brotli:enc
```

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
  -I naiveproxy/src -I naiveproxy/src/out/cronet-linux-x64/gen \
  -I naiveproxy/src/third_party/brotli/include \
  -I naiveproxy/src/third_party/boringssl/src \
  -I naiveproxy/src/third_party/boringssl/src/include \
  -I naiveproxy/src/buildtools/third_party/libc++ \
  -fno-exceptions -fno-rtti -nostdinc++ \
  -isystem naiveproxy/src/third_party/libc++/src/include \
  -isystem naiveproxy/src/third_party/libc++abi/src/include \
  test/native/reality_test.cc "${CRONET_TEST_STATIC_LIB:-lib/linux_amd64/libcronet.a}" \
  naiveproxy/src/out/cronet-linux-x64/obj/third_party/brotli/libenc.a \
  -ldl -lpthread -lrt -lm -lresolv -o /tmp/opencode/cronet-reality-native-test
/tmp/opencode/cronet-reality-native-test
```

The fixture authenticates the emitted ClientHello with its server X25519 key,
then tests a complete TLS handshake and application transfer. Thirty combinations
cover dual/X25519-only/hybrid-only key shares, plain/Brotli-compressed certificates,
and success/bad certificate HMAC/bad CertificateVerify/bad Finished/HRR. Brotli
uses Chromium's production certificate decoder. Two additional cases reject a
low-order server key and missing authentication share before sending ClientHello.
Bad Finished changes the server's transcript
after signing CertificateVerify; the record-layer encryption is still valid.
Assertions are explicitly enabled in the test despite the native release ABI.

## Alternative native connection path

`reality_features_wrap.cc` is a test-only Linux/amd64 linker wrapper for the
current Chromium libc++ ABI. It enables HappyEyeballsV3 at process initialization
without modifying a production feature default. Compile it with the matching
compiler and libc++ flags, then link CGO tests using:

```sh
naiveproxy/src/third_party/llvm-build/Release+Asserts/bin/clang++ \
  --target=x86_64-linux-gnu \
  --sysroot=naiveproxy/src/out/sysroot-build/bullseye/bullseye_amd64_staging \
  -std=c++23 -D_FILE_OFFSET_BITS=64 -D_GNU_SOURCE -DNDEBUG \
  -D_LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_EXTENSIVE \
  -D_LIBCPP_DISABLE_VISIBILITY_ANNOTATIONS -D_LIBCXXABI_DISABLE_VISIBILITY_ANNOTATIONS \
  -I naiveproxy/src/buildtools/third_party/libc++ \
  -fno-exceptions -fno-rtti -nostdinc++ \
  -isystem naiveproxy/src/third_party/libc++/src/include \
  -isystem naiveproxy/src/third_party/libc++abi/src/include \
  -c test/native/reality_features_wrap.cc -o /tmp/opencode/reality_features_wrap.o
CGO_ENABLED=1 \
CC="$PWD/naiveproxy/src/third_party/llvm-build/Release+Asserts/bin/clang" \
CXX="$PWD/naiveproxy/src/third_party/llvm-build/Release+Asserts/bin/clang++" \
CGO_LDFLAGS=-fuse-ld=lld \
CRONET_TEST_CONNECT_EVENT=TLS_STREAM_ATTEMPT_CONNECT \
go test -race -tags with_cronet_test \
  -ldflags '-extldflags "/tmp/opencode/reality_features_wrap.o -Wl,--wrap=_ZN4base11FeatureList12InitInstanceERKNSt4__Cr12basic_stringIcNS1_11char_traitsIcEENS1_9allocatorIcEEEES9_ -Wl,--defsym=REALITY_REVIEW_REVISION=3"' \
  -run '^Test(Reality|BrowserStrictECHReuse|BrowserDuplex)' -count=1 -timeout=65s .
```

For an unpackaged build, also use a Go `-overlay` for
`lib_linux_amd64_cgo.go` to point its cgo linker directive at the rebuilt archive;
`CRONET_TEST_STATIC_LIB` only selects the standalone fixture's archive.
Set `CRONET_TEST_CONNECT_EVENT=SSL_CONNECT_JOB_SSL_CONNECT` for the normal build.
The REALITY camouflage test checks the actual NetLog event. Rebuild the native
archive after C++ changes and increment `REALITY_REVIEW_REVISION` to force the
final test executable to relink; Go's cache does not track arbitrary external
static-library updates. Do not distribute the feature wrapper.

The sing-box interop suite must also run with this variant. It covers
BidirectionalStream and URLRequest sharing an engine, including the framed
stream-up download/upload pair, successful REALITY sessions and reconnection.

The actual sing-box REALITY/XHTTP and VLESS interop tests live in the sibling
sing-box module. This fixture's server behavior is only for fault injection.
