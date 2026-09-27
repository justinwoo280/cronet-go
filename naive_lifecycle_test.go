//go:build with_cronet_test

package cronet

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"

	mDNS "github.com/miekg/dns"
)

func newLifecycleNaiveClient(t *testing.T, headers map[string]string) *NaiveClient {
	t.Helper()
	client, err := NewNaiveClient(NaiveClientOptions{
		ServerAddress: M.ParseSocksaddr("127.0.0.1:1"), ExtraHeaders: headers,
		DNSResolver: func(_ context.Context, request *mDNS.Msg) *mDNS.Msg {
			return new(mDNS.Msg).SetReply(request)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestNaiveDialBeforeStartCancellation(t *testing.T) {
	client := newLifecycleNaiveClient(t, nil)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.DialEarly(ctx, M.ParseSocksaddr("example.com:443"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dial ignored the caller's deadline")
	}
}

func TestNaiveConcurrentDialAndClose(t *testing.T) {
	for i := 0; i < 32; i++ {
		client := newLifecycleNaiveClient(t, nil)
		if err := client.Start(); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var workers sync.WaitGroup
		for j := 0; j < 12; j++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				conn, err := client.DialEarly(context.Background(), M.ParseSocksaddr("example.com:443"))
				if err == nil {
					_ = conn.Close()
				} else if !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
			}()
		}
		workers.Add(1)
		go func() { defer workers.Done(); <-start; client.CloseAllConnections() }()
		close(start)
		runtime.Gosched()
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
		workers.Wait()
		if _, err := client.DialEarly(context.Background(), M.ParseSocksaddr("example.com:443")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("dial after close: %v", err)
		}
	}
}

func TestNaiveStartFailureReleasesConnection(t *testing.T) {
	client := newLifecycleNaiveClient(t, map[string]string{"invalid header": "value"})
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for i := 0; i < 16; i++ {
		if _, err := client.DialEarly(context.Background(), M.ParseSocksaddr("example.com:443")); !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("invalid header: %v", err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed streams retained an active connection reservation")
	}
}
