//go:build with_cronet_test

package cronet

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func strictECHKey(t *testing.T) (tls.EncryptedClientHelloKey, []byte) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// ECHConfigContents: config ID, X25519, public key, HKDF-SHA256/AES128-GCM,
	// maximum name length, public name, no extensions.
	contents := []byte{0, 0, 0x20, 0, 32}
	contents = append(contents, key.PublicKey().Bytes()...)
	contents = append(contents, 0, 4, 0, 1, 0, 1, 0, byte(len("public.test")))
	contents = append(contents, "public.test"...)
	contents = append(contents, 0, 0)
	config := binary.BigEndian.AppendUint16([]byte{0xfe, 0x0d}, uint16(len(contents)))
	config = append(config, contents...)
	list := binary.BigEndian.AppendUint16(nil, uint16(len(config)))
	list = append(list, config...)
	return tls.EncryptedClientHelloKey{Config: config, PrivateKey: key.Bytes(), SendAsRetry: true}, list
}

func strictECHCertificate(t *testing.T, names ...string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: names, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// Observe bytes before the Go server decrypts ClientHelloInner. Recording the
// first flight catches fallback even when the application request later fails.
type strictECHListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*strictECHRecordingConn
}

type strictECHRecordingConn struct {
	net.Conn
	mu   sync.Mutex
	data []byte
}

func (l *strictECHListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	r := &strictECHRecordingConn{Conn: c}
	l.mu.Lock()
	l.conns = append(l.conns, r)
	l.mu.Unlock()
	return r, nil
}

func (c *strictECHRecordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	if len(c.data) < 32768 {
		c.data = append(c.data, p[:min(n, 32768-len(c.data))]...)
	}
	c.mu.Unlock()
	return n, err
}

func (l *strictECHListener) check(t *testing.T, wantHello bool, minHellos int) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	hellos := 0
	for _, c := range l.conns {
		c.mu.Lock()
		data := bytes.Clone(c.data)
		c.mu.Unlock()
		if bytes.Contains(data, []byte("inner.test")) {
			t.Error("inner hostname was sent in plaintext")
		}
		if len(data) > 0 && data[0] == 22 {
			hellos++
			if !bytes.Contains(data, []byte("public.test")) {
				t.Error("ClientHello does not contain the public outer name")
			}
		}
	}
	if !wantHello && hellos != 0 {
		t.Errorf("sent %d ClientHellos before rejecting an unusable/missing config", hellos)
	}
	if hellos < minHellos {
		t.Errorf("ClientHellos = %d, want at least %d", hellos, minHellos)
	}
}

func TestBrowserStrictECH(t *testing.T) {
	for _, mode := range []string{BrowserXHTTPModePacketUp, BrowserXHTTPModeStreamUp} {
		for _, scenario := range []string{"accepted", "retry", "empty-retry", "unsupported-retry", "unsupported-version", "unsupported-kem", "malformed", "bad-outer-cert", "tls12-server", "ech-tls12-server", "retry-rejected"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				serverKey, list := strictECHKey(t)
				cert, roots := strictECHCertificate(t, "inner.test", "public.test")
				if scenario == "bad-outer-cert" {
					cert, roots = strictECHCertificate(t, "inner.test")
				}
				config := &tls.Config{Certificates: []tls.Certificate{cert}, EncryptedClientHelloKeys: []tls.EncryptedClientHelloKey{serverKey}}
				wantSuccess := scenario == "accepted" || scenario == "retry"
				wantHello := true
				minHellos := 1
				switch scenario {
				case "retry", "empty-retry", "unsupported-retry", "bad-outer-cert", "retry-rejected":
					_, list = strictECHKey(t)
					if scenario == "empty-retry" {
						config.EncryptedClientHelloKeys[0].SendAsRetry = false
					}
					if scenario == "unsupported-retry" {
						// The server can authenticate its outer name and advertise a
						// syntactically valid config with an unsupported HKDF ID.
						config.EncryptedClientHelloKeys[0].Config = bytes.Clone(serverKey.Config)
						config.EncryptedClientHelloKeys[0].Config[43] = 0xff
						config.EncryptedClientHelloKeys[0].Config[44] = 0xff
					}
					if scenario == "retry" || scenario == "retry-rejected" {
						minHellos = 2
					}
					if scenario == "retry-rejected" {
						secondKey, _ := strictECHKey(t)
						// Advertise a key whose private half is unavailable, so every
						// attempt (including the authenticated retry) is rejected.
						config.EncryptedClientHelloKeys[0].PrivateKey = secondKey.PrivateKey
					}
				case "unsupported-version":
					list[3] = 0x0c
					wantHello, minHellos = false, 0
				case "unsupported-kem":
					list[7], list[8] = 0xff, 0xff
					wantHello, minHellos = false, 0
				case "malformed":
					list = []byte{0, 1, 0xff}
					wantHello, minHellos = false, 0
				case "tls12-server":
					config.EncryptedClientHelloKeys = nil
					config.MaxVersion = tls.VersionTLS12
				case "ech-tls12-server":
					config.MaxVersion = tls.VersionTLS12
				}
				var requests atomic.Int32
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if !wantSuccess || !r.TLS.ECHAccepted || r.TLS.ServerName != "inner.test" || r.ProtoMajor != 2 {
						t.Errorf("unexpected application request: ECH=%v SNI=%q protocol=%s", r.TLS.ECHAccepted, r.TLS.ServerName, r.Proto)
					}
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, "strict ECH")
					}
				}))
				recorder := &strictECHListener{Listener: server.Listener}
				server.Listener = recorder
				server.EnableHTTP2 = true
				server.TLS = config
				server.Config.ErrorLog = log.New(io.Discard, "", 0)
				server.StartTLS()
				defer server.Close()
				client, err := NewBrowserXHTTPClient(ctx, "https://inner.test", BrowserXHTTPOptions{
					Mode: mode, ECHConfigList: list, TrustedRootCertificates: roots,
					DialContext: func(ctx context.Context) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, "tcp", recorder.Addr().String())
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
				data, err := io.ReadAll(conn)
				_ = conn.Close()
				if wantSuccess {
					if err != nil || string(data) != "strict ECH" {
						t.Fatalf("response = %q, error = %v", data, err)
					}
				} else {
					if err == nil || ctx.Err() != nil {
						t.Fatalf("expected native ECH failure, got %v (context %v)", err, ctx.Err())
					}
					if scenario == "empty-retry" && !strings.Contains(err.Error(), "STRICT_ECH_REQUIRED") {
						t.Errorf("empty retry error = %v", err)
					}
					t.Logf("rejected: %v", err)
				}
				_ = client.Close()
				server.Close()
				if !wantSuccess && requests.Load() != 0 {
					t.Errorf("sent %d application requests after ECH failure", requests.Load())
				}
				recorder.check(t, wantHello, minHellos)
			})
		}
	}
}

func TestStrictECHEnginePolicy(t *testing.T) {
	if err := checkLibrary(); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine()
	defer engine.Destroy()
	if err := engine.SetStrictECH(true); err != nil {
		t.Fatal(err)
	}
	params := NewEngineParams()
	defer params.Destroy()
	params.SetEnableCheckResult(false)
	params.SetEnableQuic(true)
	if got := engine.StartWithParams(params); got != ResultIllegalArgument {
		t.Fatalf("Strict + QUIC start = %v", got)
	}
	params.SetEnableQuic(false)
	_ = params.SetHostResolverRules("MAP inner.test 127.0.0.1")
	if got := engine.StartWithParams(params); got != ResultSuccess {
		t.Fatal(got)
	}
	defer engine.Shutdown()
	if err := engine.SetStrictECH(false); err == nil {
		t.Fatal("allowed changing a running engine's policy")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("received an application request without ECH")
	}))
	recorder := &strictECHListener{Listener: server.Listener}
	server.Listener = recorder
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, port, _ := net.SplitHostPort(recorder.Addr().String())
	client, err := NewBrowserXHTTPClient(ctx, "https://inner.test:"+port, BrowserXHTTPOptions{Engine: engine})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn, err := client.DialContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Read(make([]byte, 1))
	_ = conn.Close()
	if err == nil || !strings.Contains(err.Error(), "STRICT_ECH_REQUIRED") {
		t.Fatalf("missing config error = %v", err)
	}
	_ = client.Close()
	engine.CloseAllConnections()
	server.Close()
	recorder.check(t, false, 0)
}

func TestBrowserStrictECHReuse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	key, list := strictECHKey(t)
	rotated, _ := strictECHKey(t)
	rotated.SendAsRetry = false
	cert, roots := strictECHCertificate(t, "inner.test", "public.test")
	type observation struct {
		remote string
		state  tls.ConnectionState
	}
	observations := make(chan observation, 8)
	var reject atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observations <- observation{r.RemoteAddr, *r.TLS}
		_, _ = io.WriteString(w, "ECH session")
	}))
	recorder := &strictECHListener{Listener: server.Listener}
	server.Listener = recorder
	server.EnableHTTP2 = true
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetEncryptedClientHelloKeys: func(*tls.ClientHelloInfo) ([]tls.EncryptedClientHelloKey, error) {
			if reject.Load() {
				return []tls.EncryptedClientHelloKey{rotated}, nil
			}
			return []tls.EncryptedClientHelloKey{key}, nil
		},
	}
	server.StartTLS()
	defer server.Close()
	client, err := NewBrowserXHTTPClient(ctx, "https://inner.test", BrowserXHTTPOptions{
		ECHConfigList: list, TrustedRootCertificates: roots,
		DialContext: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", recorder.Addr().String())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// A test build can enable HappyEyeballsV3 to exercise TlsStreamAttempt.
	// Inspect actual events so a stale shared library cannot silently test the
	// default SSLConnectJob path again.
	if expected := os.Getenv("CRONET_TEST_CONNECT_EVENT"); expected != "" {
		path := filepath.Join(t.TempDir(), "netlog.json")
		if !client.engine.StartNetLogToFile(path, false) {
			t.Fatal("start NetLog")
		}
		defer func() {
			client.engine.StopNetLog()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var netlog struct {
				Constants struct {
					Types map[string]int `json:"logEventTypes"`
				} `json:"constants"`
				Events []struct {
					Type int `json:"type"`
				} `json:"events"`
			}
			if err := json.Unmarshal(data, &netlog); err != nil {
				t.Fatal(err)
			}
			id, ok := netlog.Constants.Types[expected]
			for _, event := range netlog.Events {
				if ok && event.Type == id {
					t.Logf("observed native path: %s", expected)
					return
				}
			}
			t.Errorf("native path event %s was not observed", expected)
		}()
	}
	if err := client.prepareECH(ctx); err != nil {
		t.Fatal(err)
	}
	request := func(success bool) observation {
		t.Helper()
		r, err := newStreamingRequest(client.engine, client.executor, "GET", "https://inner.test/", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.start(); err != nil {
			t.Fatal(err)
		}
		defer r.close()
		if err := r.wait(ctx); err != nil {
			if success || ctx.Err() != nil {
				t.Fatalf("request failed: %v", err)
			}
			return observation{}
		}
		if !success {
			t.Fatal("resumed a connection after ECH rejection")
		}
		select {
		case result := <-observations:
			if !result.state.ECHAccepted || result.state.ServerName != "inner.test" || result.state.NegotiatedProtocol != "h2" {
				t.Fatalf("ECH/protocol state = %+v", result.state)
			}
			return result
		case <-ctx.Done():
			t.Fatal(ctx.Err())
			return observation{}
		}
	}
	first := request(true)
	second := request(true)
	if first.remote != second.remote {
		t.Error("sequential HTTP/2 requests did not reuse the connection")
	}
	client.engine.CloseAllConnections()
	resumed := request(true)
	if resumed.remote == first.remote || !resumed.state.DidResume {
		t.Errorf("expected TLS session resumption on a new TCP connection: remote=%s, resumed=%v", resumed.remote, resumed.state.DidResume)
	}
	reject.Store(true)
	client.engine.CloseAllConnections()
	request(false)
	// Keep the engine alive until the optional NetLog check has stopped logging.
	client.engine.CloseAllConnections()
	server.Close()
	if len(observations) != 0 {
		t.Fatal("application data sent after ECH rejection")
	}
	recorder.check(t, true, 3)
}
