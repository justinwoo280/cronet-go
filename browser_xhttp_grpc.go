package cronet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	browserGRPCSessionHeader = "X-Xhttp-Session"
	browserMaxGRPCMessage    = 4 << 20
)

func browserGRPCPath(basePath string) string {
	service := strings.Trim(basePath, "/")
	if service == "" {
		service = "xhttp"
	}
	return "/" + service + "/Tun"
}

func validBrowserGRPCPath(path string) bool {
	if path == "" || path == "/" {
		return true
	}
	service := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/")
	for _, part := range strings.Split(service, ".") {
		if part == "" {
			return false
		}
		for i, ch := range part {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch == '_' || i > 0 && ch >= '0' && ch <= '9') {
				return false
			}
		}
	}
	return true
}

// Encode after the application's deadline-aware upload queue. A Write timeout
// can then report accepted payload bytes without leaving a partial gRPC frame.
type browserGRPCUpload struct {
	source  io.ReadCloser
	buffer  []byte
	pending []byte
	err     error
}

func newBrowserGRPCUpload(source io.ReadCloser) *browserGRPCUpload {
	return &browserGRPCUpload{source: source, buffer: make([]byte, streamingReadBufferSize)}
}

func (u *browserGRPCUpload) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(u.pending) == 0 {
		if u.err != nil {
			return 0, u.err
		}
		n, err := u.source.Read(u.buffer)
		u.err = err
		if n == 0 {
			return 0, err
		}
		messageSize := 1 + protowire.SizeBytes(n)
		u.pending = make([]byte, 5, 5+messageSize)
		binary.BigEndian.PutUint32(u.pending[1:], uint32(messageSize))
		u.pending = protowire.AppendTag(u.pending, 1, protowire.BytesType)
		u.pending = protowire.AppendBytes(u.pending, u.buffer[:n])
	}
	n := copy(p, u.pending)
	u.pending = u.pending[n:]
	return n, nil
}

func (u *browserGRPCUpload) Close() error { return u.source.Close() }

// Preserve partial headers and messages across read-deadline changes. A timed
// out Read must not reinterpret the rest of that message as a new gRPC header.
type browserGRPCReader struct {
	read    func(context.Context, []byte) (int, error)
	mu      sync.Mutex
	header  [5]byte
	headerN int
	frame   []byte
	frameN  int
	cache   []byte
	err     error
}

func (r *browserGRPCReader) readContext(ctx context.Context, p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for len(r.cache) == 0 {
		err := r.readMessage(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				r.err = err
			}
			return 0, err
		}
	}
	n := copy(p, r.cache)
	r.cache = r.cache[n:]
	return n, nil
}

func (r *browserGRPCReader) fill(ctx context.Context, p []byte, have *int) error {
	for *have < len(p) {
		n, err := r.read(ctx, p[*have:])
		*have += n
		if *have == len(p) {
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (r *browserGRPCReader) readMessage(ctx context.Context) error {
	if err := r.fill(ctx, r.header[:], &r.headerN); err != nil {
		if err == io.EOF && r.headerN != 0 {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if r.header[0] != 0 {
		return errors.New("cronet xhttp: compressed gRPC messages are unsupported")
	}
	size := binary.BigEndian.Uint32(r.header[1:])
	if size > browserMaxGRPCMessage {
		return errors.New("cronet xhttp: gRPC message exceeds maximum size")
	}
	if cap(r.frame) < int(size) {
		r.frame = make([]byte, int(size))
	} else {
		r.frame = r.frame[:int(size)]
	}
	if err := r.fill(ctx, r.frame, &r.frameN); err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	message := r.frame
	for len(message) > 0 {
		number, wireType, n := protowire.ConsumeTag(message)
		if n < 0 {
			return fmt.Errorf("cronet xhttp: invalid gRPC protobuf tag: %w", protowire.ParseError(n))
		}
		message = message[n:]
		if number == 1 && wireType == protowire.BytesType {
			r.cache, n = protowire.ConsumeBytes(message)
		} else {
			n = protowire.ConsumeFieldValue(number, wireType, message)
		}
		if n < 0 {
			return fmt.Errorf("cronet xhttp: invalid gRPC protobuf field: %w", protowire.ParseError(n))
		}
		message = message[n:]
	}
	r.headerN, r.frameN = 0, 0
	return nil
}

func (r *browserGRPCReader) release() {
	r.mu.Lock()
	r.frame, r.cache = nil, nil
	if r.err == nil {
		r.err = io.ErrClosedPipe
	}
	r.mu.Unlock()
}
