//go:build with_cronet_test

package cronet

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type callbackRegistryCounts struct {
	requests, uploads, buffers, runnables, executors, streams, listeners int
}

func countCallbackRegistries() (counts callbackRegistryCounts) {
	urlRequestCallbackAccess.RLock()
	counts.requests = len(urlRequestCallbackMap)
	urlRequestCallbackAccess.RUnlock()
	uploadDataAccess.RLock()
	counts.uploads = len(uploadDataProviderMap)
	uploadDataAccess.RUnlock()
	bufferCallbackAccess.RLock()
	counts.buffers = len(bufferCallbackMap)
	bufferCallbackAccess.RUnlock()
	runnableAccess.RLock()
	counts.runnables = len(runnableMap)
	runnableAccess.RUnlock()
	executorAccess.RLock()
	counts.executors = len(executors)
	executorAccess.RUnlock()
	bidirectionalStreamAccess.RLock()
	counts.streams = len(bidirectionalStreamMap)
	bidirectionalStreamAccess.RUnlock()
	urlRequestFinishedInfoListenerAccess.RLock()
	counts.listeners = len(urlRequestFinishedInfoListenerMap)
	urlRequestFinishedInfoListenerAccess.RUnlock()
	return
}

func TestUnusedCallbacksDestroyReleasesHandlers(t *testing.T) {
	if err := checkLibrary(); err != nil {
		t.Fatal(err)
	}
	before := countCallbackRegistries()
	var callbacks []URLRequestCallback
	var uploads []UploadDataProvider
	var buffers []BufferCallback
	var runnables []Runnable
	var executors []Executor
	var listeners []URLRequestFinishedInfoListener
	for i := 0; i < 16; i++ {
		callbacks = append(callbacks, NewURLRequestCallback(&streamingRequest{}))
		uploads = append(uploads, NewUploadDataProvider(&fixedUploadProvider{data: make([]byte, 1024)}))
		buffers = append(buffers, NewBufferCallback(func(BufferCallback, Buffer) {}))
		runnables = append(runnables, NewRunnable(func(Runnable) { t.Error("unused runnable executed") }))
		executors = append(executors, NewExecutor(func(Executor, Runnable) { t.Error("unused executor invoked") }))
		listeners = append(listeners, NewURLRequestFinishedInfoListener(func(URLRequestFinishedInfoListener, RequestFinishedInfo, URLResponseInfo, Error) {}))
	}
	for i := range callbacks {
		callbacks[i].Destroy()
		uploads[i].Destroy()
		buffers[i].Destroy()
		runnables[i].Destroy()
		executors[i].Destroy()
		listeners[i].Destroy()
	}
	if after := countCallbackRegistries(); after != before {
		t.Fatalf("retained handlers: before %+v, after %+v", before, after)
	}
}

func TestUnstartedStreamsDestroyReleasesHandlers(t *testing.T) {
	if err := checkLibrary(); err != nil {
		t.Fatal(err)
	}
	before := countCallbackRegistries()
	engine := NewEngine()
	defer engine.Destroy()
	params := NewEngineParams()
	result := engine.StartWithParams(params)
	params.Destroy()
	if result != ResultSuccess {
		t.Fatal(result)
	}
	defer engine.Shutdown()
	var streams []BidirectionalStream
	for i := 0; i < 16; i++ {
		streams = append(streams, engine.StreamEngine().CreateStream(&bidirectionalHandler{}))
	}
	for _, stream := range streams {
		stream.Destroy()
	}
	if after := countCallbackRegistries(); after != before {
		t.Fatalf("retained stream handlers: before %+v, after %+v", before, after)
	}
}

func TestCallbackCanDestroyAndReplaceItself(t *testing.T) {
	if err := checkLibrary(); err != nil {
		t.Fatal(err)
	}
	// The allocator commonly reuses these small handles immediately. Keep all
	// replacements until after the old callbacks return, then exercise them.
	var replacements []Runnable
	var ran atomic.Int32
	for i := 0; i < 128; i++ {
		runnable := NewRunnable(func(self Runnable) {
			self.Destroy()
			replacements = append(replacements, NewRunnable(func(Runnable) { ran.Add(1) }))
		})
		runnable.Run()
	}
	for _, replacement := range replacements {
		replacement.Run()
		replacement.Destroy()
	}
	if got := ran.Load(); got != 128 {
		t.Fatalf("only %d replacement handlers survived", got)
	}
}

func TestRoundTripperReleasesCallbackRegistries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "response")
	}))
	defer server.Close()
	before := countCallbackRegistries()
	transport := &RoundTripper{}
	defer transport.close()
	for i := 0; i < 32; i++ {
		request, _ := http.NewRequestWithContext(context.Background(), "GET", server.URL, nil)
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	transport.close()
	if after := countCallbackRegistries(); after != before {
		t.Fatalf("retained HTTP handlers: before %+v, after %+v", before, after)
	}
}
