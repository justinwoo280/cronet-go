package cronet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// browserRequest is the response side of either a URLRequest or a native
// bidirectional stream. Cancellation is asynchronous; close joins cleanup.
type browserRequest interface {
	start() error
	readContext(context.Context, []byte) (int, error)
	wait(context.Context) error
	cancel()
	close() error
	doneChannel() <-chan struct{}
	streamError() error
}

func (r *streamingRequest) doneChannel() <-chan struct{} { return r.done }
func (r *streamingRequest) streamError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// URLRequest has no response-trailer callback. Streaming RPCs use Cronet's
// bidirectional API, whose upload does not wait for response headers. Native
// buffers remain owned by this object until a terminal callback AND the upload
// worker have completed; no asynchronous C call retains a Go byte slice.
type browserDuplexRequest struct {
	stream       BidirectionalStream
	method, url  string
	headers      map[string]string
	upload       io.ReadCloser
	decoder      *browserGRPCReader
	readBuffer   Buffer
	writeBuffer  Buffer
	mu           sync.Mutex
	started      bool
	ready        bool
	haveHeaders  bool
	readPending  bool
	writePending bool
	cancelled    bool
	terminal     bool
	err          error
	pending      []byte
	grpcStatus   string
	readyCh      chan struct{}
	notify       chan struct{}
	done         chan struct{}
	uploadDone   chan struct{}
	cleanupDone  chan struct{}
}

func newBrowserDuplexRequest(engine Engine, method, rawURL string, headers http.Header, upload io.ReadCloser, framed bool) *browserDuplexRequest {
	r := &browserDuplexRequest{
		method: method, url: rawURL, headers: make(map[string]string), upload: upload,
		readyCh: make(chan struct{}), notify: make(chan struct{}), done: make(chan struct{}),
		uploadDone: make(chan struct{}), cleanupDone: make(chan struct{}),
	}
	for key, values := range headers {
		separator := ", "
		if strings.EqualFold(key, "Cookie") {
			separator = "; "
		}
		r.headers[key] = strings.Join(values, separator)
	}
	if framed {
		r.upload = newBrowserGRPCUpload(upload)
		r.decoder = &browserGRPCReader{read: r.readRawContext}
	}
	r.readBuffer = NewBuffer()
	r.readBuffer.InitWithAlloc(streamingReadBufferSize)
	r.writeBuffer = NewBuffer()
	r.writeBuffer.InitWithAlloc(streamingReadBufferSize)
	r.stream = engine.StreamEngine().CreateStream(r)
	go r.runUpload()
	go func() {
		<-r.done
		_ = r.upload.Close() // release a worker waiting for application bytes
		<-r.uploadDone
		r.readBuffer.Destroy()
		r.writeBuffer.Destroy()
		close(r.cleanupDone)
	}()
	return r
}

func (r *browserDuplexRequest) start() error {
	r.mu.Lock()
	if r.terminal {
		err := r.err
		r.mu.Unlock()
		return err
	}
	if !r.stream.Start(r.method, r.url, r.headers, 0, false) {
		err := errors.New("cronet xhttp: start bidirectional request")
		r.finishLocked(err)
		r.mu.Unlock()
		<-r.cleanupDone
		return err
	}
	r.started = true
	r.mu.Unlock()
	return nil
}

func (r *browserDuplexRequest) signalLocked() {
	close(r.notify)
	r.notify = make(chan struct{})
}

func (r *browserDuplexRequest) cancelLocked(err error) {
	if r.err == nil {
		r.err = err
	}
	if !r.started && !r.terminal {
		r.finishLocked(err)
		return
	}
	if !r.cancelled && !r.terminal && r.started {
		r.cancelled = true
		r.stream.Cancel()
	}
	r.signalLocked()
}

func (r *browserDuplexRequest) cancel() {
	r.mu.Lock()
	r.cancelLocked(context.Canceled)
	r.mu.Unlock()
}

func (r *browserDuplexRequest) close() error {
	r.cancel()
	<-r.cleanupDone
	if r.decoder != nil {
		r.decoder.release()
	}
	return nil
}

func (r *browserDuplexRequest) doneChannel() <-chan struct{} { return r.cleanupDone }
func (r *browserDuplexRequest) streamError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *browserDuplexRequest) readContext(ctx context.Context, p []byte) (int, error) {
	if r.decoder != nil {
		n, err := r.decoder.readContext(ctx, p)
		if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			r.mu.Lock()
			r.cancelLocked(err)
			r.mu.Unlock()
		}
		return n, err
	}
	return r.readRawContext(ctx, p)
}

func (r *browserDuplexRequest) readRawContext(ctx context.Context, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		r.mu.Lock()
		if len(r.pending) != 0 {
			n := copy(p, r.pending)
			r.pending = r.pending[n:]
			if len(r.pending) == 0 {
				r.pending = nil
				r.startReadLocked()
			}
			r.mu.Unlock()
			return n, nil
		}
		err, notify := r.err, r.notify
		r.mu.Unlock()
		if err != nil {
			return 0, err
		}
		select {
		case <-notify:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (r *browserDuplexRequest) wait(ctx context.Context) error {
	buffer := make([]byte, streamingReadBufferSize)
	for {
		_, err := r.readContext(ctx, buffer)
		if err != nil {
			_ = r.close()
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (r *browserDuplexRequest) startReadLocked() {
	if !r.haveHeaders || r.readPending || r.terminal || r.err != nil {
		return
	}
	r.readPending = true
	if r.stream.Read(r.readBuffer.DataSlice()) == 0 {
		r.cancelLocked(errors.New("cronet xhttp: start bidirectional read"))
	}
}

func (r *browserDuplexRequest) runUpload() {
	defer close(r.uploadDone)
	select {
	case <-r.readyCh:
	case <-r.done:
		return
	}
	buffer := r.writeBuffer.DataSlice()
	for {
		n, err := r.upload.Read(buffer)
		r.mu.Lock()
		if r.terminal || r.err != nil {
			r.mu.Unlock()
			return
		}
		if n > 0 {
			r.writePending = true
			if r.stream.Write(buffer[:n], false) == 0 {
				r.cancelLocked(errors.New("cronet xhttp: start bidirectional write"))
			}
			for r.writePending && !r.terminal && r.err == nil {
				notify := r.notify
				r.mu.Unlock()
				<-notify
				r.mu.Lock()
			}
		}
		if err != nil || n == 0 {
			if err == nil {
				err = io.ErrNoProgress
			}
			r.cancelLocked(err)
		}
		stop := r.terminal || r.err != nil
		r.mu.Unlock()
		if stop {
			return
		}
	}
}

func (r *browserDuplexRequest) OnStreamReady(BidirectionalStream) {
	r.mu.Lock()
	if !r.ready {
		r.ready = true
		close(r.readyCh)
	}
	r.mu.Unlock()
}

func (r *browserDuplexRequest) OnResponseHeadersReceived(_ BidirectionalStream, headers map[string]string, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if headers[":status"] != "200" {
		r.cancelLocked(fmt.Errorf("cronet xhttp: HTTP status %s", headers[":status"]))
		return
	}
	if r.decoder != nil {
		contentType := headers["content-type"]
		if contentType != "application/grpc" && !strings.HasPrefix(contentType, "application/grpc+") {
			r.cancelLocked(fmt.Errorf("cronet xhttp: unexpected gRPC content type %q", contentType))
			return
		}
		if encoding := headers["grpc-encoding"]; encoding != "" && encoding != "identity" {
			r.cancelLocked(fmt.Errorf("cronet xhttp: unsupported gRPC encoding %q", encoding))
			return
		}
		r.grpcStatus = headers["grpc-status"]
	}
	r.haveHeaders = true
	r.startReadLocked()
}

func (r *browserDuplexRequest) OnResponseTrailersReceived(_ BidirectionalStream, trailers map[string]string) {
	r.mu.Lock()
	if status := trailers["grpc-status"]; status != "" {
		r.grpcStatus = status
	}
	r.mu.Unlock()
}

func (r *browserDuplexRequest) eofLocked() error {
	if r.decoder != nil {
		if r.grpcStatus == "" {
			return errors.New("cronet xhttp: missing gRPC status")
		}
		if r.grpcStatus != "0" {
			return fmt.Errorf("cronet xhttp: gRPC status %s", r.grpcStatus)
		}
	}
	return io.EOF
}

func (r *browserDuplexRequest) OnReadCompleted(_ BidirectionalStream, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readPending = false
	if n == 0 {
		// Response EOF is not a native terminal callback while upload remains
		// open. Cancel and keep both native buffers until OnCanceled arrives.
		r.cancelLocked(r.eofLocked())
		return
	}
	r.pending = append([]byte(nil), r.readBuffer.DataSlice()[:n]...)
	r.signalLocked()
}

func (r *browserDuplexRequest) OnWriteCompleted(BidirectionalStream) {
	r.mu.Lock()
	r.writePending = false
	r.signalLocked()
	r.mu.Unlock()
}

func (r *browserDuplexRequest) finishLocked(err error) {
	if r.terminal {
		return
	}
	r.terminal = true
	if r.err == nil {
		r.err = err
	}
	r.stream.Destroy()
	close(r.done)
	r.signalLocked()
}

func (r *browserDuplexRequest) OnSucceeded(BidirectionalStream) {
	r.mu.Lock()
	r.finishLocked(r.eofLocked())
	r.mu.Unlock()
}

func (r *browserDuplexRequest) OnFailed(_ BidirectionalStream, code int) {
	r.mu.Lock()
	r.finishLocked(NetError(code))
	r.mu.Unlock()
}

func (r *browserDuplexRequest) OnCanceled(BidirectionalStream) {
	r.mu.Lock()
	r.finishLocked(context.Canceled)
	r.mu.Unlock()
}
