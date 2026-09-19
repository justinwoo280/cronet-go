// REALITY client regression against an in-memory TLS fixture. The production
// interop test uses sing-box's existing server; this fixture injects failures
// into authenticated certificates, CertificateVerify and Finished separately.
#include "ssl/internal.h"
#include <openssl/aead.h>
#include <openssl/bio.h>
#include <openssl/curve25519.h>
#include <openssl/evp.h>
#include <openssl/hkdf.h>
#include <openssl/hmac.h>
#include <openssl/x509.h>

#undef NDEBUG
#include <cassert>
#include <cstdio>
#include <cstring>
#include <vector>

enum class Fault { kNone, kHmac, kSignature, kFinished, kHrr };
struct State {
  Fault fault;
  uint8_t signing_private[64];
  int authenticated = 0;
};

static ssl_verify_result_t Verify(SSL* ssl, uint8_t*) {
  auto* state = static_cast<State*>(SSL_get_app_data(ssl));
  if (!SSL_verify_reality_peer_certificate(ssl)) return ssl_verify_invalid;
  state->authenticated++;
  return ssl_verify_ok;
}

static ssl_private_key_result_t Sign(SSL* ssl, uint8_t* out, size_t* out_len,
                                    size_t capacity, uint16_t algorithm,
                                    const uint8_t* in, size_t in_len) {
  auto* state = static_cast<State*>(SSL_get_app_data(ssl));
  assert(capacity >= 64 && algorithm == SSL_SIGN_ED25519);
  assert(ED25519_sign(out, in, in_len, state->signing_private));
  *out_len = 64;
  if (state->fault == Fault::kSignature) out[0] ^= 1;
  return ssl_private_key_success;
}

static void Message(int write, int, int type, const void* buf, size_t len,
                    SSL* ssl, void*) {
  auto* state = static_cast<State*>(SSL_get_app_data(ssl));
  if (state->fault == Fault::kFinished && write && type == SSL3_RT_HANDSHAKE &&
      len && static_cast<const uint8_t*>(buf)[0] == SSL3_MT_CERTIFICATE_VERIFY) {
    // CertificateVerify is already signed. Alter only the server's transcript
    // before it calculates Finished, retaining valid record-layer encryption.
    const uint8_t extra[] = {0};
    assert(ssl->s3->hs->transcript.Update(extra));
  }
}

static int SelectCertificate(SSL* ssl, void*) {
  // The REALITY fixture deliberately signs Ed25519 despite its omission from
  // the client's advertisement. This does not change any transcript bytes.
  const uint16_t sig = SSL_SIGN_ED25519;
  return ssl->s3->hs->peer_sigalgs.CopyFrom(bssl::Span(&sig, 1));
}

static void Transfer(BIO* from, BIO* to) {
  uint8_t buf[4096];
  while (BIO_ctrl_pending(from)) {
    int n = BIO_read(from, buf, sizeof(buf));
    assert(n > 0 && BIO_write(to, buf, n) == n);
  }
}

static void Run(Fault fault, bool hybrid_only) {
  State state{fault};
  bssl::UniquePtr<SSL_CTX> ctx(SSL_CTX_new(TLS_method()));
  bssl::UniquePtr<SSL> client(SSL_new(ctx.get())), server(SSL_new(ctx.get()));
  SSL_set_connect_state(client.get());
  SSL_set_accept_state(server.get());
  SSL_set_app_data(client.get(), &state);
  SSL_set_app_data(server.get(), &state);
  SSL_set_custom_verify(client.get(), SSL_VERIFY_PEER, Verify);
  SSL_set_cert_cb(server.get(), SelectCertificate, nullptr);
  SSL_set_msg_callback(server.get(), Message);
  assert(SSL_set_min_proto_version(server.get(), TLS1_3_VERSION));
  const uint16_t groups[] = {SSL_GROUP_X25519_MLKEM768, SSL_GROUP_X25519,
                             SSL_GROUP_SECP256R1};
  assert(SSL_set1_group_ids(client.get(), groups, 3));
  assert(SSL_set1_client_key_shares(client.get(), groups, hybrid_only ? 1 : 2));
  const uint16_t server_group = fault == Fault::kHrr ? SSL_GROUP_SECP256R1
                                                    : SSL_GROUP_X25519_MLKEM768;
  assert(SSL_set1_group_ids(server.get(), &server_group, 1));
  const uint16_t chrome_sigs[] = {SSL_SIGN_ECDSA_SECP256R1_SHA256,
                                  SSL_SIGN_RSA_PSS_RSAE_SHA256};
  assert(SSL_set_verify_algorithm_prefs(client.get(), chrome_sigs, 2));
  assert(SSL_set_tlsext_host_name(client.get(), "reality.test"));
  SSL_set_enable_ech_grease(client.get(), 1);
  uint8_t public_key[32], private_key[32];
  X25519_keypair(public_key, private_key);
  const uint8_t short_id[] = {1, 2, 3, 4, 5, 6, 7, 8};
  assert(SSL_set1_reality_config(client.get(), public_key, short_id));
  BIO* ci = BIO_new(BIO_s_mem());
  BIO* co = BIO_new(BIO_s_mem());
  BIO* si = BIO_new(BIO_s_mem());
  BIO* so = BIO_new(BIO_s_mem());
  BIO_set_mem_eof_return(ci, -1);
  BIO_set_mem_eof_return(si, -1);
  SSL_set_bio(client.get(), ci, co);
  SSL_set_bio(server.get(), si, so);
  int ret = SSL_do_handshake(client.get());
  assert(SSL_get_error(client.get(), ret) == SSL_ERROR_WANT_READ);

  // Authenticate the actual emitted ClientHello, using the server private key.
  char* raw;
  long len = BIO_get_mem_data(co, &raw);
  assert(len > 76);
  const auto* bytes = reinterpret_cast<const uint8_t*>(raw);
  size_t hello_len = (size_t{bytes[6]} << 16) | (size_t{bytes[7]} << 8) | bytes[8];
  assert(hello_len + 9 <= static_cast<size_t>(len));
  std::vector<uint8_t> hello(bytes + 5, bytes + 9 + hello_len);
  uint8_t sealed[32];
  memcpy(sealed, hello.data() + 39, 32);
  memset(hello.data() + 39, 0, 32);
  CBS body, sid, suites, compression, extensions;
  CBS_init(&body, hello.data() + 38, hello.size() - 38);
  assert(CBS_get_u8_length_prefixed(&body, &sid));
  assert(CBS_get_u16_length_prefixed(&body, &suites));
  assert(CBS_get_u8_length_prefixed(&body, &compression));
  assert(CBS_get_u16_length_prefixed(&body, &extensions));
  uint8_t peer[32];
  bool found = false;
  while (CBS_len(&extensions)) {
    uint16_t id;
    CBS value;
    assert(CBS_get_u16(&extensions, &id));
    assert(CBS_get_u16_length_prefixed(&extensions, &value));
    if (id != TLSEXT_TYPE_key_share) continue;
    CBS shares;
    assert(CBS_get_u16_length_prefixed(&value, &shares));
    while (CBS_len(&shares)) {
      uint16_t group;
      CBS key;
      assert(CBS_get_u16(&shares, &group));
      assert(CBS_get_u16_length_prefixed(&shares, &key));
      if ((hybrid_only && group == SSL_GROUP_X25519_MLKEM768) ||
          (!hybrid_only && group == SSL_GROUP_X25519)) {
        assert(CBS_len(&key) >= 32);
        memcpy(peer, CBS_data(&key) + CBS_len(&key) - 32, 32);
        found = true;
      }
    }
  }
  assert(found);
  uint8_t shared[32], auth[32];
  assert(X25519(shared, private_key, peer));
  const uint8_t info[] = {'R', 'E', 'A', 'L', 'I', 'T', 'Y'};
  assert(HKDF(auth, 32, EVP_sha256(), shared, 32, hello.data() + 6, 20,
              info, sizeof(info)));
  bssl::ScopedEVP_AEAD_CTX aead;
  assert(EVP_AEAD_CTX_init(aead.get(), EVP_aead_aes_256_gcm(), auth, 32, 16, nullptr));
  uint8_t plain[16];
  size_t plain_len;
  assert(EVP_AEAD_CTX_open(aead.get(), plain, &plain_len, sizeof(plain),
                          hello.data() + 26, 12, sealed, 32,
                          hello.data(), hello.size()));
  assert(plain_len == 16 && plain[0] == 1 && plain[1] == 8 && plain[2] == 1);
  assert(memcmp(plain + 8, short_id, 8) == 0);

  uint8_t signing_public[32];
  ED25519_keypair(signing_public, state.signing_private);
  bssl::UniquePtr<EVP_PKEY> signing_key(EVP_PKEY_new_raw_private_key(
      EVP_PKEY_ED25519, nullptr, state.signing_private, 32));
  bssl::UniquePtr<X509> cert(X509_new());
  assert(X509_set_version(cert.get(), 2));
  assert(ASN1_INTEGER_set(X509_get_serialNumber(cert.get()), 1));
  assert(X509_gmtime_adj(X509_getm_notBefore(cert.get()), -60));
  assert(X509_gmtime_adj(X509_getm_notAfter(cert.get()), 3600));
  assert(X509_set_pubkey(cert.get(), signing_key.get()));
  assert(X509_sign(cert.get(), signing_key.get(), nullptr));
  uint8_t* der = nullptr;
  int der_len = i2d_X509(cert.get(), &der);
  assert(der_len > 64);
  unsigned hmac_len;
  assert(HMAC(EVP_sha512(), auth, 32, signing_public, 32,
               der + der_len - 64, &hmac_len) && hmac_len == 64);
  if (fault == Fault::kHmac) der[der_len - 1] ^= 1;
  assert(SSL_use_certificate_ASN1(server.get(), der, der_len));
  OPENSSL_free(der);
  static const SSL_PRIVATE_KEY_METHOD method = {Sign, nullptr, nullptr};
  SSL_set_private_key_method(server.get(), &method);

  bool client_done = false, server_done = false, failed = false;
  uint32_t failure_reason = 0;
  for (int i = 0; i < 20 && !failed && !(client_done && server_done); i++) {
    Transfer(co, si);
    ERR_clear_error();
    ret = SSL_do_handshake(server.get());
    if (ret == 1) server_done = true;
    else assert(SSL_get_error(server.get(), ret) == SSL_ERROR_WANT_READ);
    Transfer(so, ci);
    ERR_clear_error();
    ret = SSL_do_handshake(client.get());
    if (ret == 1) client_done = true;
    else if (SSL_get_error(client.get(), ret) != SSL_ERROR_WANT_READ) {
      failed = true;
      failure_reason = ERR_GET_REASON(ERR_peek_last_error());
    }
  }
  if (fault == Fault::kNone) {
    assert(client_done && server_done && !failed && state.authenticated == 1);
    assert(SSL_get_peer_signature_algorithm(client.get()) == SSL_SIGN_ED25519);
    const char payload[] = "authenticated application data";
    assert(SSL_write(client.get(), payload, sizeof(payload)) == sizeof(payload));
    Transfer(co, si);
    char got[sizeof(payload)];
    assert(SSL_read(server.get(), got, sizeof(got)) == sizeof(got));
    assert(memcmp(got, payload, sizeof(got)) == 0);
  } else {
    assert(failed && !client_done);
    assert(state.authenticated == ((fault == Fault::kHmac || fault == Fault::kHrr) ? 0 : 1));
    if (fault == Fault::kSignature) assert(failure_reason == SSL_R_BAD_SIGNATURE);
    if (fault == Fault::kFinished) assert(failure_reason == SSL_R_DIGEST_CHECK_FAILED);
  }
  printf("PASS: REALITY fault=%d hybrid-only=%d authenticated=%d reason=%u\n",
         static_cast<int>(fault), hybrid_only, state.authenticated, failure_reason);
}

int main() {
  setbuf(stdout, nullptr);
  for (bool hybrid_only : {false, true}) {
    for (Fault fault : {Fault::kNone, Fault::kHmac, Fault::kSignature,
                        Fault::kFinished, Fault::kHrr}) {
      Run(fault, hybrid_only);
    }
  }
}
