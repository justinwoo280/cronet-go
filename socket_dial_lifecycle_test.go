//go:build with_cronet_test

package cronet

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"

	mDNS "github.com/miekg/dns"
)

func TestNativeCustomDialDoesNotBlockNetworkThread(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "unrelated connection completed")
	}))
	defer server.Close()
	blockedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blockedListener.Close()
	blockedAddress := M.SocksaddrFromNet(blockedListener.Addr())
	started, release := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	engine := NewEngine()
	engine.SetDialer(func(address string, port uint16) int {
		if port == blockedAddress.Port {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return NetErrorConnectionFailed.Code()
		}
		conn, err := new(net.Dialer).DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(int(port))))
		if err != nil {
			return toNetError(err).Code()
		}
		defer conn.Close()
		fd, err := dupSocketFD(conn.(*net.TCPConn))
		if err != nil {
			return toNetError(err).Code()
		}
		return fd
	})
	params := NewEngineParams()
	params.SetEnableQuic(false)
	result := engine.StartWithParams(params)
	params.Destroy()
	if result != ResultSuccess {
		engine.Destroy()
		cancel()
		t.Fatalf("start engine: %v", result)
	}
	transport := &RoundTripper{Engine: engine}
	var requests sync.WaitGroup
	defer func() {
		unblock()
		cancel()
		requests.Wait()
		engine.Shutdown()
		engine.Destroy()
		transport.close()
	}()
	requests.Add(1)
	go func() {
		defer requests.Done()
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+blockedAddress.String()+"/blocked", nil)
		response, _ := transport.RoundTrip(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("blocking custom dial was not entered")
	}
	done := make(chan error, 1)
	requests.Add(1)
	go func() {
		defer requests.Done()
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		response, err := transport.RoundTrip(request)
		if err == nil {
			var body []byte
			body, err = io.ReadAll(response.Body)
			response.Body.Close()
			if err == nil && string(body) != "unrelated connection completed" {
				err = errors.New("unexpected response body")
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked dial stalled unrelated work on the same native engine")
	}
}

type pendingNaiveDialer struct{ entered chan context.Context }

func (d pendingNaiveDialer) DialContext(ctx context.Context, network string, _ M.Socksaddr) (net.Conn, error) {
	if network != "tcp" {
		return nil, errors.New("reachability probe escaped to the proxy dialer")
	}
	d.entered <- ctx
	<-ctx.Done()
	return nil, ctx.Err()
}

func (pendingNaiveDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("unexpected packet listener")
}

func TestNaiveResetCancelsPendingDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	dialer := pendingNaiveDialer{entered: make(chan context.Context, 32)}
	client, err := NewNaiveClient(NaiveClientOptions{
		Context: ctx, ServerAddress: M.ParseSocksaddr("127.0.0.1:443"), Dialer: dialer,
		DNSResolver: func(_ context.Context, request *mDNS.Msg) *mDNS.Msg { return new(mDNS.Msg).SetReply(request) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.Start(); err != nil {
		t.Fatal(err)
	}
	connect := func() (NaiveConn, <-chan error) {
		t.Helper()
		conn, err := client.DialEarly(ctx, M.ParseSocksaddr("destination.test:443"))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- conn.HandshakeContext(ctx) }()
		return conn, done
	}
	first, firstDone := connect()
	defer first.Close()
	var firstDial context.Context
	select {
	case firstDial = <-dialer.entered:
	case <-ctx.Done():
		t.Fatal("native dial did not start")
	}
	reset := make(chan struct{})
	go func() { client.CloseAllConnections(); close(reset) }()
	select {
	case <-reset:
	case <-time.After(2 * time.Second):
		t.Fatal("network reset stalled behind a pending custom dial")
	}
	if !errors.Is(firstDial.Err(), context.Canceled) {
		t.Fatal("reset did not cancel the previous dial generation")
	}
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("aborted handshake succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending handshake survived network reset")
	}
	second, secondDone := connect()
	defer second.Close()
	for {
		select {
		case next := <-dialer.entered:
			if next.Err() != nil {
				continue
			}
			if next == firstDial {
				t.Fatal("new connection reused the aborted dial context")
			}
		case <-ctx.Done():
			t.Fatal("reset did not allow a fresh native dial")
		}
		break
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondDone:
		if err == nil {
			t.Fatal("closed handshake succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not release the fresh pending handshake")
	}
}
