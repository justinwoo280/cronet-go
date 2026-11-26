# Selective upstream synchronization — 2026-11-26

The maintained source is `justinwoo280/cronet-go` with its
`naiveproxy-REALITY/naiveproxy` submodule, based on Chromium 154.0.8037.49.
SagerNet/nekohasekai commits are reviewed as individual fixes for that fork.

## Selected changes

| Upstream wrapper commit | Integration |
| --- | --- |
| `90fed339` | Zero-initialize CGO bidirectional header arrays before pointer writes. |
| `fa319c4f`, `ebdb666f` | Delegate proxy address resolution to the configured dialer; supply routable DNS placeholders on IPv6-only hosts. |
| `c9c1d9dc`, `e265295f` | Run native TCP/async UDP dial callbacks off the network thread, limit outstanding workers, drain abandoned sockets, and cancel pending Naive handshakes on reset. |
| `abcdebeb` | Distinguish BSD connected-datagram shutdown from a valid empty datagram. |
| `73349ff2` | Serialize Windows TCP port-reuse metrics across engine threads. |

The native fixes correspond to `be1be963`, `7c64cae2`, `d10b2128` and
`467414d8`. They are applied to our native fork, preserving its network-handle
checks, Browser REALITY/Strict ECH, per-engine ALPN and H3 support, trust-anchor
IDs, Android standalone resolver fix and platform build adaptations. Go-side
reset/cancellation is reconciled with our existing admission and terminal-callback
ownership instead of replacing those lifecycle repairs.

## Already covered

- Chromium 154 and explicit static archive linking are already integrated.
- The Windows iperf3 download already uses a pinned, checksum-verified archive.
- Chrome Root Store v45 is already present. The existing
  `cert_verify_proc_blocklist.inc` is byte-identical to upstream's eMudhra
  blocklist change `728f5e7f` (wrapper `2e12f399`). Our additional SPKI distrust
  check is retained. This synchronization does not replace the root store or
  count that existing blocklist as a new fix.
- Existing Browser and DNS/ECH regressions remain; domain-address fixtures now
  resolve in their custom dialer, matching the revised ownership of DNS.

Native builds and validation run in GitHub Actions. Consumers must use the
matching published wrapper, `all` package and native archives. Go/unit and
SekaiMod consumer validation use the prepared build Box with source hashes.
