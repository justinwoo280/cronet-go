package cronet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2/hpack"
)

const (
	BrowserXHTTPModePacketUp = "packet-up"
	BrowserXHTTPModeStreamUp = "stream-up"

	BrowserXHTTPPlacementPath          = "path"
	BrowserXHTTPPlacementQuery         = "query"
	BrowserXHTTPPlacementHeader        = "header"
	BrowserXHTTPPlacementCookie        = "cookie"
	BrowserXHTTPPlacementQueryInHeader = "query-in-header"
	BrowserXHTTPPlacementBody          = "body"
	BrowserXHTTPPlacementAuto          = "auto"

	BrowserXHTTPPaddingRepeatX  = "repeat-x"
	BrowserXHTTPPaddingTokenish = "tokenish"
)

// BrowserXHTTPRange is an inclusive range. Equal From and To values disable
// randomness while retaining the configured value.
type BrowserXHTTPRange struct {
	From int32
	To   int32
}

// BrowserRealityOptions authenticates a REALITY endpoint using its X25519
// public key and short ID. TLS and HTTP/2 remain inside Cronet.
type BrowserRealityOptions struct {
	PublicKey [32]byte
	ShortID   [8]byte
}

// BrowserXHTTPOptions contains the XHTTP wire-format options supported by the
// Cronet client. Connection-pool and xmux tuning is intentionally absent:
// Cronet owns connection reuse and multiplexing.
type BrowserXHTTPOptions struct {
	Mode   string
	Host   string
	Path   string
	Method string

	Headers http.Header

	NoGRPCHeader bool
	NoSSEHeader  bool

	XPaddingBytes   BrowserXHTTPRange
	XPaddingObfs    bool
	XPaddingPlace   string
	XPaddingKey     string
	XPaddingHeader  string
	XPaddingMethod  string
	SessionPlace    string
	SessionKey      string
	SeqPlace        string
	SeqKey          string
	UplinkPlace     string
	UplinkKey       string
	UplinkChunkSize BrowserXHTTPRange

	SessionIDTable  string
	SessionIDLength BrowserXHTTPRange

	ScMaxEachPostBytes BrowserXHTTPRange
	// ScMaxBufferedPosts bounds concurrent packet POSTs (default 30). Responses
	// may arrive out of order; the XHTTP server reassembles data by sequence.
	ScMaxBufferedPosts   int
	ScMinPostsIntervalMs BrowserXHTTPRange

	// Engine and Executor may be supplied by an embedding application to share
	// a started Cronet engine. Zero values are created and owned by the client.
	Engine    Engine
	Executor  Executor
	Dialer    Dialer
	UserAgent string
	// DisableQUIC is required when only a custom TCP dialer is available.
	DisableQUIC             bool
	TrustedRootCertificates string
	// ECHConfigList is a wire-format ECHConfigList. GetECHConfigList may instead
	// resolve it on demand (and handle DNS TTLs). Both require HTTPS and DialContext.
	// The callback runs before requests start; errors and empty lists fail the dial.
	// ECH is strict: unusable configurations and authenticated rejection without
	// a usable retry configuration fail instead of exposing the inner hostname.
	ECHConfigList    []byte
	GetECHConfigList func(context.Context) ([]byte, error)
	// Reality requires an owned engine and HTTPS. ECH and custom roots are
	// mutually exclusive with REALITY; TLS resumption and 0-RTT are disabled.
	Reality *BrowserRealityOptions
	// DialContext opens the configured endpoint using the embedding application's
	// routing and DNS. Cronet retains the URL hostname for TLS and HTTP authority.
	// It is mutually exclusive with Dialer and a supplied Engine, and disables QUIC.
	DialContext func(context.Context) (net.Conn, error)
}

// BrowserXHTTPClient is a Cronet-backed XHTTP client transport. It implements
// net.Conn sessions for packet-up and stream-up modes.
type BrowserXHTTPClient struct {
	ctx    context.Context
	cancel context.CancelFunc

	engine        Engine
	executor      Executor
	closeEngine   bool
	closeExecutor bool

	baseURL url.URL
	host    string
	opts    BrowserXHTTPOptions
	codec   browserXHTTPCodec

	mu         sync.Mutex
	closed     bool
	conns      map[*browserXHTTPConn]struct{}
	executorWG sync.WaitGroup
	closeDone  chan struct{}
	relayWG    sync.WaitGroup
	echMu      sync.Mutex
	echConfig  []byte
}

var _ interface {
	DialContext(context.Context) (net.Conn, error)
	Close() error
} = (*BrowserXHTTPClient)(nil)

var _ net.Conn = (*browserXHTTPConn)(nil)

// NewBrowserXHTTPClient creates a Cronet-backed XHTTP client for baseURL. The
// scheme must be http or https; TLS and HTTP protocol selection remain inside
// Cronet. A custom Dialer must be installed before the engine is started.
func NewBrowserXHTTPClient(ctx context.Context, baseURL string, options BrowserXHTTPOptions) (*BrowserXHTTPClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("cronet xhttp: parse URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("cronet xhttp: unsupported URL scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("cronet xhttp: URL host is required")
	}
	if options.Mode == "" {
		options.Mode = BrowserXHTTPModePacketUp
	}
	if options.Mode != BrowserXHTTPModePacketUp && options.Mode != BrowserXHTTPModeStreamUp {
		return nil, fmt.Errorf("cronet xhttp: unsupported mode %q", options.Mode)
	}
	if err := validateBrowserOptions(options); err != nil {
		return nil, err
	}
	if (len(options.ECHConfigList) > 0 || options.GetECHConfigList != nil) && (u.Scheme != "https" || net.ParseIP(u.Hostname()) != nil) {
		return nil, errors.New("cronet xhttp: ECH requires an HTTPS URL with a DNS hostname")
	}
	if err := checkLibrary(); err != nil {
		return nil, err
	}
	if options.Reality != nil {
		if u.Scheme != "https" {
			return nil, errors.New("cronet xhttp: REALITY requires HTTPS")
		}
		reality := *options.Reality
		options.Reality = &reality
	}
	options.Headers = cloneHTTPHeaders(options.Headers)
	options.ECHConfigList = append([]byte(nil), options.ECHConfigList...)
	if options.Method == "" {
		options.Method = http.MethodPost
	}
	if options.Path != "" {
		path, query := splitBrowserPath(options.Path)
		u.Path = path
		u.RawQuery = query
	}
	if u.Path == "" {
		u.Path = "/"
	}
	if options.Path == "" {
		options.Path = u.Path
	}

	ctx, cancel := context.WithCancel(ctx)
	c := &BrowserXHTTPClient{
		ctx:       ctx,
		cancel:    cancel,
		baseURL:   *u,
		host:      options.Host,
		opts:      options,
		codec:     newBrowserXHTTPCodec(options),
		conns:     make(map[*browserXHTTPConn]struct{}),
		closeDone: make(chan struct{}),
	}
	if c.host == "" {
		c.host = u.Host
	}

	if options.Engine == (Engine{}) {
		c.engine = NewEngine()
		c.closeEngine = true
		params := NewEngineParams()
		params.SetEnableCheckResult(false)
		params.SetEnableHTTP2(true)
		params.SetEnableQuic(!options.DisableQUIC && options.Dialer == nil && options.DialContext == nil && options.Reality == nil)
		params.SetEnableBrotli(true)
		if options.Reality != nil {
			if err := c.engine.SetReality(options.Reality.PublicKey, options.Reality.ShortID); err != nil {
				params.Destroy()
				c.engine.Destroy()
				cancel()
				return nil, fmt.Errorf("cronet xhttp: enable REALITY: %w", err)
			}
		}
		if c.echEnabled() {
			if err := c.engine.SetStrictECH(true); err != nil {
				params.Destroy()
				c.engine.Destroy()
				cancel()
				return nil, fmt.Errorf("cronet xhttp: enable Strict ECH: %w", err)
			}
		}
		if options.UserAgent != "" {
			params.SetUserAgent(options.UserAgent)
		}
		if options.Dialer != nil {
			c.engine.SetDialer(options.Dialer)
		}
		if options.DialContext != nil {
			// The application resolves the real endpoint. A loopback placeholder
			// keeps Cronet from resolving the SNI name through the system DNS.
			if len(options.ECHConfigList) > 0 || options.GetECHConfigList != nil {
				c.configureECH(params)
			} else {
				_ = params.SetHostResolverRules("MAP * 127.0.0.1")
			}
			c.engine.SetDialer(c.dialSocket)
		}
		if options.TrustedRootCertificates != "" && !c.engine.SetTrustedRootCertificates(options.TrustedRootCertificates) {
			params.Destroy()
			c.engine.Destroy()
			cancel()
			return nil, errors.New("cronet xhttp: set trusted root certificates")
		}
		result := c.engine.StartWithParams(params)
		params.Destroy()
		if result != ResultSuccess {
			c.engine.Destroy()
			cancel()
			return nil, fmt.Errorf("cronet xhttp: start engine: result %d", result)
		}
	} else {
		c.engine = options.Engine
	}

	if options.Executor == (Executor{}) {
		c.executor = NewExecutor(func(_ Executor, command Runnable) {
			c.executorWG.Add(1)
			go func() {
				defer c.executorWG.Done()
				command.Run()
				command.Destroy()
			}()
		})
		c.closeExecutor = true
	} else {
		c.executor = options.Executor
	}
	go func() { <-ctx.Done(); _ = c.Close() }()
	return c, nil
}

// DialContext opens one XHTTP session. The GET is started before the method
// returns and may still be waiting for response headers when the connection is
// handed to the caller; this is required for middleboxes that buffer headers.
func (c *BrowserXHTTPClient) DialContext(ctx context.Context) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.prepareECH(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	// Keep the client lock until the connection is registered. Close must not
	// destroy a client-owned engine while this request is being initialized.
	if err := contextError(ctx); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if err := c.ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	sessionID := c.codec.generateSessionID()
	getURL, getHeaders := c.newRequest(http.MethodGet, sessionID, "")
	get, err := newStreamingRequest(c.engine, c.executor, http.MethodGet, getURL, getHeaders, nil, nil)
	if err != nil {
		c.mu.Unlock()
		cancel()
		return nil, err
	}
	if err = get.start(); err != nil {
		c.mu.Unlock()
		cancel()
		return nil, err
	}

	conn := &browserXHTTPConn{
		client: c,
		reader: get,
		local:  &net.TCPAddr{},
		remote: browserRemoteAddr(c.baseURL),
		cancel: cancel,
		mode:   c.opts.Mode,
		ctx:    ctx,
	}
	conn.setDeadlines(time.Time{}, time.Time{})

	switch c.opts.Mode {
	case BrowserXHTTPModeStreamUp:
		if err := c.startStreamUp(ctx, sessionID, conn); err != nil {
			cancel()
			_ = conn.writer.Close()
			_ = get.close()
			c.mu.Unlock()
			return nil, err
		}
	case BrowserXHTTPModePacketUp:
		c.startPacketUp(ctx, sessionID, conn)
	}

	c.conns[conn] = struct{}{}
	c.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			conn.setError(ctx.Err())
		case <-c.ctx.Done():
			conn.setError(c.ctx.Err())
		case <-get.done:
			get.mu.Lock()
			err := get.err
			get.mu.Unlock()
			// Preserve any buffered response bytes on normal EOF.
			if errors.Is(err, io.EOF) {
				return
			}
			conn.setError(err)
		}
		_ = conn.Close()
	}()
	return conn, nil
}

func (c *BrowserXHTTPClient) startStreamUp(ctx context.Context, sessionID string, conn *browserXHTTPConn) error {
	writer := newBrowserBatch(streamingReadBufferSize)
	reader := &browserBatchReader{batch: writer}
	conn.writer = writer
	upload := &readerUploadProvider{reader: reader}
	postURL, postHeaders := c.newRequest(c.opts.Method, sessionID, "")
	if !c.opts.NoGRPCHeader {
		postHeaders.Set("Content-Type", "application/grpc")
	}
	post, err := newStreamingRequest(c.engine, c.executor, c.opts.Method, postURL, postHeaders, upload, nil)
	if err != nil {
		_ = reader.Close()
		return err
	}
	if err = post.start(); err != nil {
		_ = writer.closeWithError(err)
		return err
	}
	conn.post = post
	conn.uploadDone = make(chan struct{})
	go func() {
		defer close(conn.uploadDone)
		if err := post.wait(ctx); err != nil {
			conn.setError(err)
			_ = writer.closeWithError(err)
			conn.reader.cancel()
		}
	}()
	return nil
}

func (c *BrowserXHTTPClient) startPacketUp(ctx context.Context, sessionID string, conn *browserXHTTPConn) {
	maxEach := int(browserRangeRand(c.codec.maxEachPostBytes))
	if maxEach <= 0 {
		maxEach = 1_000_000
	}
	batch := newBrowserBatch(maxEach)
	conn.writer = batch
	conn.uploadDone = make(chan struct{})
	go func() {
		defer close(conn.uploadDone)
		var uploads sync.WaitGroup
		defer uploads.Wait()
		limit := c.opts.ScMaxBufferedPosts
		if limit == 0 {
			limit = 30
		}
		slots := make(chan struct{}, limit)
		var seq uint64
		var last time.Time
		for {
			if ctx.Err() != nil {
				return
			}
			payload, err := batch.drain()
			if err != nil {
				return
			}
			if delay := browserPostDelay(c.codec.minPostsIntervalMs, last); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
			last = time.Now()
			seqText := strconv.FormatUint(seq, 10)
			seq++
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			post, err := c.startPacket(sessionID, seqText, payload)
			if err != nil {
				conn.setError(err)
				_ = batch.closeWithError(err)
				conn.reader.cancel()
				conn.cancel()
				return
			}
			uploads.Add(1)
			go func() {
				defer uploads.Done()
				defer func() { <-slots }()
				if err := post.wait(ctx); err != nil {
					conn.setError(err)
					_ = batch.closeWithError(err)
					conn.reader.cancel()
					conn.cancel()
				}
			}()
		}
	}()
}

func (c *BrowserXHTTPClient) startPacket(sessionID, seq string, payload []byte) (*streamingRequest, error) {
	postURL, headers := c.newRequest(c.opts.Method, sessionID, seq)
	postURLValue, err := url.Parse(postURL)
	if err != nil {
		return nil, err
	}
	if c.codec.encodeUplink(headers, postURLValue, payload) {
		postURL = postURLValue.String()
		payload = nil
	}
	var upload UploadDataProviderHandler
	if len(payload) > 0 {
		upload = &fixedUploadProvider{data: payload}
	}
	post, err := newStreamingRequest(c.engine, c.executor, c.opts.Method, postURL, headers, upload, nil)
	if err != nil {
		return nil, err
	}
	if err = post.start(); err != nil {
		return nil, err
	}
	return post, nil
}

func (c *BrowserXHTTPClient) newRequest(method, sessionID, seq string) (string, http.Header) {
	u := c.baseURL
	u.Path = c.codec.basePath
	u.RawQuery = c.baseURL.RawQuery
	headers := cloneHTTPHeaders(c.opts.Headers)
	if c.host != "" {
		headers.Set("Host", c.host)
	}
	// NoSSEHeader controls the server's response, not a GET request header.
	c.codec.applyMeta(&u, headers, sessionID, seq)
	c.codec.applyPadding(&u, headers)
	return u.String(), headers
}

func (c *BrowserXHTTPClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone
		return nil
	}
	c.closed = true
	conns := make([]*browserXHTTPConn, 0, len(c.conns))
	for conn := range c.conns {
		conns = append(conns, conn)
	}
	c.mu.Unlock()
	c.cancel()
	for _, conn := range conns {
		_ = conn.Close()
	}
	if c.closeEngine {
		c.engine.Shutdown()
		c.engine.Destroy()
	}
	c.relayWG.Wait()
	if c.closeExecutor {
		c.executorWG.Wait()
		c.executor.Destroy()
	}
	close(c.closeDone)
	return nil
}

func (c *BrowserXHTTPClient) removeConn(conn *browserXHTTPConn) {
	c.mu.Lock()
	delete(c.conns, conn)
	c.mu.Unlock()
}

type browserXHTTPConn struct {
	client     *BrowserXHTTPClient
	reader     *streamingRequest
	post       *streamingRequest
	writer     io.WriteCloser
	local      net.Addr
	remote     net.Addr
	cancel     context.CancelFunc
	mode       string
	ctx        context.Context
	uploadDone chan struct{}

	mu                      sync.Mutex
	closeOnce               sync.Once
	err                     error
	readDeadline            time.Time
	writeDeadline           time.Time
	readCtx, writeCtx       context.Context
	readCancel, writeCancel context.CancelFunc
	writeMu                 sync.Mutex
}

func (c *browserXHTTPConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		ctx := c.readCtx
		err := c.err
		c.mu.Unlock()
		if err != nil {
			return 0, err
		}
		n, err := c.reader.readContext(ctx, p)
		if err != nil {
			c.mu.Lock()
			if c.err != nil {
				err = c.err
			}
			c.mu.Unlock()
		}
		if errors.Is(err, context.Canceled) && c.ctx.Err() == nil {
			continue
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return n, &timeoutError{}
		}
		return n, err
	}
}

func (c *browserXHTTPConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	total := 0
	for {
		c.mu.Lock()
		ctx := c.writeCtx
		err := c.err
		c.mu.Unlock()
		if err != nil {
			return total, err
		}
		n, err := c.writer.(*browserBatch).writeContext(ctx, p[total:])
		total += n
		if err != nil {
			c.mu.Lock()
			if c.err != nil {
				err = c.err
			}
			c.mu.Unlock()
		}
		if errors.Is(err, context.Canceled) && c.ctx.Err() == nil {
			continue
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return total, &timeoutError{}
		}
		return total, err
	}
}

func (c *browserXHTTPConn) Close() error {
	c.closeOnce.Do(func() {
		c.setError(net.ErrClosed)
		c.cancel()
		c.mu.Lock()
		c.readCancel()
		c.writeCancel()
		c.mu.Unlock()
		if c.writer != nil {
			_ = c.writer.Close()
		}
		if c.post != nil {
			_ = c.post.close()
		}
		if c.reader != nil {
			_ = c.reader.close()
		}
		if c.uploadDone != nil {
			<-c.uploadDone
		}
		c.client.removeConn(c)
	})
	return nil
}

func (c *browserXHTTPConn) LocalAddr() net.Addr  { return c.local }
func (c *browserXHTTPConn) RemoteAddr() net.Addr { return c.remote }

func (c *browserXHTTPConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.writeDeadline = t
	c.setDeadlines(t, t)
	c.mu.Unlock()
	return nil
}

func (c *browserXHTTPConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.resetReadDeadline(t)
	c.mu.Unlock()
	return nil
}

func (c *browserXHTTPConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.resetWriteDeadline(t)
	c.mu.Unlock()
	return nil
}

func (c *browserXHTTPConn) setDeadlines(read, write time.Time) {
	c.resetReadDeadline(read)
	c.resetWriteDeadline(write)
}
func (c *browserXHTTPConn) resetReadDeadline(t time.Time) {
	if c.readCancel != nil {
		c.readCancel()
	}
	if t.IsZero() {
		c.readCtx, c.readCancel = context.WithCancel(c.ctx)
	} else {
		c.readCtx, c.readCancel = context.WithDeadline(c.ctx, t)
	}
}
func (c *browserXHTTPConn) resetWriteDeadline(t time.Time) {
	if c.writeCancel != nil {
		c.writeCancel()
	}
	if t.IsZero() {
		c.writeCtx, c.writeCancel = context.WithCancel(c.ctx)
	} else {
		c.writeCtx, c.writeCancel = context.WithDeadline(c.ctx, t)
	}
}

func (c *browserXHTTPConn) setError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

type browserBatch struct {
	mu       sync.Mutex
	notify   chan struct{}
	data     []byte
	max      int
	closed   bool
	closeErr error
}

func newBrowserBatch(max int) *browserBatch {
	if max < 1 {
		max = 1
	}
	b := &browserBatch{max: max, data: make([]byte, 0, max), notify: make(chan struct{})}
	return b
}

func (b *browserBatch) Write(p []byte) (int, error) {
	return b.writeContext(context.Background(), p)
}

func (b *browserBatch) writeContext(ctx context.Context, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		if err := contextError(ctx); err != nil {
			return total, err
		}
		b.mu.Lock()
		if b.closed {
			err := b.closeErr
			b.mu.Unlock()
			if err != nil {
				return total, err
			}
			return total, io.ErrClosedPipe
		}
		if len(b.data) < b.max {
			n := min(b.max-len(b.data), len(p)-total)
			b.data = append(b.data, p[total:total+n]...)
			total += n
			b.signalLocked()
			b.mu.Unlock()
			continue
		}
		wait := b.notify
		b.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return total, ctx.Err()
		}
	}
	return total, nil
}

type browserBatchReader struct {
	batch   *browserBatch
	pending []byte
}

func (r *browserBatchReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		var err error
		r.pending, err = r.batch.drain()
		if err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
func (r *browserBatchReader) Close() error { return r.batch.closeWithError(io.ErrClosedPipe) }

func (b *browserBatch) drain() ([]byte, error) {
	for {
		b.mu.Lock()
		if len(b.data) > 0 {
			n := len(b.data)
			if n > b.max {
				n = b.max
			}
			out := append([]byte(nil), b.data[:n]...)
			copy(b.data, b.data[n:])
			b.data = b.data[:len(b.data)-n]
			b.signalLocked()
			b.mu.Unlock()
			return out, nil
		}
		if b.closed {
			b.mu.Unlock()
			return nil, io.EOF
		}
		wait := b.notify
		b.mu.Unlock()
		<-wait
	}
}

func (b *browserBatch) Close() error { return b.closeWithError(nil) }

func (b *browserBatch) closeWithError(err error) error {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		b.closeErr = err
		b.signalLocked()
	}
	b.mu.Unlock()
	return nil
}

func (b *browserBatch) signalLocked() {
	close(b.notify)
	b.notify = make(chan struct{})
}

type browserXHTTPCodec struct {
	basePath                                       string
	sessionPlace, sessionKey                       string
	seqPlace, seqKey                               string
	xpadObfs                                       bool
	xpadPlacement, xpadKey, xpadHeader, xpadMethod string
	xpadRange                                      BrowserXHTTPRange
	uplinkPlace, uplinkKey                         string
	uplinkChunkSize                                BrowserXHTTPRange
	maxEachPostBytes                               BrowserXHTTPRange
	minPostsIntervalMs                             BrowserXHTTPRange
	sessionTable                                   string
	sessionLength                                  BrowserXHTTPRange
}

func newBrowserXHTTPCodec(o BrowserXHTTPOptions) browserXHTTPCodec {
	c := browserXHTTPCodec{
		basePath:           normalizeBrowserPath(o.Path),
		sessionPlace:       o.SessionPlace,
		sessionKey:         o.SessionKey,
		seqPlace:           o.SeqPlace,
		seqKey:             o.SeqKey,
		xpadObfs:           o.XPaddingObfs,
		xpadPlacement:      o.XPaddingPlace,
		xpadKey:            o.XPaddingKey,
		xpadHeader:         o.XPaddingHeader,
		xpadMethod:         o.XPaddingMethod,
		xpadRange:          defaultBrowserRange(o.XPaddingBytes, BrowserXHTTPRange{100, 1000}),
		uplinkPlace:        o.UplinkPlace,
		uplinkKey:          o.UplinkKey,
		uplinkChunkSize:    o.UplinkChunkSize,
		maxEachPostBytes:   defaultBrowserRange(o.ScMaxEachPostBytes, BrowserXHTTPRange{1_000_000, 1_000_000}),
		minPostsIntervalMs: defaultBrowserRange(o.ScMinPostsIntervalMs, BrowserXHTTPRange{30, 30}),
		sessionTable:       o.SessionIDTable,
		sessionLength:      o.SessionIDLength,
	}
	if c.sessionPlace == "" {
		c.sessionPlace = BrowserXHTTPPlacementPath
	}
	if c.seqPlace == "" {
		c.seqPlace = BrowserXHTTPPlacementPath
	}
	if c.sessionKey == "" {
		c.sessionKey = browserDefaultKey(c.sessionPlace, "X-Session")
	}
	if c.seqKey == "" {
		c.seqKey = browserDefaultKey(c.seqPlace, "X-Seq")
	}
	if c.uplinkPlace == "" {
		c.uplinkPlace = BrowserXHTTPPlacementBody
	}
	if c.xpadObfs {
		if c.xpadPlacement == "" {
			c.xpadPlacement = BrowserXHTTPPlacementHeader
		}
		if c.xpadKey == "" {
			c.xpadKey = "x_padding"
		}
		if c.xpadHeader == "" {
			c.xpadHeader = "X-Padding"
		}
		if c.xpadMethod == "" {
			c.xpadMethod = BrowserXHTTPPaddingRepeatX
		}
	}
	if c.uplinkChunkSize.To == 0 {
		if c.uplinkPlace == BrowserXHTTPPlacementCookie {
			c.uplinkChunkSize = BrowserXHTTPRange{2048, 3072}
		} else if c.uplinkPlace == BrowserXHTTPPlacementHeader {
			c.uplinkChunkSize = BrowserXHTTPRange{3000, 4000}
		} else {
			c.uplinkChunkSize = c.maxEachPostBytes
		}
	}
	c.uplinkChunkSize.From = max(64, c.uplinkChunkSize.From)
	c.uplinkChunkSize.To = max(c.uplinkChunkSize.From, c.uplinkChunkSize.To)
	return c
}

func (c browserXHTTPCodec) applyMeta(u *url.URL, h http.Header, sessionID, seq string) {
	if sessionID != "" {
		browserApplyField(u, h, c.sessionPlace, c.sessionKey, sessionID)
	}
	if seq != "" {
		browserApplyField(u, h, c.seqPlace, c.seqKey, seq)
	}
}

func browserApplyField(u *url.URL, h http.Header, place, key, value string) {
	switch place {
	case BrowserXHTTPPlacementPath:
		u.Path = browserAppendPath(u.Path, value)
	case BrowserXHTTPPlacementQuery:
		q := u.Query()
		q.Set(key, value)
		u.RawQuery = q.Encode()
	case BrowserXHTTPPlacementHeader:
		h.Set(key, value)
	case BrowserXHTTPPlacementCookie:
		browserAddCookie(h, key, value)
	}
}

func (c browserXHTTPCodec) applyPadding(u *url.URL, h http.Header) {
	n := int(browserRangeRand(c.xpadRange))
	if n <= 0 {
		return
	}
	if !c.xpadObfs {
		ref := *u
		q := ref.Query()
		q.Set("x_padding", browserGeneratePadding(BrowserXHTTPPaddingRepeatX, n))
		ref.RawQuery = q.Encode()
		h.Set("Referer", ref.String())
		return
	}
	v := browserGeneratePadding(c.xpadMethod, n)
	switch c.xpadPlacement {
	case BrowserXHTTPPlacementHeader:
		h.Set(c.xpadHeader, v)
	case BrowserXHTTPPlacementQuery:
		q := u.Query()
		q.Set(c.xpadKey, v)
		u.RawQuery = q.Encode()
	case BrowserXHTTPPlacementCookie:
		browserAddCookie(h, c.xpadKey, v)
	case BrowserXHTTPPlacementQueryInHeader:
		ref := *u
		q := ref.Query()
		q.Set(c.xpadKey, v)
		ref.RawQuery = q.Encode()
		h.Set(c.xpadHeader, ref.String())
	}
}

func (c browserXHTTPCodec) encodeUplink(h http.Header, u *url.URL, payload []byte) bool {
	if c.uplinkPlace != BrowserXHTTPPlacementHeader && c.uplinkPlace != BrowserXHTTPPlacementCookie {
		return false
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	for i := 0; len(encoded) > 0; i++ {
		chunk := int(browserRangeRand(c.uplinkChunkSize))
		if chunk <= 0 {
			chunk = len(encoded)
		}
		n := chunk
		if n > len(encoded) {
			n = len(encoded)
		}
		if c.uplinkPlace == BrowserXHTTPPlacementHeader {
			h.Set(fmt.Sprintf("%s-%d", c.uplinkKey, i), encoded[:n])
		} else {
			browserAddCookie(h, fmt.Sprintf("%s_%d", c.uplinkKey, i), encoded[:n])
		}
		encoded = encoded[n:]
	}
	return true
}

func (c browserXHTTPCodec) generateSessionID() string {
	if c.sessionTable != "" {
		table := c.sessionTable
		if predefined, ok := browserPredefinedTables[table]; ok {
			table = predefined
		}
		n := int(c.sessionLength.From)
		if c.sessionLength.To > c.sessionLength.From {
			n = int(browserRangeRand(c.sessionLength))
		}
		if n > 0 && len(table) > 0 {
			out := make([]byte, n)
			random := make([]byte, n)
			if _, err := rand.Read(random); err == nil {
				for i, b := range random {
					out[i] = table[int(b)%len(table)]
				}
				return string(out)
			}
		}
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	var text [36]byte
	hex.Encode(text[0:8], raw[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], raw[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], raw[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], raw[8:10])
	text[23] = '-'
	hex.Encode(text[24:36], raw[10:16])
	return string(text[:])
}

var browserPredefinedTables = map[string]string{"ALPHABET": "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "Alphabet": "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", "BASE36": "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ", "Base62": "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", "HEX": "0123456789ABCDEF", "alphabet": "abcdefghijklmnopqrstuvwxyz", "base36": "0123456789abcdefghijklmnopqrstuvwxyz", "hex": "0123456789abcdef", "number": "0123456789"}

func cloneHTTPHeaders(h http.Header) http.Header {
	if h == nil {
		return make(http.Header)
	}
	return h.Clone()
}
func normalizeBrowserPath(p string) string {
	if p == "" {
		return "/"
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}
func splitBrowserPath(p string) (string, string) {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}
func browserAppendPath(p, segment string) string {
	if strings.HasSuffix(p, "/") {
		return p + segment
	}
	return p + "/" + segment
}
func browserDefaultKey(place, header string) string {
	if place == BrowserXHTTPPlacementHeader {
		return header
	}
	if place == BrowserXHTTPPlacementPath {
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(header, "-", "_"))
}
func browserAddCookie(h http.Header, name, value string) {
	old := h.Get("Cookie")
	item := name + "=" + value
	if old == "" {
		h.Set("Cookie", item)
	} else {
		h.Set("Cookie", old+"; "+item)
	}
}
func defaultBrowserRange(v, d BrowserXHTTPRange) BrowserXHTTPRange {
	if v.To == 0 {
		return d
	}
	return v
}
func browserRangeRand(r BrowserXHTTPRange) int32 {
	if r.To <= r.From {
		return r.From
	}
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	n := uint64(binary.LittleEndian.Uint32(raw[:]))
	return int32(int64(r.From) + int64(n%uint64(int64(r.To)-int64(r.From)+1)))
}
func browserGeneratePadding(method string, length int) string {
	if length <= 0 {
		return ""
	}
	if method != BrowserXHTTPPaddingTokenish {
		return strings.Repeat("X", length)
	}
	const table = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 0, length*2)
	var raw [1]byte
	for int(hpack.HuffmanEncodeLength(string(out))) < length {
		if _, err := rand.Read(raw[:]); err != nil {
			out = append(out, 'X')
		} else {
			out = append(out, table[int(raw[0])%len(table)])
		}
	}
	return string(out)
}
func browserPostDelay(r BrowserXHTTPRange, last time.Time) time.Duration {
	if last.IsZero() {
		return 0
	}
	delay := time.Duration(browserRangeRand(r))*time.Millisecond - time.Since(last)
	if delay < 0 {
		return 0
	}
	return delay
}
func browserRemoteAddr(u url.URL) net.Addr {
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Host
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, _ := strconv.Atoi(port)
	return &net.TCPAddr{IP: net.ParseIP(host), Port: n}
}
func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func validateBrowserOptions(o BrowserXHTTPOptions) error {
	if o.Reality != nil {
		if o.Engine != (Engine{}) {
			return errors.New("cronet xhttp: REALITY requires an owned engine")
		}
		if len(o.ECHConfigList) != 0 || o.GetECHConfigList != nil || o.TrustedRootCertificates != "" {
			return errors.New("cronet xhttp: REALITY is incompatible with ECH and custom roots")
		}
	}
	if len(o.ECHConfigList) > 0 || o.GetECHConfigList != nil {
		if o.DialContext == nil {
			return errors.New("cronet xhttp: ECH requires DialContext")
		}
		if len(o.ECHConfigList) > 0 && o.GetECHConfigList != nil {
			return errors.New("cronet xhttp: ECHConfigList and GetECHConfigList are mutually exclusive")
		}
	}
	if o.DialContext != nil && (o.Dialer != nil || o.Engine != (Engine{})) {
		return errors.New("cronet xhttp: DialContext requires an owned engine and no Dialer")
	}
	for name, r := range map[string]BrowserXHTTPRange{
		"x_padding_bytes": o.XPaddingBytes, "uplink_chunk_size": o.UplinkChunkSize,
		"session_id_length": o.SessionIDLength, "sc_max_each_post_bytes": o.ScMaxEachPostBytes,
		"sc_min_posts_interval_ms": o.ScMinPostsIntervalMs,
	} {
		if r.From < 0 || r.To < r.From {
			return fmt.Errorf("cronet xhttp: invalid %s range", name)
		}
	}
	for name, place := range map[string]string{"session": o.SessionPlace, "seq": o.SeqPlace} {
		switch place {
		case "", BrowserXHTTPPlacementPath, BrowserXHTTPPlacementQuery, BrowserXHTTPPlacementHeader, BrowserXHTTPPlacementCookie:
		default:
			return fmt.Errorf("cronet xhttp: invalid %s placement %q", name, place)
		}
	}
	switch o.UplinkPlace {
	case "", BrowserXHTTPPlacementBody, BrowserXHTTPPlacementAuto:
	case BrowserXHTTPPlacementHeader, BrowserXHTTPPlacementCookie:
		if o.Mode == BrowserXHTTPModeStreamUp {
			return errors.New("cronet xhttp: stream-up requires body uplink placement")
		}
		if o.UplinkKey == "" {
			return errors.New("cronet xhttp: uplink key is required for header/cookie placement")
		}
	default:
		return fmt.Errorf("cronet xhttp: invalid uplink placement %q", o.UplinkPlace)
	}
	if o.XPaddingObfs {
		switch o.XPaddingPlace {
		case "", BrowserXHTTPPlacementHeader, BrowserXHTTPPlacementQuery, BrowserXHTTPPlacementCookie, BrowserXHTTPPlacementQueryInHeader:
		default:
			return fmt.Errorf("cronet xhttp: invalid padding placement %q", o.XPaddingPlace)
		}
		if o.XPaddingMethod != "" && o.XPaddingMethod != BrowserXHTTPPaddingRepeatX && o.XPaddingMethod != BrowserXHTTPPaddingTokenish {
			return fmt.Errorf("cronet xhttp: invalid padding method %q", o.XPaddingMethod)
		}
	}
	if o.ScMaxBufferedPosts < 0 {
		return errors.New("cronet xhttp: negative buffered posts")
	}
	return nil
}
