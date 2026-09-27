//go:build with_cronet_test

package cronet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRoundTripperConcurrentFirstUse(t *testing.T) {
	payload := bytes.Repeat([]byte("response"), 8192)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Test", "first")
		w.Header().Add("X-Test", "second")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	transport := &RoundTripper{}
	defer transport.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			request, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil || !bytes.Equal(data, payload) {
				t.Errorf("response: %d bytes, error %v", len(data), err)
			}
			if response.StatusCode != 503 || response.ProtoMajor != 1 || len(response.Header.Values("X-Test")) != 2 {
				t.Errorf("unexpected response metadata: %+v", response)
			}
		}()
	}
	close(start)
	wg.Wait()
}

type countingRequestBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *countingRequestBody) Close() error {
	b.closed.Add(1)
	return nil
}

func TestRoundTripperCanceledAndInvalidRequest(t *testing.T) {
	transport := &RoundTripper{}
	defer transport.close()
	for _, scenario := range []string{"canceled", "method", "header"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			body := &countingRequestBody{Reader: strings.NewReader("upload")}
			request, _ := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:1", body)
			switch scenario {
			case "canceled":
				cancel()
			case "method":
				request.Method = "bad method"
			case "header":
				request.Header.Set("bad header", "value")
			}
			response, err := transport.RoundTrip(request)
			if response != nil || err == nil {
				t.Fatalf("response %v, error %v", response, err)
			}
			if scenario == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if body.closed.Load() != 1 {
				t.Fatalf("body closed %d times", body.closed.Load())
			}
		})
	}
}

func TestRoundTripperRedirectLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.Copy(w, r.Body)
	}))
	defer server.Close()
	for _, follow := range []bool{false, true} {
		name := "refuse"
		if follow {
			name = "follow"
		}
		t.Run(name, func(t *testing.T) {
			transport := &RoundTripper{CheckRedirect: func(string) bool { return follow }}
			defer transport.close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for i := 0; i < 16; i++ {
				request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/redirect", strings.NewReader("upload"))
				response, err := transport.RoundTrip(request)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if follow {
					if response.StatusCode != 200 || string(data) != "upload" {
						t.Fatalf("followed redirect: %d %q", response.StatusCode, data)
					}
				} else if response.StatusCode != 307 || len(data) != 0 {
					t.Fatalf("refused redirect: %d %q", response.StatusCode, data)
				}
			}
		})
	}
}

func TestRoundTripperReadCloseAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	transport := &RoundTripper{}
	defer transport.close()
	for i := 0; i < 32; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		request, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
		response, err := transport.RoundTrip(request)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, err := response.Body.Read(make([]byte, 1))
			if err == nil {
				t.Error("read succeeded after cancellation")
			}
		}()
		go func() { defer wg.Done(); _ = response.Body.Close() }()
		go func() { defer wg.Done(); cancel(); _ = response.Body.Close() }()
		wg.Wait()
		cancel()
	}
}

func TestRoundTripperCancelBlockedUpload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer server.Close()
	transport := &RoundTripper{}
	defer transport.close()
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", server.URL, reader)
	response, err := transport.RoundTrip(request)
	if response != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("response %v, error %v", response, err)
	}
	if _, err := writer.Write([]byte("closed")); err == nil {
		t.Fatal("request body was not closed on cancellation")
	}
}

func TestRoundTripperReadCompletionAndClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "complete")
	}))
	defer server.Close()
	transport := &RoundTripper{}
	defer transport.close()
	for i := 0; i < 64; i++ {
		request, _ := http.NewRequest("GET", server.URL, nil)
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(response.Body); err != nil {
			t.Fatal(err)
		}
		if _, err := response.Body.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("read after EOF: %v", err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
