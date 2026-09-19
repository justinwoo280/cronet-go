//go:build with_cronet_test

package cronet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// An ordinary TLS target models the connection after REALITY has passed the
// original ClientHello through. The sing-box interop fixture also exercises the
// actual authentication failure and server-side forwarding.
func TestRealityCamouflage(t *testing.T) {
	if err := checkLibrary(); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"h2", "http1", "no-alpn", "tls12", "untrusted", "wrong-host", "expired", "redirect", "isolated", "close-all", "shutdown", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			timeout := 10 * time.Second
			if scenario == "timeout" {
				timeout = 40 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			name := "reality.test"
			if scenario == "wrong-host" {
				name = "other.test"
			}
			cert, roots := strictECHCertificate(t, name)
			if scenario == "expired" {
				leaf, err := x509.ParseCertificate(cert.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				leaf.NotBefore = time.Now().Add(-2 * time.Hour)
				leaf.NotAfter = time.Now().Add(-time.Hour)
				cert.Certificate[0], err = x509.CreateCertificate(rand.Reader, leaf, leaf, leaf.PublicKey, cert.PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
			}
			invalidCert := scenario == "untrusted" || scenario == "wrong-host" || scenario == "expired"
			wantH2 := scenario != "http1" && scenario != "no-alpn" && scenario != "tls12"
			requests := make(chan *http.Request, 8)
			drained := make(chan error, 8)
			closed := make(chan struct{}, 8)
			release := make(chan struct{})
			var releaseOnce sync.Once
			allowResponse := func() { releaseOnce.Do(func() { close(release) }) }
			defer allowResponse()
			var handshakes atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || len(body) != 0 || r.Method != "GET" || r.RequestURI != "/" || r.Host != "reality.test" {
					t.Errorf("camouflage leaked request metadata/body: %s %s host=%s body=%q err=%v", r.Method, r.RequestURI, r.Host, body, err)
				}
				cookie, err := r.Cookie("padding")
				if err != nil || len(r.Cookies()) != 1 || len(cookie.Value) < 30 || len(cookie.Value) > 61 || strings.Trim(cookie.Value, "0") != "" {
					t.Errorf("invalid camouflage cookie: %q", r.Header.Get("Cookie"))
				}
				if r.Header.Get("Authorization") != "" || r.Header.Get("X-Private") != "" || r.Header.Get("Content-Type") != "" || r.Header.Get("User-Agent") != "CamouflageTest/1" {
					t.Errorf("camouflage headers: %v", r.Header)
				}
				if (r.ProtoMajor == 2) != wantH2 || r.TLS.ServerName != "reality.test" {
					t.Errorf("camouflage protocol/SNI: %s %s", r.Proto, r.TLS.ServerName)
				}
				requests <- r
				select {
				case <-release:
				case <-r.Context().Done():
					return
				case <-ctx.Done():
					return
				}
				if scenario == "redirect" {
					w.Header().Set("Location", "/should-not-follow")
					w.WriteHeader(http.StatusFound)
				} else {
					// Also exercise informational response handling on HTTP/1.1.
					w.WriteHeader(http.StatusEarlyHints)
				}
				// Exceed Chromium's HTTP/2 stream receive window: completing this
				// write requires the independent request to keep draining the body.
				_, err = io.Copy(w, bytes.NewReader(bytes.Repeat([]byte("c"), 8<<20)))
				drained <- err
			}))
			server.EnableHTTP2 = wantH2
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				handshakes.Add(1)
				return nil, nil
			}}
			if scenario == "no-alpn" {
				server.TLS.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					handshakes.Add(1)
					return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
				}
			}
			if scenario == "tls12" {
				server.TLS.MaxVersion = tls.VersionTLS12
			}
			server.StartTLS()
			defer server.Close()

			engine := NewEngine()
			defer engine.Destroy()
			if err := engine.SetReality([32]byte{9}, [8]byte{}); err != nil {
				t.Fatal(err)
			}
			if scenario == "untrusted" {
				_, roots = strictECHCertificate(t, name)
			}
			if !engine.SetTrustedRootCertificates(roots) {
				t.Fatal("set fixture trust root")
			}
			params := NewEngineParams()
			defer params.Destroy()
			params.SetEnableCheckResult(false)
			params.SetEnableHTTP2(true)
			params.SetEnableQuic(false)
			params.SetUserAgent("CamouflageTest/1")
			if err := params.SetHostResolverRules("MAP reality.test 127.0.0.1"); err != nil {
				t.Fatal(err)
			}
			if got := engine.StartWithParams(params); got != ResultSuccess {
				t.Fatal(got)
			}
			stopped := false
			defer func() {
				if !stopped {
					engine.Shutdown()
				}
			}()
			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			client, err := NewBrowserXHTTPClient(ctx, "https://reality.test:"+port, BrowserXHTTPOptions{Engine: engine})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			r, err := newStreamingRequest(engine, client.executor, "POST", client.baseURL.String()+"/private/session/9?secret=value", http.Header{
				"Host": {"private-front.test"}, "Authorization": {"secret"}, "Cookie": {"private=secret"}, "X-Private": {"secret"}, "Content-Type": {"application/grpc"},
			}, &fixedUploadProvider{data: bytes.Repeat([]byte("secret upload"), 100)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer r.close()
			started := time.Now()
			if err := r.start(); err != nil {
				t.Fatal(err)
			}
			_, err = r.readContext(ctx, make([]byte, 1))
			var nativeError *ErrorGo
			if !errors.As(err, &nativeError) {
				t.Fatalf("expected a native TLS error: %v", err)
			}
			if invalidCert {
				wantError := NetErrorCertAuthorityInvalid
				if scenario == "wrong-host" {
					wantError = NetErrorCertCommonNameInvalid
				} else if scenario == "expired" {
					wantError = NetErrorCertDateInvalid
				}
				if nativeError.InternalErrorCode != wantError.Code() {
					t.Fatalf("invalid ordinary certificate: %v", err)
				}
				_ = r.close()
				engine.CloseAllConnections()
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if len(requests) != 0 {
					t.Fatal("sent camouflage with an invalid ordinary certificate")
				}
				return
			}
			if scenario == "tls12" {
				if nativeError.InternalErrorCode == NetErrorRealityAuthenticationFailed.Code() || nativeError.Retryable {
					t.Fatalf("REALITY downgraded to TLS 1.2: %v", err)
				}
				_ = r.close()
				engine.CloseAllConnections()
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if len(requests) != 0 {
					t.Fatal("sent camouflage after rejecting a TLS 1.2-only peer")
				}
				return
			}
			if nativeError.InternalErrorCode != NetErrorRealityAuthenticationFailed.Code() || nativeError.Retryable {
				t.Fatalf("original request error = %v", err)
			}
			// This closes the original URLRequest before the camouflage response
			// is sent, proving the background request does not share its lifetime.
			_ = r.close()
			select {
			case <-requests:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			wantConnections := int32(1)
			if scenario == "isolated" {
				// While the first camouflage stream is still active, the same
				// engine must not pool a second application request onto it.
				second, err := newStreamingRequest(engine, client.executor, "GET", client.baseURL.String()+"/another-private-session", nil, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer second.close()
				if err := second.start(); err != nil {
					t.Fatal(err)
				}
				_, err = second.readContext(ctx, make([]byte, 1))
				if !errors.As(err, &nativeError) || nativeError.InternalErrorCode != NetErrorRealityAuthenticationFailed.Code() {
					t.Fatalf("second request used camouflage connection: %v", err)
				}
				_ = second.close()
				select {
				case <-requests:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				wantConnections = 2
			}
			if scenario == "close-all" {
				engine.CloseAllConnections()
			} else if scenario == "shutdown" {
				_ = client.Close()
				if got := engine.Shutdown(); got != ResultSuccess {
					t.Fatal(got)
				}
				stopped = true
			} else if scenario != "timeout" {
				allowResponse()
				for range wantConnections {
					select {
					case err := <-drained:
						if err != nil {
							t.Fatal("camouflage response was not drained:", err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			}
			for range wantConnections {
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal("camouflage socket was not closed:", ctx.Err())
				}
			}
			if scenario == "timeout" && time.Since(started) < 25*time.Second {
				t.Fatal("stalled camouflage closed before its timeout")
			}
			if handshakes.Load() != wantConnections || len(requests) != 0 {
				t.Fatalf("camouflage reconnected, retried, or followed a redirect: handshakes=%d extra requests=%d", handshakes.Load(), len(requests))
			}
		})
	}
}
