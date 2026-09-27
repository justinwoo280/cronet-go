package cronet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"sync"
)

// RoundTripper is a wrapper from URLRequest to http.RoundTripper.
// Configure its exported fields before the first request. A RoundTripper must
// not be copied after first use.
type RoundTripper struct {
	CheckRedirect func(newLocationUrl string) bool
	Engine        Engine
	Executor      Executor

	initOnce      sync.Once
	initErr       error
	closeOnce     sync.Once
	closeEngine   bool
	closeExecutor bool
	executorWG    *sync.WaitGroup
}

func (t *RoundTripper) initialize() {
	if err := checkLibrary(); err != nil {
		t.initErr = err
		return
	}
	if t.Engine == (Engine{}) {
		params := NewEngineParams()
		defer params.Destroy()
		params.SetEnableCheckResult(false)
		params.SetEnableHTTP2(true)
		params.SetEnableQuic(true)
		params.SetEnableBrotli(true)
		params.SetUserAgent("Go-http-client/1.1")
		engine := NewEngine()
		if result := engine.StartWithParams(params); result != ResultSuccess {
			engine.Destroy()
			t.initErr = fmt.Errorf("cronet: start HTTP engine: result %d", result)
			return
		}
		t.Engine = engine
		t.closeEngine = true
	}
	if t.Executor == (Executor{}) {
		// Do not capture t: the executor registry would keep its finalizer alive.
		workers := new(sync.WaitGroup)
		t.executorWG = workers
		t.Executor = NewExecutor(func(_ Executor, command Runnable) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				command.Run()
				command.Destroy()
			}()
		})
		t.closeExecutor = true
	}
	if t.closeEngine || t.closeExecutor {
		runtime.SetFinalizer(t, (*RoundTripper).close)
	}
}

func (t *RoundTripper) close() {
	t.closeOnce.Do(func() {
		if t.closeEngine {
			t.Engine.Shutdown()
			t.Engine.Destroy()
		}
		if t.closeExecutor {
			t.executorWG.Wait()
			t.Executor.Destroy()
		}
	})
}

func (t *RoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	closeBody := func() {
		if request.Body != nil {
			_ = request.Body.Close()
		}
	}
	if err := request.Context().Err(); err != nil {
		closeBody()
		return nil, err
	}
	if request.URL == nil {
		closeBody()
		return nil, errors.New("cronet: request URL is required")
	}
	t.initOnce.Do(t.initialize)
	if t.initErr != nil {
		closeBody()
		return nil, t.initErr
	}

	response := &urlResponse{
		ctx: request.Context(), roundTripper: t, ready: make(chan struct{}),
		response: http.Response{Request: request, Header: make(http.Header)},
	}
	response.response.Body = response
	var upload UploadDataProviderHandler
	if request.Body != nil && request.Body != http.NoBody {
		upload = &readerUploadProvider{reader: request.Body, length: request.ContentLength, getBody: request.GetBody}
	}
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	r, err := newStreamingRequestWithCache(t.Engine, t.Executor, method, request.URL.String(), request.Header, upload, response.onDone, false)
	if err != nil {
		return nil, err
	}
	response.request = r
	// No response callbacks run before start, so publish all handlers first.
	r.onResponse = response.onResponse
	r.onRedirect = response.onRedirect
	if err := r.start(); err != nil {
		return nil, err
	}
	go response.monitorContext()
	select {
	case <-response.ready:
	case <-request.Context().Done():
		r.cancelWithError(request.Context().Err())
		_ = r.close()
		return nil, request.Context().Err()
	}
	if response.response.StatusCode == 0 {
		_ = r.close()
		r.mu.Lock()
		err = r.err
		r.mu.Unlock()
		return nil, err
	}
	return &response.response, nil
}

type urlResponse struct {
	ctx          context.Context
	request      *streamingRequest
	response     http.Response
	ready        chan struct{}
	readyOnce    sync.Once
	roundTripper *RoundTripper // Keep owned native resources alive through Body.Close.
}

func (r *urlResponse) monitorContext() {
	select {
	case <-r.request.done:
	case <-r.ctx.Done():
		r.request.cancelWithError(r.ctx.Err())
	}
}

func (r *urlResponse) onDone(error) {
	r.readyOnce.Do(func() { close(r.ready) })
}

func (r *urlResponse) onRedirect(info URLResponseInfo, location string) bool {
	if r.roundTripper.CheckRedirect == nil || r.roundTripper.CheckRedirect(location) {
		return true
	}
	r.setResponse(info)
	// Publish the refused redirect only after native cancellation and cleanup.
	// The original redirect body is unavailable once the request is canceled.
	r.response.ContentLength = 0
	return false
}

func (r *urlResponse) onResponse(info URLResponseInfo) {
	r.setResponse(info)
	r.readyOnce.Do(func() { close(r.ready) })
}

func (r *urlResponse) setResponse(info URLResponseInfo) {
	r.response.StatusCode = info.StatusCode()
	r.response.Status = strconv.Itoa(info.StatusCode()) + " " + info.StatusText()
	for i := 0; i < info.HeaderSize(); i++ {
		header := info.HeaderAt(i)
		r.response.Header.Add(header.Name(), header.Value())
	}
	r.response.ContentLength = -1
	if length, err := strconv.ParseInt(r.response.Header.Get("Content-Length"), 10, 64); err == nil {
		r.response.ContentLength = length
	}
	switch info.NegotiatedProtocol() {
	case "h2":
		r.response.Proto, r.response.ProtoMajor, r.response.ProtoMinor = "HTTP/2.0", 2, 0
	case "h3", "quic":
		r.response.Proto, r.response.ProtoMajor, r.response.ProtoMinor = "HTTP/3.0", 3, 0
	default:
		r.response.Proto, r.response.ProtoMajor, r.response.ProtoMinor = "HTTP/1.1", 1, 1
	}
}

func (r *urlResponse) Read(p []byte) (int, error) {
	n, err := r.request.readContext(r.ctx, p)
	if err != nil {
		// In particular, EOF must not let a caller destroy an external engine
		// while the terminal callback still owns native request resources.
		_ = r.request.close()
	}
	runtime.KeepAlive(r.roundTripper)
	return n, err
}

func (r *urlResponse) Close() error {
	err := r.request.close()
	runtime.KeepAlive(r.roundTripper)
	return err
}

var _ io.ReadCloser = (*urlResponse)(nil)
