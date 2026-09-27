//go:build with_purego && with_cronet_test

package cronet

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrowserECHFailureReleasesCallbacks(t *testing.T) {
	for _, mode := range []string{BrowserXHTTPModePacketUp, BrowserXHTTPModeStreamUp} {
		t.Run(mode, func(t *testing.T) {
			testBrowserECHFailureReleasesCallbacks(t, mode)
		})
	}
}

func testBrowserECHFailureReleasesCallbacks(t *testing.T, mode string) {
	t.Helper()
	_, list := strictECHKey(t)
	serverKey, _ := strictECHKey(t)
	serverKey.SendAsRetry = false
	cert, roots := strictECHCertificate(t, "inner.test", "public.test")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached server after ECH rejection")
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, EncryptedClientHelloKeys: []tls.EncryptedClientHelloKey{serverKey}}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	before := snapshotBrowserCallbacks()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := NewBrowserXHTTPClient(ctx, "https://inner.test", BrowserXHTTPOptions{
		Mode: mode, ECHConfigList: list, TrustedRootCertificates: roots,
		DialContext: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for i := 0; i < 64; i++ {
		conn, err := client.DialContext(ctx)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		// Exercise upload cleanup too; the handshake may already have failed.
		_, _ = conn.Write([]byte("ECH failure lifecycle"))
		_, err = io.ReadAll(conn)
		if err == nil || !strings.Contains(err.Error(), "STRICT_ECH_REQUIRED") {
			t.Errorf("request %d: expected strict ECH failure, got %v", i, err)
		}
		deadline := time.Now().Add(time.Second)
		for {
			client.mu.Lock()
			active := len(client.conns)
			client.mu.Unlock()
			if active == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("request %d: %d connections retained after native failure", i, active)
			}
			time.Sleep(time.Millisecond)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if after := snapshotBrowserCallbacks(); after != before {
		t.Errorf("callbacks retained after ECH failures: before %+v, after %+v", before, after)
	}
}

type browserCallbackCounts struct {
	requests, uploads, runnables, executors, tcpDialers, udpDialers, socketClosers int
}

func snapshotBrowserCallbacks() (counts browserCallbackCounts) {
	urlRequestCallbackAccess.RLock()
	counts.requests = len(urlRequestCallbackMap)
	urlRequestCallbackAccess.RUnlock()
	uploadDataAccess.RLock()
	counts.uploads = len(uploadDataProviderMap)
	uploadDataAccess.RUnlock()
	runnableAccess.RLock()
	counts.runnables = len(runnableMap)
	runnableAccess.RUnlock()
	executorAccess.RLock()
	counts.executors = len(executors)
	executorAccess.RUnlock()
	dialerAccess.RLock()
	counts.tcpDialers = len(dialerMap)
	dialerAccess.RUnlock()
	udpDialerAccess.RLock()
	counts.udpDialers = len(udpDialerMap)
	udpDialerAccess.RUnlock()
	socketCloseCallbacks.Range(func(_, _ any) bool {
		counts.socketClosers++
		return true
	})
	return
}
