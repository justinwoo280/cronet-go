# Browser TLS verification

Verified locally on Linux/amd64 on 2026-09-19 using this checkout's native library
and Debian Chromium. These results describe the tested builds and fresh TCP/TLS
connections; browser releases and field trials can change the ClientHello.

| Component | Version |
| --- | --- |
| Cronet / embedded Chromium | 150.0.7871.63 |
| Installed Chromium | 152.0.7977.82 |

## ECH

This Cronet build supports real ECH. `net/socket/ssl_client_socket_impl.cc` calls
BoringSSL's `SSL_set1_ech_config_list`; DNS HTTPS endpoint metadata supplies that
list. ECH GREASE is also enabled by default, even without a real configuration.
The presence of extension `0xfe0d` alone therefore does not prove ECH acceptance.

Browser XHTTP now accepts a static `ECHConfigList` or a `GetECHConfigList`
callback. The sing-box adapter exports static PEM configurations or obtains
HTTPS records using its DNS router and `tls.ech.query_server_name`, with TTL
caching. Records are delivered to Cronet over internal UDP/TCP socketpairs.
Chromium handles ECH, certificate verification, HTTP/2 and connection reuse; the
application's TCP dialer still resolves and connects the actual endpoint.
When ECH configuration is present, the native socket raises its minimum TLS
version to TLS 1.3. A TLS 1.2-only peer therefore fails before an application
request can be sent, including after an authenticated ECH retry.

Verified with a Go TLS server supporting ECH and the real sing-xhttp server:

- Static and DNS-discovered configurations: TLS 1.3, HTTP/2, `ECHAccepted=true`.
- `packet-up` and `stream-up`: 2 MB echo with independent Host and inner TLS name.
- Stale ECH key: authenticated retry configuration followed by ECH acceptance.
- ECHConfigList larger than 512 bytes: internal UDP DNS truncation followed by
  TCP DNS retry and ECH acceptance.
- Configuration lookup: lazy fetch, TTL caching/refresh, propagated DNS errors,
  and failure when an expired record no longer contains a configuration.
- Both CGO and PureGo bindings pass native integration tests with the race detector.

A separate wire capture confirms outer SNI `public.test`, while the server sees
inner SNI `hello.test` and reports `ECHAccepted=true`.

### Strict ECH backport

Browser now enables Strict ECH whenever an ECH configuration or getter is supplied.
The native policy is fixed before engine startup. Ordinary engines retain their
opportunistic policy. `Engine.SetStrictECH(true)` requires QUIC to be disabled;
changing the policy after engine startup returns an error. PureGo resolves the
new C API optionally and explicitly rejects Strict requests on older libraries.

The backport contains Chromium's empty-list guard and BoringSSL's rejection of
nonempty but unsupported configuration lists. The TLS paths share the check in
`SSLClientSocketImpl::Init()`, including after authenticated ECH rejection. Valid
retry configurations still work. An empty list returns `ERR_STRICT_ECH_REQUIRED`;
an unsupported list returns `ERR_SSL_PROTOCOL_ERROR`, as in the upstream TLS
implementation. A malformed list returns `ERR_INVALID_ECH_CONFIG_LIST`.

Based on these upstream changes, adapted to the local v150 interfaces:

- Chromium [TLS enforcement](https://github.com/chromium/chromium/commit/b3d083a367cba8cc55e43dc24f04a7b011827da1)
  and [reject-unusable integration](https://github.com/chromium/chromium/commit/db9218e74f23457a3d06d6a5d0ddf12918a0c786).
- BoringSSL [reject-unusable API](https://github.com/google/boringssl/commit/59e89e9132799b287fea2b5c95988621a1365130).

Additional Linux/amd64 checks with the patched static/shared libraries:

- Both XHTTP modes: valid ECH and authenticated key rotation succeed. Empty retry
  lists, unusable retry algorithms, unsupported initial version/KEM, malformed
  lists, invalid outer certificates, TLS-1.2-only servers, and a second ECH
  rejection fail without sending application requests or exposing the inner name.
- Recorded TCP input before TLS decryption contains no plaintext `inner.test`.
  Missing and unusable initial configurations emit no ClientHello. Successful or
  rejected real-ECH attempts use the public outer name.
- Sequential requests reuse HTTP/2; after closing connections, TLS session
  resumption still negotiates ECH. Subsequent rejection fails even with a cached
  session ticket.
- A standalone BoringSSL test checks eight combinations of strict/opportunistic,
  TLS 1.2/1.3 client ceilings, and absent/unsupported configuration. Strict cases
  fail before ClientHello; the new error table entry resolves correctly.
- PureGo loading a real older native library rejects Browser ECH initialization
  explicitly while allowing ordinary Browser initialization.
- Both native connection paths were exercised: the default `SSLConnectJob`, and
  `TlsStreamAttempt` via a temporary build enabling `kHappyEyeballsV3`. NetLog
  confirms the latter emitted `TLS_STREAM_ATTEMPT_CONNECT`. The shipped local
  artifacts restore the original feature default.
- CGO/PureGo native tests and sing-box's real sing-xhttp interop pass with the race
  detector. Android and QUIC Strict behavior are outside this verification.

Tests live in `browser_ech_strict_integration_test.go`,
`engine_strict_purego_test.go`, and `test/native/strict_ech_test.cc`. To include the
older-library check, set `CRONET_TEST_OLD_LIBRARY` to its absolute shared-library
path. To assert the native connection path in the reuse test, set
`CRONET_TEST_CONNECT_EVENT=SSL_CONNECT_JOB_SSL_CONNECT`, or
`TLS_STREAM_ATTEMPT_CONNECT` for the temporary Happy Eyeballs V3 build.

The BoringSSL test can be run from the repository root on the current Linux build:

```sh
naiveproxy/src/third_party/llvm-build/Release+Asserts/bin/clang++ \
  --target=x86_64-linux-gnu \
  --sysroot=naiveproxy/src/out/sysroot-build/bullseye/bullseye_amd64_staging \
  -std=c++20 -fuse-ld=lld -I naiveproxy/src/third_party/boringssl/src/include \
  test/native/strict_ech_test.cc lib/linux_amd64/libcronet.a \
  -ldl -lpthread -lrt -lm -lresolv -o /tmp/opencode/cronet-strict-boringssl-test
/tmp/opencode/cronet-strict-boringssl-test
```

## REALITY

The client integrates REALITY into BoringSSL and Chromium. The ClientHello's
32-byte Session ID carries AES-256-GCM authentication using an HKDF-SHA256 key
derived from the handshake's X25519 share and the configured server public key.
`ClientVer` is fixed to `1.8.1`, matching sing-box. The existing standalone X25519
share is preferred for authentication; a hybrid-only hello uses its X25519
component. TLS can still negotiate X25519MLKEM768 in either case.

The peer's Ed25519 certificate public key is authenticated by its REALITY HMAC.
Only after that succeeds does the connection permit an unadvertised Ed25519
CertificateVerify. BoringSSL still verifies its signature and the Finished MAC.
The final serialized ClientHello is modified before entering the transcript;
Chromium's advertised signature list and key shares are preserved.

Verified with Linux/amd64 CGO and PureGo builds:

- The actual sing-box REALITY server (`metacubex/utls v1.8.7`) and sing-xhttp
  transport, using a local TLS camouflage target: `packet-up` and `stream-up`
  each complete 2 MB echo with both X25519 and X25519MLKEM768 server exchanges.
- Full sing-box VLESS outbound/inbound construction and echo in both modes,
  with `browser: true`, `tls.reality`, and no client `tls.utls` setting.
- Wire capture independently decrypts Session ID and checks ClientVer, short ID,
  timestamp and ServerHello echo. The captured ClientHello omits Ed25519, retains
  both Chrome key shares, and includes ECH GREASE.
- Repeated XHTTP sessions reuse authenticated HTTP/2 connections. Closing server
  connections causes a fresh REALITY handshake without TLS resumption.
- Incorrect public key and short ID fail the XHTTP request. The existing REALITY
  server passes the TLS connection through to the camouflage target. If its
  ordinary certificate validates, it receives an independent HTTP/2 `GET /`;
  XHTTP paths, Host overrides, headers and upload data are not forwarded. An
  untrusted target receives no HTTP request.
- Camouflage integration tests verify one TLS handshake per request, HTTP/2,
  HTTP/1.1, no ALPN, and rejection of TLS 1.2-only peers. The original request fails and can be destroyed
  before the website responds, while the independent request drains an 8 MiB
  response, including HTTP/2 flow control. Concurrent application requests
  cannot acquire the camouflage connection. Untrusted, expired and wrong-host
  certificates fail; redirects are drained without following them. Engine
  shutdown, CloseAllConnections and the 30-second timeout close stalled requests.
- Both native connection paths pass: the default SSLConnectJob and a test build
  enabling HappyEyeballsV3/TlsStreamAttempt. NetLog confirms the latter path;
  the packaged libraries retain the normal disabled-by-default feature setting.
- Engine startup rejects QUIC/Strict ECH conflicts; credentials cannot change
  after startup. A real older shared library produces an explicit capability
  error, and ordinary Browser initialization still works.
- Standalone native fixture: valid handshake/application data, bad certificate
  HMAC, bad CertificateVerify, bad Finished, and rejected HRR, each with default
  dual shares and hybrid-only shares. All ten cases pass. Bad CertificateVerify
  and Finished fail even after successful REALITY certificate authentication.
- Native Go integration suites, sing-box transport/TLS/VLESS tests and existing
  Strict ECH regression tests pass with the race detector.

The tested REALITY server omits ALPN. For an authenticated REALITY connection,
Chromium therefore uses h2 with prior knowledge when h2 was offered and no ALPN
was returned. Explicit server ALPN takes precedence. The test checks the real
HTTP/2 connection preface as well as successful data transfer.

REALITY does not combine with real ECH or custom CA roots. TLS resumption and
0-RTT are disabled; HRR is rejected. The native socket requires TLS 1.3 for
both the REALITY handshake and its ordinary camouflage connection. The native
system clock supplies timestamps.
Android runtime behavior and other server implementations/releases have not
been verified here. Android arm64 build and artifact checks are recorded in
[ANDROID_BROWSER.md](ANDROID_BROWSER.md).

On REALITY authentication failure, Chromium performs its ordinary hostname,
certificate chain, validity and policy checks. Certificate exceptions do not
bypass these checks. Only after the complete TLS handshake succeeds is the
socket transferred to a background request owned by the HTTP network session.
The original request receives `ERR_REALITY_AUTHENTICATION_FAILED` (-189), which
is not marked retryable. Certificate or handshake errors do not start camouflage.

The background request uses `https://<verified SNI>/`, the engine's User-Agent,
and a fresh `padding` cookie of 30–61 zeroes, matching sing-box's padding range.
It uses the existing socket without reconnecting. ALPN selects native HTTP/2 or
HTTP/1.1; absent ALPN means HTTP/1.1 for an ordinary website. Each HTTP/2 request
owns a private pool with the engine's normal HTTP/2 settings, so application
transactions cannot reuse or coalesce onto that connection. The body is discarded
until completion or a 30-second timeout, then the connection is closed. Redirects
do not cause further connections. The original request's cancellation does not
cancel camouflage, while closing all engine connections or the engine does.

`engine_reality_fallback_integration_test.go` tests the native request lifecycle;
it installs a local test CA through the lower-level engine API, without weakening
production Browser REALITY validation. Run it with `with_cronet_test`, optionally
`with_purego`; allow at least 60 seconds for the suite including the timeout test.

Go tests are in sing-box's `transport/v2rayxhttp/browser_reality_test.go` and
`protocol/vless/browser_reality_test.go`. Run them with
`with_cronet with_cronet_test with_utls`, adding `with_purego` for dynamic loading.
`with_utls` supplies the test server, not the Browser client. See
[native test instructions](test/native/README.md) for the cryptographic fixture.

## ClientHello comparison

The probe used the same `hello.test` DNS hostname and a local HTTPS server for
both clients. Each run used a fresh Cronet engine or Chromium profile; there were
three runs per variant. Chromium ran headless with system proxies disabled and
a host resolver rule directing the test hostname to loopback. A certificate SPKI
exception trusted only the generated local certificate. Cronet trusted that
certificate through its existing PEM option.

The receiver recorded raw TLS bytes before decryption. Comparison removes random
values (client random, session ID contents, key shares and ECH ciphertext), GREASE
values/payloads and extension ordering, retaining algorithm order and key lengths.
ECH GREASE padding lengths and trust-anchor ordering are also normalized.

| Field | Cronet 150 vs Chromium 152 |
| --- | --- |
| TLS versions | Same: 1.3 and 1.2 |
| Cipher suites and preference order | Same: 15 non-GREASE suites |
| Supported groups | Same: X25519MLKEM768, X25519, P-256, P-384 |
| Key shares | Same: X25519MLKEM768 (1216 bytes), X25519 (32 bytes) |
| Signature algorithms | Same: ML-DSA-44/65/87, ECDSA, RSA-PSS and RSA-PKCS1 |
| ALPN | Same: `h2`, `http/1.1` |
| ALPS | Same: new codepoint `0x44cd`, protocol `h2` |
| Certificate compression | Same: Brotli |
| OCSP, SCT, session ticket and remaining stable extensions | Same |
| GREASE and shuffled extension order | Present in both |
| ECH GREASE | Present in both; no real ECH accepted in baseline runs |
| Trust Anchor IDs (`0xca34`) | Present in Chromium 152; absent in this Cronet 150 build |

The installed Chromium advertises 32 trust-anchor IDs in the captured baseline.
With `--disable-features=TLSTrustAnchorIDs`, all three normalized Chromium profiles
match Cronet. This control identifies the observed configuration difference;
default installed Chromium and Cronet are not byte-identical fingerprints.

The source confirms Chromium's own ClientHello configuration is used:

- `naiveproxy/src/net/socket/custom_client_socket_factory.cc` delegates TLS socket
  creation to Chromium's default socket factory.
- `naiveproxy/src/net/socket/ssl_client_socket_impl.cc` sets ciphers, signatures,
  ALPN/ALPS, ECH and extension permutation using BoringSSL.
- `naiveproxy/src/net/ssl/ssl_config_service.cc` provides Chromium's supported
  groups and requires configured trust-anchor IDs before advertising them.
- `naiveproxy/src/net/base/features.cc` disables `kTLSTrustAnchorIDs` by default
  in this source revision.

This comparison covers fresh TCP ClientHello messages. It does not establish
identity for HTTP headers, HTTP/2 settings, QUIC, session resumption or every
Chromium experiment.

Local diagnostic artifacts from this session:

- `/tmp/opencode/cronet_clienthello_probe.go`: capture driver.
- `/tmp/opencode/cronet_clienthello_compare.py`: decoder and normalization.
- `/tmp/opencode/cronet-clienthello-3592642712/`: raw `.tls` captures, negotiated
  server states, browser logs and `comparison.json`.

To repeat in this workspace:

```sh
# Run from ../sing-box:
GOWORK="$PWD/go.work.browser.example" \
LD_LIBRARY_PATH="$PWD/../cronet-go/lib/linux_amd64" \
go run -tags with_purego /tmp/opencode/cronet_clienthello_probe.go
# Pass the newly printed artifact directory:
python3 /tmp/opencode/cronet_clienthello_compare.py /tmp/opencode/cronet-clienthello-3592642712
```
