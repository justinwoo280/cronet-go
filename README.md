# cronet-go

[![Reference](https://pkg.go.dev/badge/github.com/sagernet/cronet-go.svg)](https://pkg.go.dev/github.com/sagernet/cronet-go)

Go bindings for [naiveproxy](https://github.com/klzgrad/naiveproxy).

## Supported Platforms

| Target        | OS      | CPU   |
|---------------|---------|-------|
| android/386   | android | x86   |
| android/amd64 | android | x64   |
| android/arm   | android | arm   |
| android/arm64 | android | arm64 |
| darwin/amd64  | mac     | x64   |
| darwin/arm64  | mac     | arm64 |
| ios/arm64     | ios     | arm64 |
| ios/amd64     | ios     | amd64 |
| linux/386     | linux   | x86   |
| linux/amd64   | linux   | x64   |
| linux/arm     | linux   | arm   |
| linux/arm64   | linux   | arm64   |
| linux/loong64 | linux   | loong64 |
| windows/amd64 | win     | x64     |
| windows/arm64 | win     | arm64 |

## System Requirements

| Platform      | Minimum Version |
|---------------|-----------------|
| macOS         | 12.0 (Monterey) |
| iOS/tvOS      | 15.0            |
| Windows       | 10              |
| Android (current local native build) | 6.0 (API 23) |
| Linux (glibc)       | glibc 2.31 (loong64: 2.36) |
| Linux (musl)        | any (loong64: 1.2.5)       |

## Downstream Build Requirements

| Platform                             | Requirements                    | Go Build Flags                    |
|--------------------------------------|---------------------------------|-----------------------------------|
| Linux (glibc)                        | Chromium toolchain              | -                                 |
| Linux (musl)                         | Chromium toolchain              | `-tags with_musl`                 |
| macOS / iOS                          | macOS Xcode                     | -                                 |
| iOS simulator/ tvOS / tvOS simulator | macOS Xcode + SagerNet/gomobile | -                                 |
| Windows                              | -                               | `CGO_ENABLED=0 -tags with_purego` |
| Android                              | Android NDK                     | -                                 |

## Linux Build instructions

```bash
git clone --recursive --depth=1 https://github.com/sagernet/cronet-go.git
cd cronet-go
go run ./cmd/build-naive --target=linux/amd64 download-toolchain
#go run ./cmd/build-naive --target=linux/amd64 --libc=musl download-toolchain

# Outputs CC, CXX, and CGO_LDFLAGS=-fuse-ld=lld
export $(go run ./cmd/build-naive --target=linux/amd64 env)
#export $(go run ./cmd/build-naive --target=linux/amd64 --libc=musl env)

cd /path/to/your/project
go build
# go build -tags with_musl
```

### Directories to cache

```yaml
- cronet-go/naiveproxy/src/third_party/llvm-build/
- cronet-go/naiveproxy/src/gn/out/
- cronet-go/naiveproxy/src/chrome/build/pgo_profiles/
- cronet-go/naiveproxy/src/out/sysroot-build/
```

### Building the native library locally

On Debian, install `ninja-build`, `generate-ninja`, `gpgv`, `curl`, `unzip`,
`xz-utils` and a C/C++ compiler. `download-toolchain` supplies the Chromium
Clang, sysroot, PGO profile and the exact GN revision required by the source.
The build command uses that GN from `naiveproxy/src/gn/out/gn`.

```bash
go run ./cmd/build-naive --target=linux/amd64 build
go run ./cmd/build-naive --target=linux/amd64 package --local
eval "$(go run ./cmd/build-naive --target=linux/amd64 env --export)"
go test ./...
go test -tags with_cronet_test -race ./...
```

`package --local` installs generated headers in `include/`, libraries in `lib/`,
and local CGO linker flags. These generated artifacts are ignored by Git.

### Browser XHTTP

`NewBrowserXHTTPClient` exposes `packet-up` and `stream-up` sessions as `net.Conn`.
It implements XHTTP metadata/padding independently and uses Cronet for TLS,
HTTP/2, QUIC and connection reuse. An optional `DialContext` callback supplies
the actual TCP endpoint through the application's routing; this disables QUIC.
Direct TCP sockets are duplicated for Cronet, and other connections are relayed
through a socketpair. `Host` is independent of the URL's TLS hostname when using
the native library built from this checkout.

ECH is supported over the custom TCP dialer with an HTTPS DNS hostname. Supply
the wire-format `ECHConfigList` or a `GetECHConfigList(context.Context)` callback
that resolves and caches it. The callback runs before an XHTTP session starts;
lookup errors and empty configurations fail the dial. An internal DNS socketpair
passes HTTPS records to Chromium's native ECH implementation; it does not bind a
local DNS port. Browser ECH uses a fixed, per-engine Strict policy: authenticated
retries with a usable configuration are supported; empty or unusable retry lists
fail instead of falling back to ordinary TLS. This requires the patched native
library; the native socket also requires TLS 1.3 whenever ECH is active. PureGo
reports an explicit error when an older library lacks the API.
Existing HTTP/2 connections remain reusable when a DNS configuration expires;
refreshed settings apply to subsequent handshakes.

REALITY is supported through `BrowserXHTTPOptions.Reality`, containing a 32-byte
X25519 public key and an eight-byte short ID. Browser owns a dedicated engine and
uses the native REALITY handshake with `ClientVer=1.8.1`. The certificate HMAC is
authenticated before allowing unadvertised Ed25519; CertificateVerify and
Finished remain cryptographically verified. HTTP/2 is reused; TLS resumption,
0-RTT, real ECH and QUIC are disabled for this mode. The native socket requires
TLS 1.3 for REALITY and its ordinary camouflage connection. Authenticated REALITY peers
omitting ALPN use h2 if offered, for compatibility with sing-box's server.
If REALITY authentication fails but the ordinary website certificate and TLS
handshake verify, the original request fails while an independent `GET /` runs
on that same connection. It uses Cronet's HTTP stack and a generated padding
cookie, drains the response, and never sends the XHTTP request to the website.
Older PureGo libraries explicitly reject REALITY initialization.

See [BROWSER_TLS.md](BROWSER_TLS.md) for the local Chromium ClientHello comparison
and ECH/REALITY verification results.

Native tests require `with_cronet_test`. For PureGo, also use `with_purego` and
set `LD_LIBRARY_PATH` to the directory containing the built `libcronet.so`.

For Android applications built with gomobile, use CGO static linking. Cronet is
included in the Go JNI library inside the AAR; a separate `libcronet.so` is not
required. See [ANDROID_BROWSER.md](ANDROID_BROWSER.md) for the SekaiMod build
path, ABI mapping and minimum Android API requirements.

## Windows / purego Build Instructions

For Windows or pure Go builds (no CGO), you need to distribute the dynamic library alongside your binary.

### Download Library

Download `libcronet.dll` (Windows) or `libcronet.so` (Linux) from [GitHub Releases](https://github.com/sagernet/cronet-go/releases).

### Build with purego

```bash
# Windows (purego is required)
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags with_purego -o myapp.exe

# Linux with purego (optional, for dynamic linking)
CGO_ENABLED=0 go build -tags with_purego -o myapp
```

### Distribution

Place the library file in the same directory as your executable:
- Windows: `libcronet.dll`
- Linux: `libcronet.so`

### For Downstream Developers

The `go` branch holds the libraries. The same commit supplies the
`github.com/sagernet/cronet-go/lib/*` modules. Pin this commit. Read the library file from it:

```bash
git init cronet-go-lib
git -C cronet-go-lib remote add origin https://github.com/sagernet/cronet-go.git
git -C cronet-go-lib sparse-checkout set --no-cone /lib/windows_amd64/libcronet.dll
git -C cronet-go-lib fetch --depth=1 --filter=blob:none origin "$CRONET_GO_VERSION"
git -C cronet-go-lib checkout FETCH_HEAD
```
