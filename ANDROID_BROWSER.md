# Browser XHTTP in SekaiMod on Android

SekaiMod currently builds `libcore.aar` using `gomobile bind`. Use cronet-go's
normal CGO bindings and statically link `libcronet.a` into the Go JNI library
inside that AAR. Gradle already packages `app/libs/libcore.aar`; an additional
`libcronet.so`, Cronet Java AAR, WebView, or Play Services dependency is unnecessary.
Cronet code still increases the native library and APK size.

```text
SekaiMod → libcore.aar → Go JNI library
                        ├─ sing-box Browser XHTTP
                        ├─ cronet-go CGO bindings
                        └─ Chromium/BoringSSL from libcronet.a
```

The Android arm64 native archive, `libcore.aar`, and SekaiMod `ossDebug` APK have
been built for API 23. Protocol integration tests and libcore configuration tests
ran on Linux/amd64. No Android device was connected, so device/VPN operation and
runtime behavior on Android 6 or 16 KiB page devices remain untested. The other
three Android ABIs have not been rebuilt in this verification.

## Native libraries and ABIs

Build the native library from the same patched checkout as the Go bindings. A
generic Cronet library does not expose this fork's custom TCP dialer and Strict
ECH/REALITY APIs, and does not contain its explicit Host-to-authority fix.

| gomobile target | Android ABI | Generated static library |
| --- | --- | --- |
| android/arm64 | arm64-v8a | lib/android_arm64/libcronet.a |
| android/arm | armeabi-v7a | lib/android_arm/libcronet.a |
| android/amd64 | x86_64 | lib/android_amd64/libcronet.a |
| android/386 | x86 | lib/android_386/libcronet.a |

From the cronet-go directory, the build tool supports all four targets:

```sh
go run ./cmd/build-naive --target=android/arm64,android/arm,android/amd64,android/386 download-toolchain
go run ./cmd/build-naive --target=android/arm64,android/arm,android/amd64,android/386 build --jobs=4
go run ./cmd/build-naive --target=android/arm64,android/arm,android/amd64,android/386 package --local
```

`package --local` generates the C headers and per-ABI CGO linker files, which
select the appropriate static archive automatically during `gomobile bind`.
Packaging recreates `lib/` and `include/`; include any already-built host targets
in that packaging invocation if their artifacts must be retained. For an arm64
only APK, build/package just arm64 and select `GOMOBILE_TARGET=android/arm64` below.

## Go module and gomobile configuration

`SekaiMod/libcore/go.mod` selects the local sing-box and cronet-go checkouts with
`replace` directives. Use module mode for gomobile: its generated binding module
lives outside the source tree, so it fails with an explicit `GOWORK` file. The
local replacements are copied into that generated module.
The native archive uses Chromium's relative C++ vtable ABI; NDK 26's linker is
too old for its arm64 relocations. The Browser build script uses the `ld.lld`
from the same Chromium toolchain (override its path with `CRONET_LD`) and passes
`-Wl,-z,max-page-size=16384` for 16 KiB page alignment. The NDK still supplies
gomobile's Android C compiler and sysroot.

In `SekaiMod/libcore`, after configuring `ANDROID_HOME` and the NDK, run:

```sh
WITH_CRONET=1 GOMOBILE_TARGET=android/arm64 bash build.sh
```

This enables `with_cronet`, sets `GOWORK=off`, and copies `libcore.aar` to
`app/libs`. Omitting `GOMOBILE_TARGET` builds all four ABIs and requires all four
native archives. Omitting `WITH_CRONET=1` builds the usual core. Do not add
`with_purego` to this static-linking build. The application's other QUIC/uTLS
features keep their build tags; Browser uses Cronet TLS and disables QUIC with a
custom dialer.

For that arm64-only AAR, build the matching APK from the SekaiMod directory:

```sh
./gradlew :app:assembleOssDebug -PtargetAbi=arm64-v8a
```

`targetAbi` also accepts a comma-separated list; omit it after building a
four-ABI AAR to produce the usual ABI splits.

The native GN configuration, SekaiMod's gomobile command and Gradle `minSdk` all
use API 23 (Android 6.0). Android 5.x is no longer supported by these SekaiMod
builds. `compileSdk` and `targetSdk` remain 35.

## Verified artifacts

The arm64 build used Go 1.26.0, NDK 26.1.10909125 and the matching Chromium LLVM
23 toolchain. The resulting files in the sibling checkouts are:

| Artifact | Location |
| --- | --- |
| Native archive | `cronet-go/lib/android_arm64/libcronet.a` |
| Go JNI AAR | `SekaiMod/libcore/libcore.aar`, copied to `SekaiMod/app/libs/libcore.aar` |
| Debug APK | `SekaiMod/app/build/outputs/apk/oss/debug/NekoBox-1.4.2-arm64-v8a-debug.apk` |

The AAR declares minimum API 23 and contains a single arm64 `libgojni.so`.
Its defined symbols include `Cronet_Engine_SetDialer`, `Cronet_Engine_SetReality`
and `Cronet_Engine_SetStrictECH`. Its only shared dependencies are Android's
`liblog`, `libandroid`, `libdl`, `libm` and `libc`; no separate Cronet shared
library is needed. All 337 strong imports are present in the NDK's API-23 stubs,
and all four ELF load segments have 16 KiB alignment.

The APK contains exactly the same JNI library as the AAR. Its manifest reports
minimum API 23, target API 35 and application ID `moe.sekai.mod.debug`. APK
signature verification and `zipalign -c -P 16 4` passed. These are build and
artifact checks, not a substitute for device execution.

`SekaiMod/libcore/browser_config_test.go` exercises the profile editor's
`CheckSingBoxConfig` entry point with both XHTTP modes, creates and closes the
native REALITY engine, and rejects an invalid short ID. It passed with CGO and
PureGo plus the race detector on Linux. The repackaged Linux native library also
passed the Strict ECH reuse and REALITY camouflage HTTP/2 smoke tests.

## Transport configuration and VPN routing

In SekaiMod, open a VLESS or EWP profile, choose `XHTTP`, and enable `Browser
Dialer`. The editor exposes only `packet-up` and `stream-up` while this switch
is enabled. It disables Xmux, uTLS, insecure verification, custom ALPN and TLS
fragmentation controls; XHTTP padding, SNI, certificates for ordinary TLS,
REALITY and ECH remain available where Browser TLS supports them. The setting
is stored in the profile and is included in VLESS/EWP share links as
`browser=1`.

The sing-box configuration enables the implementation:

```json
{
  "type": "xhttp",
  "browser": true,
  "mode": "packet-up",
  "host": "front.example.com",
  "path": "/xhttp"
}
```

Use this as the outbound's `transport` object; TLS and ECH use the outbound's
`tls` object as described in sing-box's `transport/v2rayxhttp/README.browser.md`.
SekaiMod produces this object from the profile setting. Custom outbound JSON is
filtered for Browser-incompatible TLS and Xmux fields before it is merged, so
old custom overrides cannot turn those options back on. Direct sing-box users
can enable Browser with the same JSON:

```json
{
  "transport": { "browser": true, "mode": "packet-up" }
}
```

Choose `stream-up` instead when required by the server. Use TLS, disable insecure
verification, and leave ALPN empty or use `h2,http/1.1`. Clear explicit xmux tuning;
Cronet manages pooling and multiplexing. The profile's existing XHTTP Host/path,
SNI and REALITY credentials are preserved by this recursive JSON merge.

For REALITY, preserve the outbound's `tls.reality.public_key` and `short_id`
alongside `transport.browser`. The native client uses fixed `ClientVer=1.8.1`;
the native socket enforces TLS 1.3, so there is no separate version setting.
REALITY requires this updated native
build, does not require client uTLS, and cannot be combined with ECH.
This includes the native camouflage request: a validated ordinary website gets
an independent GET on the existing TLS connection after REALITY authentication
fails. The socket remains under the application's original routing and VPN
protection. Device tests should cover ordinary website certificate verification
and engine shutdown during this background request.

The sing-box dialer opens the actual TCP endpoint and applies the Android VPN
socket protection, routing and DNS choices. cronet-go duplicates a TCP descriptor
or relays a wrapped connection through a socketpair. Cronet performs TLS/ECH and
HTTP/2 over that connection. ECH's synthetic DNS queries remain inside socketpairs
in the process. Device validation should cover VPN protection, default trusted
certificates, network changes, and the ABIs/Android versions being distributed.
The native trust store checks `/apex/com.android.conscrypt/cacerts` first on
Android 14 and later, with `/system/etc/security/cacerts` as a fallback for older
devices. Go's external `ca.pem` override is separate from Cronet's trust store.

## If dynamic loading is chosen

PureGo requires a separate custom `libcronet.so` for each supported ABI, packaged
under `app/src/main/jniLibs/<ABI>/` or the equivalent AAR `jni/<ABI>/` directories.
Initialize `cronet.LoadLibrary` with the actual native library path before the
first Cronet call; the current automatic search does not discover Android's app
native-library directory. SekaiMod's `useLegacyPackaging=true` can provide an
extracted path through `applicationInfo.nativeLibraryDir`.

This is additional integration work: the current native build/package commands
produce Android static archives and only package shared libraries for Linux and
Windows. Android PureGo callback support also needs checking for every desired
architecture, especially 32-bit ABIs. The existing gomobile/CGO route is therefore
the recommended integration for SekaiMod.
