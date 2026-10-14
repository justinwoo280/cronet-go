//go:build with_cronet_test

package cronet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A proxy can terminate the download independently of a still writable upload.
// The connection must stop uploading, preserve the final response bytes, and
// allow the next session to use the same client.
func TestBrowserXHTTPDownloadEndStopsUpload(t *testing.T) {
	for _, mode := range []string{BrowserXHTTPModePacketUp, BrowserXHTTPModeStreamUp} {
		t.Run(mode, func(t *testing.T) {
			payload := bytes.Repeat([]byte("last download bytes"), 100)
			client := newBrowserDuplexTestClient(t, mode, false, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write(payload)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
			})
			for i := 0; i < 2; i++ {
				conn := dialBrowserDuplexTest(t, client)
				got, err := io.ReadAll(conn)
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("session %d download: %d bytes, %v", i, len(got), err)
				}
				if n, err := conn.Write([]byte("upload after download ended")); n != 0 || err == nil {
					t.Fatalf("session %d accepted upload after download EOF: %d, %v", i, n, err)
				}
				select {
				case <-conn.uploadDone:
				case <-time.After(time.Second):
					t.Fatal("upload worker survived download EOF")
				}
			}
		})
	}
}

func TestBrowserXHTTPDownloadEndStillObservesCancel(t *testing.T) {
	client := newBrowserDuplexTestClient(t, BrowserXHTTPModePacketUp, false, func(w http.ResponseWriter, r *http.Request) {})
	ctx, cancel := context.WithCancel(client.ctx)
	defer cancel()
	raw, err := client.DialContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn := raw.(*browserXHTTPConn)
	t.Cleanup(func() { _ = conn.Close() })
	select {
	case <-conn.reader.doneChannel():
	case <-client.ctx.Done():
		t.Fatal("download did not end")
	}
	// Let the terminal callback be consumed before cancelling the caller.
	select {
	case <-conn.uploadDone:
	case <-time.After(time.Second):
		t.Error("idle upload worker survived download EOF")
	}
	cancel()
	deadline := time.After(time.Second)
	for {
		client.mu.Lock()
		_, registered := client.conns[conn]
		client.mu.Unlock()
		if !registered {
			break
		}
		select {
		case <-deadline:
			t.Fatal("connection retained after download EOF and caller cancellation")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancellation = %v", err)
	}
}

func TestBrowserXHTTPNativeRequestCancellation(t *testing.T) {
	for _, mode := range []string{BrowserXHTTPModePacketUp, BrowserXHTTPModeStreamOne} {
		t.Run(mode, func(t *testing.T) {
			client := newBrowserDuplexTestClient(t, mode, false, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			conn := dialBrowserDuplexTest(t, client)
			// The native request can be cancelled independently of the caller's
			// context. This must not be mistaken for SetReadDeadline replacing
			// a context, which would retry the same cancelled request forever.
			conn.reader.cancel()
			readDone := make(chan error, 1)
			go func() { _, err := conn.Read(make([]byte, 1)); readDone <- err }()
			select {
			case err := <-readDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("native cancellation = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("read spun on native cancellation")
			}
		})
	}
}

func TestBrowserXHTTPUploadFailureClosesSession(t *testing.T) {
	for _, mode := range []string{BrowserXHTTPModePacketUp, BrowserXHTTPModeStreamUp} {
		t.Run(mode, func(t *testing.T) {
			client := newBrowserDuplexTestClient(t, mode, false, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			conn := dialBrowserDuplexTest(t, client)
			_, _ = conn.Write([]byte("rejected upload"))
			select {
			case <-conn.ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("upload failure did not cancel the session")
			}
			_ = conn.Close()
			if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("upload error replaced by cancellation: %v", err)
			}
		})
	}
}

func TestBrowserXHTTPCancellationChurn(t *testing.T) {
	for _, mode := range []string{BrowserXHTTPModePacketUp, BrowserXHTTPModeStreamUp, BrowserXHTTPModeStreamOne} {
		t.Run(mode, func(t *testing.T) {
			before := countCallbackRegistries()
			client := newBrowserDuplexTestClient(t, mode, false, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			for round := 0; round < 8; round++ {
				var wg sync.WaitGroup
				for i := 0; i < 16; i++ {
					ctx, cancel := context.WithCancel(client.ctx)
					raw, err := client.DialContext(ctx)
					if err != nil {
						cancel()
						t.Fatal(err)
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						cancel()
						_ = raw.Close()
					}()
				}
				wg.Wait()
				client.mu.Lock()
				remaining := len(client.conns)
				client.mu.Unlock()
				if remaining != 0 {
					t.Fatalf("round %d retained %d cancelled sessions", round, remaining)
				}
			}
			_ = client.Close()
			if after := countCallbackRegistries(); after != before {
				t.Fatalf("callbacks retained after cancellation churn: before %+v, after %+v", before, after)
			}
		})
	}
}

func TestBrowserXHTTPStalledDownloadDeadlineRecovery(t *testing.T) {
	for _, mode := range []string{BrowserXHTTPModePacketUp, BrowserXHTTPModeStreamUp} {
		t.Run(mode, func(t *testing.T) {
			var downloads atomic.Int32
			client := newBrowserDuplexTestClient(t, mode, false, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					_, _ = io.Copy(io.Discard, r.Body)
					return
				}
				if downloads.Add(1) == 1 {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-r.Context().Done() // TCP/H2 is healthy, only this GET is silent.
					return
				}
				_, _ = io.WriteString(w, "recovered")
			})
			first := dialBrowserDuplexTest(t, client)
			if _, err := first.Write([]byte("upload still works")); err != nil {
				t.Fatal(err)
			}
			_ = first.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			_, err := first.Read(make([]byte, 1))
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatalf("stalled download deadline = %v", err)
			}
			_ = first.Close()
			second := dialBrowserDuplexTest(t, client)
			got, err := io.ReadAll(second)
			if err != nil || string(got) != "recovered" {
				t.Fatalf("new session after stalled GET = %q, %v", got, err)
			}
		})
	}
}
