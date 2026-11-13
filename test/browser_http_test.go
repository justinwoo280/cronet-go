package test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	cronet "github.com/sagernet/cronet-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type browserEchoSession struct {
	chunks  chan []byte
	mu      sync.Mutex
	next    int
	packets map[int][]byte
}

func TestBrowserALPNEngineLifecycle(t *testing.T) {
	engine := cronet.NewEngine()
	defer engine.Destroy()
	params := cronet.NewEngineParams()
	defer params.Destroy()
	params.SetEnableCheckResult(false)
	params.SetEnableQuic(false)
	require.Error(t, engine.SetALPN([]string{"invalid"}))
	require.NoError(t, engine.SetALPN([]string{"h3"}))
	require.NotEqual(t, cronet.ResultSuccess, engine.StartWithParams(params))
	require.NoError(t, engine.SetALPN([]string{"h2"}))
	require.Equal(t, cronet.ResultSuccess, engine.StartWithParams(params))
	defer engine.Shutdown()
	require.Error(t, engine.SetALPN([]string{"http/1.1"}))
}

func browserEchoHandler(t *testing.T, mode string, protocol int, framed bool) http.Handler {
	var sessions sync.Map
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != protocol {
			t.Errorf("HTTP protocol = %s, expected HTTP/%d", r.Proto, protocol)
		}
		if r.Host != "authority.test" {
			t.Errorf("authority = %q", r.Host)
		}
		if mode == cronet.BrowserXHTTPModeStreamOne {
			if framed {
				w.Header().Set("Content-Type", "application/grpc")
				w.Header().Set("Trailer", "Grpc-Status")
				defer w.Header().Set("Grpc-Status", "0")
			}
			browserEchoStream(w, r)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if framed && r.Method == http.MethodPost {
			parts = []string{"xhttp", r.Header.Get("X-Xhttp-Session")}
		}
		if len(parts) < 2 {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		value, _ := sessions.LoadOrStore(parts[1], &browserEchoSession{
			chunks: make(chan []byte, 64), packets: make(map[int][]byte),
		})
		session := value.(*browserEchoSession)
		if r.Method == http.MethodGet {
			defer sessions.Delete(parts[1])
			for {
				select {
				case chunk := <-session.chunks:
					if _, err := w.Write(chunk); err != nil {
						return
					}
					if err := http.NewResponseController(w).Flush(); err != nil {
						return
					}
				case <-r.Context().Done():
					return
				}
			}
		}
		if framed {
			for {
				chunk, err := readBrowserGRPCPayload(r.Body)
				if err != nil {
					return
				}
				select {
				case session.chunks <- chunk:
				case <-r.Context().Done():
					return
				}
			}
		}
		browserReadUpload(t, w, r, mode, parts, session)
	})
}

func readBrowserGRPCPayload(reader io.Reader) ([]byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if header[0] != 0 || length > 4*1024*1024 {
		return nil, errors.New("invalid gRPC frame")
	}
	message := make([]byte, int(length))
	if _, err := io.ReadFull(reader, message); err != nil {
		return nil, err
	}
	var value wrapperspb.BytesValue
	if err := proto.Unmarshal(message, &value); err != nil {
		return nil, err
	}
	return value.Value, nil
}

func browserEchoStream(w http.ResponseWriter, r *http.Request) {
	buffer := make([]byte, 16*1024)
	for {
		n, err := r.Body.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func browserReadUpload(t *testing.T, w http.ResponseWriter, r *http.Request, mode string, parts []string, session *browserEchoSession) {
	if mode == cronet.BrowserXHTTPModePacketUp {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		sequence, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			t.Error(err)
			return
		}
		session.mu.Lock()
		defer session.mu.Unlock()
		session.packets[sequence] = data
		for {
			chunk, found := session.packets[session.next]
			if !found {
				break
			}
			delete(session.packets, session.next)
			session.next++
			select {
			case session.chunks <- chunk:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	buffer := make([]byte, 16*1024)
	for {
		n, err := r.Body.Read(buffer)
		if n > 0 {
			select {
			case session.chunks <- bytes.Clone(buffer[:n]):
			case <-r.Context().Done():
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func browserTestCertificate(t *testing.T) (tls.Certificate, string) {
	ca, cert, key := generateCertificate(t, "localhost")
	certificate, err := tls.LoadX509KeyPair(cert, key)
	require.NoError(t, err)
	roots, err := os.ReadFile(ca)
	require.NoError(t, err)
	return certificate, string(roots)
}

func browserRoundTrip(t *testing.T, client *cronet.BrowserXHTTPClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := client.DialContext(ctx)
	require.NoError(t, err)
	defer conn.Close()
	payload := bytes.Repeat([]byte("native-browser-http-echo"), 50000)
	written := make(chan error, 1)
	go func() { _, err := conn.Write(payload); written <- err }()
	got := make([]byte, len(payload))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.NoError(t, <-written)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
	_, err = conn.Read(make([]byte, 1))
	var timeout net.Error
	require.ErrorAs(t, err, &timeout)
	require.True(t, timeout.Timeout())
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	_, err = conn.Write([]byte("after deadline"))
	require.NoError(t, err)
	got = make([]byte, len("after deadline"))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	require.Equal(t, "after deadline", string(got))
}

type browserUDPWrapper struct {
	net.Conn
	reads, writes *atomic.Int32
	closed        *atomic.Int32
	once          sync.Once
}

func (c *browserUDPWrapper) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.Conn.Read(p)
}

func (c *browserUDPWrapper) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

func (c *browserUDPWrapper) Close() error {
	c.once.Do(func() { c.closed.Add(1) })
	return c.Conn.Close()
}

func TestBrowserHTTP3(t *testing.T) {
	certificate, roots := browserTestCertificate(t)
	for _, stream := range []struct {
		mode   string
		framed bool
	}{
		{"packet-up", false}, {"stream-up", false}, {"stream-one", false},
		{"stream-up", true}, {"stream-one", true},
	} {
		mode := stream.mode
		name := mode
		if stream.framed {
			name += "/grpc"
		}
		for _, route := range []string{"native", "handoff", "relay"} {
			t.Run(name+"/"+route, func(t *testing.T) {
				// localhost resolves to both ::1 and 127.0.0.1. Let the native
				// resolver choose either family; routed tests explicitly use IPv4.
				packet, err := net.ListenPacket("udp", ":0")
				require.NoError(t, err)
				server := &http3.Server{
					Logger:    slog.Default(),
					TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"h3"}},
					Handler:   browserEchoHandler(t, mode, 3, stream.framed),
				}
				done := make(chan error, 1)
				go func() { done <- server.Serve(packet) }()
				t.Cleanup(func() { _ = server.Close(); _ = packet.Close(); <-done })
				_, port, err := net.SplitHostPort(packet.LocalAddr().String())
				require.NoError(t, err)
				var dials, closed, reads, writes, tcpDials atomic.Int32
				opts := cronet.BrowserXHTTPOptions{
					Mode: mode, Path: "/xhttp", Host: "authority.test",
					GRPCFraming: stream.framed,
					ALPN:        []string{"h3"}, TrustedRootCertificates: roots,
					ScMaxEachPostBytes:   cronet.BrowserXHTTPRange{From: 32768, To: 32768},
					ScMinPostsIntervalMs: cronet.BrowserXHTTPRange{From: 1, To: 1},
				}
				if route != "native" {
					opts.DialContext = func(context.Context) (net.Conn, error) {
						tcpDials.Add(1)
						return nil, errors.New("HTTP/3 must not dial TCP")
					}
					opts.DialUDPContext = func(ctx context.Context) (net.Conn, error) {
						conn, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort("127.0.0.1", port))
						if err != nil {
							return nil, err
						}
						dials.Add(1)
						if route == "relay" {
							return &browserUDPWrapper{Conn: conn, reads: &reads, writes: &writes, closed: &closed}, nil
						}
						return conn, nil
					}
				}
				client, err := cronet.NewBrowserXHTTPClient(context.Background(), "https://localhost:"+port, opts)
				require.NoError(t, err)
				t.Cleanup(func() { _ = client.Close() })
				browserRoundTrip(t, client)
				browserRoundTrip(t, client)
				require.NoError(t, client.Close())
				require.Zero(t, tcpDials.Load())
				if route != "native" {
					require.Positive(t, dials.Load())
				}
				if route == "relay" {
					require.Equal(t, dials.Load(), closed.Load())
					require.Greater(t, reads.Load(), int32(1))
					require.Greater(t, writes.Load(), int32(1))
				}
			})
		}
	}
}

func TestBrowserTCPALPN(t *testing.T) {
	certificate, roots := browserTestCertificate(t)
	for _, alpn := range [][]string{{"h2"}, {"http/1.1"}, {"h2", "http/1.1"}, {"http/1.1", "h2"}} {
		t.Run(strings.Join(alpn, ","), func(t *testing.T) {
			protocol := 2
			if len(alpn) == 1 && alpn[0] == "http/1.1" {
				protocol = 1
			}
			server := httptest.NewUnstartedServer(browserEchoHandler(t, "packet-up", protocol, false))
			server.EnableHTTP2 = protocol == 2
			var handshakes atomic.Int32
			server.TLS = &tls.Config{
				Certificates: []tls.Certificate{certificate},
				GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					handshakes.Add(1)
					if strings.Join(hello.SupportedProtos, ",") != strings.Join(alpn, ",") {
						t.Errorf("offered ALPN = %v, want %v", hello.SupportedProtos, alpn)
					}
					return nil, nil
				},
			}
			server.StartTLS()
			defer server.Close()
			client, err := cronet.NewBrowserXHTTPClient(context.Background(), "https://localhost", cronet.BrowserXHTTPOptions{
				Mode: "packet-up", Path: "/xhttp", Host: "authority.test",
				ALPN: alpn, TrustedRootCertificates: roots,
				ScMaxEachPostBytes:   cronet.BrowserXHTTPRange{From: 32768, To: 32768},
				ScMinPostsIntervalMs: cronet.BrowserXHTTPRange{From: 1, To: 1},
				DialContext: func(ctx context.Context) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
				},
			})
			require.NoError(t, err)
			defer client.Close()
			browserRoundTrip(t, client)
			require.Positive(t, handshakes.Load())
		})
	}
}

func TestBrowserALPNNoFallback(t *testing.T) {
	certificate, roots := browserTestCertificate(t)
	for _, alpn := range []string{"h2", "h3"} {
		t.Run(alpn, func(t *testing.T) {
			var requests, tcpDials, udpDials atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
				// A valid TLS server that deliberately omits ALPN.
				return &tls.Config{Certificates: []tls.Certificate{certificate}}, nil
			}}
			server.StartTLS()
			defer server.Close()
			opts := cronet.BrowserXHTTPOptions{ALPN: []string{alpn}, TrustedRootCertificates: roots,
				DialContext: func(ctx context.Context) (net.Conn, error) {
					tcpDials.Add(1)
					return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
				},
			}
			if alpn == "h3" {
				opts.DialUDPContext = func(context.Context) (net.Conn, error) {
					udpDials.Add(1)
					return nil, errors.New("UDP unavailable")
				}
			}
			client, err := cronet.NewBrowserXHTTPClient(context.Background(), "https://localhost", opts)
			require.NoError(t, err)
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := client.DialContext(ctx)
			require.NoError(t, err)
			defer conn.Close()
			_, err = conn.Read(make([]byte, 1))
			require.Error(t, err)
			require.NotErrorIs(t, err, context.DeadlineExceeded)
			require.Zero(t, requests.Load())
			if alpn == "h3" {
				require.Zero(t, tcpDials.Load())
				require.Positive(t, udpDials.Load())
			}
		})
	}
}

func TestBrowserHTTP3CancelBeforeResponse(t *testing.T) {
	certificate, roots := browserTestCertificate(t)
	for _, mode := range []string{"packet-up", "stream-up", "stream-one"} {
		t.Run(mode, func(t *testing.T) {
			packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
			require.NoError(t, err)
			entered := make(chan struct{}, 1)
			server := &http3.Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}}, Logger: slog.Default(),
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					select {
					case entered <- struct{}{}:
					default:
					}
					<-r.Context().Done()
				}),
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(packet) }()
			t.Cleanup(func() { _ = server.Close(); _ = packet.Close(); <-done })
			var opened, closed, reads, writes atomic.Int32
			client, err := cronet.NewBrowserXHTTPClient(context.Background(), "https://localhost", cronet.BrowserXHTTPOptions{
				Mode: mode, ALPN: []string{"h3"}, TrustedRootCertificates: roots,
				DialUDPContext: func(ctx context.Context) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, "udp", packet.LocalAddr().String())
					if err != nil {
						return nil, err
					}
					opened.Add(1)
					return &browserUDPWrapper{Conn: conn, reads: &reads, writes: &writes, closed: &closed}, nil
				},
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := client.DialContext(ctx)
			require.NoError(t, err)
			_, err = conn.Write([]byte("pending response"))
			require.NoError(t, err)
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("request never reached HTTP/3 server")
			}
			joined := make(chan error, 2)
			go func() { joined <- conn.Close() }()
			go func() { joined <- client.Close() }()
			for range 2 {
				select {
				case err := <-joined:
					require.NoError(t, err)
				case <-ctx.Done():
					t.Fatal("HTTP/3 cancellation did not release the relay")
				}
			}
			require.Positive(t, opened.Load())
			require.Equal(t, opened.Load(), closed.Load())
		})
	}
}

func TestBrowserHTTP3RejectsUntrustedPeer(t *testing.T) {
	certificate, _ := browserTestCertificate(t)
	packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	var requests atomic.Int32
	server := &http3.Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}}, Logger: slog.Default(),
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }),
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(packet) }()
	defer func() { _ = server.Close(); _ = packet.Close(); <-done }()
	client, err := cronet.NewBrowserXHTTPClient(context.Background(), "https://localhost", cronet.BrowserXHTTPOptions{
		ALPN: []string{"h3"}, UseTrustedRootCertificates: true,
		DialUDPContext: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", packet.LocalAddr().String())
		},
	})
	require.NoError(t, err)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := client.DialContext(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, requests.Load())
}
