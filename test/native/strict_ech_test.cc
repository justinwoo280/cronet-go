// Exercise the backported BoringSSL guard without a server. The client must fail
// before emitting a ClientHello when Strict ECH has no usable configuration,
// including when the client's maximum TLS version is below 1.3.
#include <openssl/bio.h>
#include <openssl/err.h>
#include <openssl/ssl.h>

#include <assert.h>
#include <stdio.h>
#include <string.h>

int main() {
  // Syntactically valid ECHConfigList containing an unknown version.
  const uint8_t unsupported[] = {0, 4, 0xfe, 0x0c, 0, 0};
  const bool choices[] = {false, true};
  for (bool strict : choices) {
    for (bool tls12 : choices) {
      for (bool with_config : choices) {
        SSL_CTX* ctx = SSL_CTX_new(TLS_method());
        assert(ctx);
        SSL* ssl = SSL_new(ctx);
        assert(ssl);
        SSL_set_connect_state(ssl);
        assert(SSL_set_tlsext_host_name(ssl, "inner.test"));
        SSL_set_enable_ech_grease(ssl, 1);
        SSL_set_reject_unusable_ech_config(ssl, strict);
        if (tls12) {
          assert(SSL_set_max_proto_version(ssl, TLS1_2_VERSION));
        }
        if (with_config) {
          assert(SSL_set1_ech_config_list(ssl, unsupported, sizeof(unsupported)));
        }
        BIO* input = BIO_new(BIO_s_mem());
        BIO* output = BIO_new(BIO_s_mem());
        assert(input && output);
        BIO_set_mem_eof_return(input, -1);
        SSL_set_bio(ssl, input, output);
        ERR_clear_error();
        int result = SSL_do_handshake(ssl);
        int error = SSL_get_error(ssl, result);
        if (strict) {
          assert(error == SSL_ERROR_SSL);
          uint32_t reason = ERR_peek_error();
          assert(ERR_GET_REASON(reason) == SSL_R_UNUSABLE_ECH_CONFIG_LIST);
          assert(strcmp(ERR_reason_error_string(reason),
                        "UNUSABLE_ECH_CONFIG_LIST") == 0);
          char* data = nullptr;
          long length = BIO_get_mem_data(output, &data);
          // A fatal alert is permitted; a ClientHello is not.
          assert(length == 0 || static_cast<unsigned char>(data[0]) == 21);
        } else {
          assert(error == SSL_ERROR_WANT_READ);
          assert(BIO_ctrl_pending(output) > 0);
        }
        SSL_free(ssl);
        SSL_CTX_free(ctx);
      }
    }
  }
  puts("Strict ECH: all 8 pre-ClientHello checks passed");
}
