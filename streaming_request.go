package cronet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

const streamingReadBufferSize = 32 * 1024

// streamingExecutor serializes native callbacks and application operations for
// one request, including destruction after the terminal callback has returned.
type streamingExecutor struct {
	parent  Executor
	native  Executor
	mu      sync.Mutex
	queue   []func()
	running bool
}

func newStreamingExecutor(parent Executor) *streamingExecutor {
	e := &streamingExecutor{parent: parent}
	e.native = NewExecutor(func(_ Executor, command Runnable) {
		e.submit(func() {
			command.Run()
			command.Destroy()
		})
	})
	return e
}

func (e *streamingExecutor) submit(f func()) {
	e.mu.Lock()
	e.queue = append(e.queue, f)
	if e.running {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.mu.Unlock()
	go e.parent.Execute(NewRunnable(func(_ Runnable) {
		for {
			e.mu.Lock()
			if len(e.queue) == 0 {
				e.running = false
				e.mu.Unlock()
				return
			}
			f := e.queue[0]
			e.queue[0] = nil
			e.queue = e.queue[1:]
			e.mu.Unlock()
			f()
		}
	}))
}

// Request state used by native calls belongs to serial. mu protects only the
// response stream shared with net.Conn readers. Native Read takes ownership of
// its buffer; only OnReadCompleted transfers that ownership back to us.
type streamingRequest struct {
	serial                         *streamingExecutor
	request                        URLRequest
	callback                       URLRequestCallback
	upload                         *streamingUpload
	started, terminal, readPending bool
	activeUploads                  int
	cleanupQueued                  bool
	mu                             sync.Mutex
	status                         int
	header                         http.Header
	err                            error
	pending                        []byte
	readCh                         chan struct{}
	done                           chan struct{}
	onDone                         func(error)
}

// upload ownership passes to this function, including initialization failures.
func newStreamingRequest(engine Engine, executor Executor, method, rawURL string, headers http.Header, upload UploadDataProviderHandler, onDone func(error)) (*streamingRequest, error) {
	if engine == (Engine{}) || executor == (Executor{}) {
		return nil, errors.New("cronet: engine and executor are required")
	}
	r := &streamingRequest{
		serial: newStreamingExecutor(executor),
		readCh: make(chan struct{}, 1), done: make(chan struct{}),
		header: make(http.Header), onDone: onDone,
	}
	params := NewURLRequestParams()
	defer params.Destroy()
	params.SetMethod(method)
	params.SetDisableCache(true)
	for key, values := range headers {
		for _, value := range values {
			header := NewHTTPHeader()
			header.SetName(key)
			header.SetValue(value)
			params.AddHeader(header)
			header.Destroy()
		}
	}
	if upload != nil {
		r.upload = &streamingUpload{request: r, handler: upload}
		r.upload.provider = NewUploadDataProvider(r.upload)
		params.SetUploadDataProvider(r.upload.provider)
		params.SetUploadDataExecutor(r.serial.native)
	}
	r.callback = NewURLRequestCallback(r)
	r.request = NewURLRequest()
	if result := r.request.InitWithParams(engine, rawURL, params, r.callback, r.serial.native); result != ResultSuccess {
		err := fmt.Errorf("cronet: initialize %s request: result %d", method, result)
		r.serial.submit(func() { r.finish(err) })
		<-r.done
		return nil, err
	}
	return r, nil
}

func (r *streamingRequest) start() error {
	result := make(chan error, 1)
	r.serial.submit(func() {
		if r.terminal || r.started {
			result <- errors.New("cronet: request already started or closed")
			return
		}
		if code := r.request.Start(); code != ResultSuccess {
			err := fmt.Errorf("cronet: start request: result %d", code)
			r.finish(err)
			result <- err
			return
		}
		r.started = true
		result <- nil
	})
	err := <-result
	if err != nil {
		r.mu.Lock()
		finished := r.err != nil
		r.mu.Unlock()
		if finished {
			<-r.done
		}
	}
	return err
}

func (r *streamingRequest) Read(p []byte) (int, error) {
	return r.readContext(context.Background(), p)
}

func (r *streamingRequest) readContext(ctx context.Context, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		r.mu.Lock()
		if len(r.pending) > 0 {
			n := copy(p, r.pending)
			r.pending = r.pending[n:]
			if len(r.pending) == 0 {
				r.pending = nil
				// Submit before releasing mu so final cleanup cannot race this
				// last consumer operation on the native executor.
				if r.err == nil {
					r.serial.submit(r.startRead)
				}
			}
			r.mu.Unlock()
			return n, nil
		}
		err := r.err
		r.mu.Unlock()
		if err != nil {
			return 0, err
		}
		select {
		case <-r.readCh:
		case <-r.done:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (r *streamingRequest) startRead() {
	if r.terminal || r.readPending {
		return
	}
	r.mu.Lock()
	ready := len(r.pending) == 0 && r.err == nil
	r.mu.Unlock()
	if !ready {
		return
	}
	buffer := NewBuffer()
	buffer.InitWithAlloc(streamingReadBufferSize)
	r.readPending = true
	if result := r.request.Read(buffer); result != ResultSuccess {
		// READ_FAILED has already transferred the buffer to Cronet.
		if result != ResultIllegalStateReadFailed {
			buffer.Destroy()
		}
		r.readPending = false
		r.fail(fmt.Errorf("cronet: read response: result %d", result))
	}
}

// cancel is nonblocking and is safe from callbacks; close must be called by an
// application goroutine, never by an executor callback.
func (r *streamingRequest) cancel() {
	r.mu.Lock()
	if r.err == nil {
		r.serial.submit(func() {
			if r.terminal {
				return
			}
			if r.upload != nil {
				r.upload.stop()
			}
			if r.started {
				r.request.Cancel()
			} else {
				r.finish(context.Canceled)
			}
		})
	}
	r.mu.Unlock()
}

func (r *streamingRequest) close() error {
	r.cancel()
	<-r.done
	return nil
}

func (r *streamingRequest) wait(ctx context.Context) error {
	// Upload responses can contain a body. Drain it so Cronet reaches its
	// terminal callback even when the server sends more than one buffer.
	buffer := make([]byte, streamingReadBufferSize)
	for {
		_, err := r.readContext(ctx, buffer)
		if err != nil {
			_ = r.close()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (r *streamingRequest) fail(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	if r.upload != nil {
		r.upload.stop()
	}
	r.request.Cancel()
}

func (r *streamingRequest) OnRedirectReceived(_ URLRequestCallback, _ URLRequest, info URLResponseInfo, _ string) {
	r.fail(fmt.Errorf("cronet: unexpected HTTP redirect %d %s", info.StatusCode(), info.StatusText()))
}

func (r *streamingRequest) OnResponseStarted(_ URLRequestCallback, _ URLRequest, info URLResponseInfo) {
	r.mu.Lock()
	r.status = info.StatusCode()
	for i := 0; i < info.HeaderSize(); i++ {
		header := info.HeaderAt(i)
		r.header.Add(header.Name(), header.Value())
	}
	r.mu.Unlock()
	if info.StatusCode() != http.StatusOK {
		r.fail(fmt.Errorf("cronet: HTTP status %d %s", info.StatusCode(), info.StatusText()))
		return
	}
	r.startRead()
}

func (r *streamingRequest) OnReadCompleted(_ URLRequestCallback, _ URLRequest, _ URLResponseInfo, buffer Buffer, bytesRead int64) {
	data := append([]byte(nil), buffer.DataSlice()[:bytesRead]...)
	buffer.Destroy()
	r.readPending = false
	r.mu.Lock()
	r.pending = data
	select {
	case r.readCh <- struct{}{}:
	default:
	}
	r.mu.Unlock()
	if len(data) == 0 {
		r.serial.submit(r.startRead)
	}
}

func (r *streamingRequest) OnSucceeded(_ URLRequestCallback, _ URLRequest, _ URLResponseInfo) {
	r.finish(io.EOF)
}
func (r *streamingRequest) OnFailed(_ URLRequestCallback, _ URLRequest, _ URLResponseInfo, err Error) {
	r.finish(ErrorFromError(err))
}
func (r *streamingRequest) OnCanceled(_ URLRequestCallback, _ URLRequest, _ URLResponseInfo) {
	r.finish(context.Canceled)
}

func (r *streamingRequest) finish(err error) {
	if r.terminal {
		return
	}
	r.terminal = true
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	if r.upload != nil {
		r.upload.stop()
	}
	r.cleanup()
}

func (r *streamingRequest) cleanup() {
	if !r.terminal || r.activeUploads != 0 || r.cleanupQueued {
		return
	}
	r.cleanupQueued = true
	r.serial.submit(func() {
		if r.upload != nil {
			r.upload.destroy()
		}
		r.request.Destroy()
		cleanupURLRequestCallback(r.callback.ptr) // Also covers unstarted requests.
		r.callback.Destroy()
		r.serial.native.Destroy()
		r.mu.Lock()
		err := r.err
		close(r.done)
		r.mu.Unlock()
		if r.onDone != nil {
			r.onDone(err)
		}
	})
}

// Blocking readers run off the executor, but sink notifications return to the
// serialized queue. Destruction waits for every notification, even on cancel.
type streamingUpload struct {
	request   *streamingRequest
	handler   UploadDataProviderHandler
	provider  UploadDataProvider
	destroyed bool
}

func (p *streamingUpload) Length(self UploadDataProvider) int64 { return p.handler.Length(self) }
func (p *streamingUpload) Read(self UploadDataProvider, sink UploadDataSink, buffer Buffer) {
	reader, ok := p.handler.(*readerUploadProvider)
	if !ok {
		p.handler.Read(self, sink, buffer)
		return
	}
	p.request.activeUploads++
	go func() {
		var n int
		var err error
		for attempts := 0; attempts < 100; attempts++ {
			n, err = reader.reader.Read(buffer.DataSlice())
			if n != 0 || err != nil {
				break
			}
		}
		if n == 0 && err == nil {
			err = io.ErrNoProgress
		}
		p.request.serial.submit(func() {
			if err != nil && err != io.EOF {
				sink.OnReadError(err.Error())
			} else {
				sink.OnReadSucceeded(int64(n), err == io.EOF)
			}
			p.request.activeUploads--
			p.request.cleanup()
		})
	}()
}
func (p *streamingUpload) Rewind(self UploadDataProvider, sink UploadDataSink) {
	p.handler.Rewind(self, sink)
}
func (p *streamingUpload) Close(_ UploadDataProvider) { p.stop() }
func (p *streamingUpload) stop() {
	if reader, ok := p.handler.(*readerUploadProvider); ok {
		_ = reader.reader.Close()
	}
}
func (p *streamingUpload) destroy() {
	if p.destroyed {
		return
	}
	p.destroyed = true
	uploadDataAccess.Lock()
	delete(uploadDataProviderMap, p.provider.ptr)
	uploadDataAccess.Unlock()
	p.provider.Destroy()
}

type fixedUploadProvider struct {
	data []byte
	off  int
}

func (p *fixedUploadProvider) Length(UploadDataProvider) int64 { return int64(len(p.data)) }
func (p *fixedUploadProvider) Read(_ UploadDataProvider, sink UploadDataSink, buffer Buffer) {
	n := copy(buffer.DataSlice(), p.data[p.off:])
	p.off += n
	if n == 0 {
		sink.OnReadError("read past fixed upload length")
		return
	}
	sink.OnReadSucceeded(int64(n), false)
}
func (p *fixedUploadProvider) Rewind(_ UploadDataProvider, sink UploadDataSink) {
	p.off = 0
	sink.OnRewindSucceeded()
}
func (*fixedUploadProvider) Close(UploadDataProvider) {}

type readerUploadProvider struct{ reader io.ReadCloser }

func (*readerUploadProvider) Length(UploadDataProvider) int64 { return -1 }
func (*readerUploadProvider) Read(UploadDataProvider, UploadDataSink, Buffer) {
	panic("reader uploads require streamingUpload")
}
func (*readerUploadProvider) Rewind(_ UploadDataProvider, sink UploadDataSink) {
	sink.OnRewindError("rewind is not supported")
}
func (p *readerUploadProvider) Close(UploadDataProvider) { _ = p.reader.Close() }
