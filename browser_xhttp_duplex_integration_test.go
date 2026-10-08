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
	"strings"
	"sync"
	"testing"
	"time"
)

func newBrowserDuplexTestClient(t *testing.T, mode string, framed bool, handler http.HandlerFunc) *BrowserXHTTPClient {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client, err := NewBrowserXHTTPClient(ctx, server.URL, BrowserXHTTPOptions{
		Mode: mode, GRPCFraming: framed, Path: "/xhttp", DisableQUIC: true,
		TrustedRootCertificates: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func dialBrowserDuplexTest(t *testing.T, client *BrowserXHTTPClient) *browserXHTTPConn {
	t.Helper()
	conn, err := client.DialContext(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn.(*browserXHTTPConn)
}

func TestBrowserDuplexGRPCTrailers(t *testing.T) {
	for _, test := range []struct {
		name, status, wantError string
		headerOnly              bool
	}{
		{name: "success", status: "0"},
		{name: "failure after data", status: "13", wantError: "gRPC status 13"},
		{name: "missing status", wantError: "missing gRPC status"},
		{name: "trailers only", status: "16", wantError: "gRPC status 16", headerOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("final response\n"), 6000)
			if test.headerOnly {
				payload = nil
			}
			client := newBrowserDuplexTestClient(t, BrowserXHTTPModeStreamOne, true, func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 || r.URL.Path != "/xhttp/Tun" || r.Header.Get("TE") != "trailers" {
					t.Errorf("RPC request: %s %s %v", r.Proto, r.URL, r.Header)
				}
				w.Header().Set("Content-Type", "application/grpc")
				if !test.headerOnly && test.status != "" {
					w.Header().Set("Trailer", "Grpc-Status")
				}
				if len(payload) > 0 {
					_, _ = w.Write(browserTestGRPCFrame(payload))
				}
				if test.status != "" {
					w.Header().Set("Grpc-Status", test.status)
				}
			})
			conn := dialBrowserDuplexTest(t, client)
			var got []byte
			if len(payload) > 0 {
				first := make([]byte, 1)
				if _, err := io.ReadFull(conn, first); err != nil {
					t.Fatal(err)
				}
				got = append(got, first...)
				// The whole protobuf message has been received but only one byte
				// consumed. Native termination must preserve its buffered tail.
				select {
				case <-conn.reader.doneChannel():
				case <-client.ctx.Done():
					t.Fatal("response did not terminate while upload was open")
				}
			}
			tail, err := io.ReadAll(conn)
			got = append(got, tail...)
			if !bytes.Equal(got, payload) {
				t.Fatalf("response lost bytes: %d/%d", len(got), len(payload))
			}
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("response error = %v; want %s", err, test.wantError)
			}
		})
	}
}

func TestBrowserDuplexResponseErrors(t *testing.T) {
	for _, test := range []struct {
		name, contentType, encoding, wantError string
		status                                 int
		body                                   []byte
	}{
		{name: "HTTP", status: http.StatusForbidden, wantError: "HTTP status 403"},
		{name: "content type", contentType: "text/plain", wantError: "content type"},
		{name: "encoding", encoding: "gzip", wantError: "encoding"},
		{name: "compressed", body: []byte{1, 0, 0, 0, 0}, wantError: "compressed"},
		{name: "truncated", body: []byte{0, 0, 0, 0, 8, 0x0a}, wantError: "unexpected EOF"},
		{name: "malformed", body: browserTestGRPCWire([]byte{0x0a, 1, 'a', 0x0f}), wantError: "protobuf"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newBrowserDuplexTestClient(t, BrowserXHTTPModeStreamOne, true, func(w http.ResponseWriter, r *http.Request) {
				contentType := test.contentType
				if contentType == "" {
					contentType = "application/grpc"
				}
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Grpc-Encoding", test.encoding)
				w.Header().Set("Trailer", "Grpc-Status")
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				_, _ = w.Write(test.body)
				w.Header().Set("Grpc-Status", "0")
			})
			conn := dialBrowserDuplexTest(t, client)
			for i := 0; i < 2; i++ {
				n, err := conn.Read(make([]byte, 1))
				if n != 0 || err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("read %d = %d, %v; want %s", i, n, err, test.wantError)
				}
			}
		})
	}
}

func TestBrowserDuplexReadDeadlineResume(t *testing.T) {
	for _, offset := range []int{2, 8} {
		t.Run(string(rune('0'+offset)), func(t *testing.T) {
			payload := []byte("response survives a deadline in the middle of a frame")
			wire := browserTestGRPCFrame(payload)
			resume := make(chan struct{})
			paused := make(chan struct{})
			client := newBrowserDuplexTestClient(t, BrowserXHTTPModeStreamOne, true, func(w http.ResponseWriter, r *http.Request) {
				// Require initial data before sending response headers.
				if _, err := browserTestReadGRPC(r.Body); err != nil {
					return
				}
				w.Header().Set("Content-Type", "application/grpc")
				w.Header().Set("Trailer", "Grpc-Status")
				_, _ = w.Write(wire[:offset])
				w.(http.Flusher).Flush()
				close(paused)
				select {
				case <-resume:
					_, _ = w.Write(wire[offset:])
					w.Header().Set("Grpc-Status", "0")
				case <-r.Context().Done():
				}
			})
			conn := dialBrowserDuplexTest(t, client)
			if _, err := conn.Write([]byte("request")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-paused:
			case <-client.ctx.Done():
				t.Fatal("server did not receive the initial upload")
			}
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, err := conn.Read(make([]byte, len(payload)))
			var timeout net.Error
			if n != 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatalf("deadline = %d, %v", n, err)
			}
			decoder := conn.reader.(*browserDuplexRequest).decoder
			decoder.mu.Lock()
			consumed := decoder.headerN + decoder.frameN
			decoder.mu.Unlock()
			if consumed != offset {
				t.Fatalf("deadline interrupted at byte %d; want %d", consumed, offset)
			}
			_ = conn.SetReadDeadline(time.Time{})
			close(resume)
			got, err := io.ReadAll(conn)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("response after reset = %q, %v", got, err)
			}
		})
	}
}

func TestBrowserDuplexStreamUpPreservesDownload(t *testing.T) {
	releaseDownload := make(chan struct{})
	payload := bytes.Repeat([]byte("download after upload EOF\n"), 10000)
	client := newBrowserDuplexTestClient(t, BrowserXHTTPModeStreamUp, true, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			select {
			case <-releaseDownload:
				_, _ = w.Write(payload)
			case <-r.Context().Done():
			}
			return
		}
		if _, err := browserTestReadGRPC(r.Body); err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "0")
	})
	conn := dialBrowserDuplexTest(t, client)
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.uploadDone:
	case <-client.ctx.Done():
		t.Fatal("upload EOF not observed")
	}
	close(releaseDownload)
	got, err := io.ReadAll(conn)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("download after upload EOF: %d/%d bytes, %v", len(got), len(payload), err)
	}
}

func TestBrowserDuplexConcurrentCloseReleasesCallbacks(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		framed     bool
	}{
		{"raw", BrowserXHTTPModeStreamOne, false},
		{"framed", BrowserXHTTPModeStreamOne, true},
		{"stream-up", BrowserXHTTPModeStreamUp, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := countCallbackRegistries()
			started := make(chan struct{})
			var startOnce sync.Once
			client := newBrowserDuplexTestClient(t, test.mode, test.framed, func(w http.ResponseWriter, r *http.Request) {
				startOnce.Do(func() { close(started) })
				<-r.Context().Done()
			})
			conn := dialBrowserDuplexTest(t, client)
			readDone, writeDone := make(chan error, 1), make(chan error, 1)
			go func() { _, err := conn.Read(make([]byte, 1)); readDone <- err }()
			go func() { _, err := conn.Write(make([]byte, 8<<20)); writeDone <- err }()
			select {
			case <-started:
			case <-client.ctx.Done():
				t.Fatal("native request did not start")
			}
			var closed sync.WaitGroup
			for i := 0; i < 4; i++ {
				closed.Add(1)
				go func() { defer closed.Done(); _ = conn.Close(); _ = client.Close() }()
			}
			closed.Wait()
			if err := <-readDone; err == nil {
				t.Error("read succeeded after cancellation")
			}
			if err := <-writeDone; err == nil {
				t.Error("blocked write succeeded after cancellation")
			}
			if after := countCallbackRegistries(); after != before {
				t.Fatalf("retained callbacks: before %+v, after %+v", before, after)
			}
		})
	}
}

func TestBrowserDuplexUnstartedAndInvalidRequest(t *testing.T) {
	before := countCallbackRegistries()
	client := newBrowserDuplexTestClient(t, BrowserXHTTPModeStreamOne, false, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	for _, method := range []string{"", "bad method"} {
		request := newBrowserDuplexRequest(client.engine, method, client.baseURL.String(), nil, io.NopCloser(strings.NewReader("")), false)
		if method != "" {
			if err := request.start(); err == nil {
				if err := request.wait(client.ctx); err == nil {
					t.Error("invalid method accepted")
				}
			}
		}
		_ = request.close()
	}
	_ = client.Close()
	if after := countCallbackRegistries(); after != before {
		t.Fatalf("retained callbacks: before %+v, after %+v", before, after)
	}
}
