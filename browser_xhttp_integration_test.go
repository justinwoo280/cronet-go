//go:build with_cronet_test

package cronet

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests run against the native library (CGO or PureGo). The server delays
// GET headers until upload data arrives, like a buffering reverse proxy.
func TestBrowserXHTTPNativeRoundTrip(t *testing.T) {
	for _, mode := range []string{BrowserXHTTPModePacketUp, BrowserXHTTPModeStreamUp} {
		t.Run(mode, func(t *testing.T) {
			chunks := make(chan []byte, 32)
			var sequenceMu sync.Mutex
			nextSequence := 0
			packets := make(map[int][]byte)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					t.Errorf("protocol = %s", r.Proto)
				}
				if r.Method == http.MethodGet {
					if r.Header.Get("Content-Type") != "" {
						t.Error("GET contains Content-Type")
					}
					for {
						select {
						case data := <-chunks:
							if _, err := w.Write(data); err != nil {
								return
							}
							w.(http.Flusher).Flush()
						case <-r.Context().Done():
							return
						}
					}
				}
				if mode == BrowserXHTTPModePacketUp {
					data, err := io.ReadAll(r.Body)
					if err != nil {
						return
					}
					seq, err := strconv.Atoi(path.Base(r.URL.Path))
					if err != nil {
						t.Error(err)
						return
					}
					sequenceMu.Lock()
					packets[seq] = data
					for {
						data, ok := packets[nextSequence]
						if !ok {
							break
						}
						delete(packets, nextSequence)
						nextSequence++
						chunks <- data
					}
					sequenceMu.Unlock()
					_, _ = w.Write(bytes.Repeat([]byte("a"), streamingReadBufferSize*3))
					return
				}
				buf := make([]byte, 8192)
				for {
					n, err := r.Body.Read(buf)
					if n > 0 {
						select {
						case chunks <- bytes.Clone(buf[:n]):
						case <-r.Context().Done():
							return
						}
					}
					if err != nil {
						break
					}
				}
				// A nonempty multi-buffer upload response must be drained.
				_, _ = w.Write(bytes.Repeat([]byte("a"), streamingReadBufferSize*3))
			}))
			server.EnableHTTP2 = true
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
			der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
			server.StartTLS()
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client, err := NewBrowserXHTTPClient(ctx, server.URL, BrowserXHTTPOptions{
				Mode: mode, Path: "/xhttp", DisableQUIC: true,
				TrustedRootCertificates: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})),
				ScMaxEachPostBytes:      BrowserXHTTPRange{65536, 65536},
				ScMinPostsIntervalMs:    BrowserXHTTPRange{1, 1},
				DialContext: func(ctx context.Context) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
					if err == nil && mode == BrowserXHTTPModeStreamUp {
						conn = &browserWrappedConn{conn}
					}
					return conn, err
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			conn, err := client.DialContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			payload := bytes.Repeat([]byte("browser xhttp native echo\n"), 12000)
			writeErr := make(chan error, 1)
			go func() { _, err := conn.Write(payload); writeErr <- err }()
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("echo mismatch")
			}
			if err := <-writeErr; err != nil {
				t.Fatal(err)
			}
			readErr := make(chan error, 1)
			go func() { _, err := conn.Read(make([]byte, 1)); readErr <- err }()
			_ = conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			select {
			case err := <-readErr:
				if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
					t.Fatalf("deadline: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("deadline did not interrupt read")
			}
			_ = conn.SetReadDeadline(time.Time{})
			// Simultaneous connection and client closes must wait for native work.
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); _ = conn.Close(); _ = client.Close() }()
			}
			wg.Wait()
		})
	}
}

type browserWrappedConn struct{ net.Conn }

func TestBrowserECHLookupFailure(t *testing.T) {
	var dials atomic.Int32
	lookupError := errors.New("ECH lookup failed")
	client, err := NewBrowserXHTTPClient(context.Background(), "https://inner.test", BrowserXHTTPOptions{
		GetECHConfigList: func(context.Context) ([]byte, error) { return nil, lookupError },
		DialContext: func(context.Context) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("unexpected dial")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.DialContext(context.Background()); !errors.Is(err, lookupError) {
		t.Fatalf("error = %v", err)
	}
	if dials.Load() != 0 {
		t.Fatal("connection attempted after ECH lookup failure")
	}
}

func TestBrowserXHTTPNativeFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "redirect") {
			http.Redirect(w, r, "/other", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	for _, path := range []string{"/forbidden", "/redirect"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := NewBrowserXHTTPClient(ctx, server.URL, BrowserXHTTPOptions{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			conn, err := client.DialContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_, err = conn.Read(make([]byte, 1))
			if err == nil || !strings.Contains(err.Error(), "HTTP") {
				t.Fatalf("response error = %v", err)
			}
		})
	}
}

func TestBrowserXHTTPNativeCancelAndInvalidRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			_, _ = io.Copy(io.Discard, r.Body)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := NewBrowserXHTTPClient(context.Background(), server.URL, BrowserXHTTPOptions{Mode: BrowserXHTTPModeStreamUp})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		conn, err := client.DialContext(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Init can fail after Cronet has adopted an upload provider. Cleanup must
	// release that provider exactly once without waiting for a terminal callback.
	_, err = newStreamingRequest(client.engine, client.executor, "bad method", server.URL, nil, &fixedUploadProvider{data: []byte("test")}, nil)
	if err == nil {
		t.Fatal("invalid method accepted")
	}
	_, err = newStreamingRequest(client.engine, client.executor, "POST", server.URL, http.Header{"bad header": {"value"}}, &fixedUploadProvider{data: []byte("test")}, nil)
	if err == nil {
		t.Fatal("invalid header accepted")
	}
}
