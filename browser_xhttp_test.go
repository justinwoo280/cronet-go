package cronet

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

func TestBrowserXHTTPCodecMetadataPlacements(t *testing.T) {
	c := newBrowserXHTTPCodec(BrowserXHTTPOptions{
		Path:         "/xhttp",
		SessionPlace: BrowserXHTTPPlacementQuery,
		SeqPlace:     BrowserXHTTPPlacementCookie,
	})
	u, err := url.Parse("https://example.com/xhttp/")
	if err != nil {
		t.Fatal(err)
	}
	h := make(http.Header)
	c.applyMeta(u, h, "session", "7")
	if got := u.Query().Get("x_session"); got != "session" {
		t.Fatalf("session query = %q", got)
	}
	if got := h.Get("Cookie"); got != "x_seq=7" {
		t.Fatalf("seq cookie = %q", got)
	}
}

func TestBrowserXHTTPCodecPadding(t *testing.T) {
	u, err := url.Parse("https://example.com/xhttp/session")
	if err != nil {
		t.Fatal(err)
	}
	c := newBrowserXHTTPCodec(BrowserXHTTPOptions{
		XPaddingBytes: BrowserXHTTPRange{From: 12, To: 12},
	})
	h := make(http.Header)
	c.applyPadding(u, h)
	ref, err := url.Parse(h.Get("Referer"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ref.Query().Get("x_padding"); len(got) != 12 || strings.Trim(got, "X") != "" {
		t.Fatalf("default padding = %q", got)
	}

	u, _ = url.Parse("https://example.com/xhttp")
	c = newBrowserXHTTPCodec(BrowserXHTTPOptions{
		XPaddingObfs:   true,
		XPaddingPlace:  BrowserXHTTPPlacementHeader,
		XPaddingBytes:  BrowserXHTTPRange{From: 9, To: 9},
		XPaddingHeader: "X-Test-Padding",
		XPaddingMethod: BrowserXHTTPPaddingTokenish,
	})
	h = make(http.Header)
	c.applyPadding(u, h)
	if got := h.Get("X-Test-Padding"); hpack.HuffmanEncodeLength(got) != 9 {
		t.Fatalf("obfuscated padding = %q", got)
	}
}

func TestBrowserXHTTPSessionID(t *testing.T) {
	c := newBrowserXHTTPCodec(BrowserXHTTPOptions{
		SessionIDTable:  "number",
		SessionIDLength: BrowserXHTTPRange{From: 16, To: 16},
	})
	id := c.generateSessionID()
	if len(id) != 16 {
		t.Fatalf("session ID length = %d", len(id))
	}
	if strings.Trim(id, "0123456789") != "" {
		t.Fatalf("session ID contains invalid characters: %q", id)
	}
}

func TestBrowserBatchDrainAndClose(t *testing.T) {
	b := newBrowserBatch(3)
	if n, err := b.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("first write = %d, %v", n, err)
	}
	drained := make(chan []byte, 1)
	go func() {
		chunk, _ := b.drain()
		drained <- chunk
	}()
	select {
	case chunk := <-drained:
		if string(chunk) != "abc" {
			t.Fatalf("drained chunk = %q", chunk)
		}
	case <-time.After(time.Second):
		t.Fatal("batch drain blocked")
	}
	if _, err := b.Write([]byte("d")); err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("closed")
	if err := b.closeWithError(closeErr); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("e")); !errors.Is(err, closeErr) {
		t.Fatalf("write after close = %v", err)
	}
	chunk, err := b.drain()
	if err != nil || string(chunk) != "d" {
		t.Fatalf("remaining drain = %q, %v", chunk, err)
	}
	if _, err := b.drain(); !errors.Is(err, io.EOF) {
		t.Fatalf("final drain = %v", err)
	}
}

func TestBrowserBatchWriteContext(t *testing.T) {
	b := newBrowserBatch(1)
	if _, err := b.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.writeContext(ctx, []byte("b")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write context error = %v", err)
	}
}

func TestBrowserWriteDeadlineInterruptAndReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batch := newBrowserBatch(3)
	conn := &browserXHTTPConn{ctx: ctx, writer: batch}
	conn.setDeadlines(time.Time{}, time.Time{})
	result := make(chan struct {
		n   int
		err error
	}, 1)
	go func() {
		n, err := conn.Write([]byte("abcdef"))
		result <- struct {
			n   int
			err error
		}{n, err}
	}()
	_ = conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	select {
	case r := <-result:
		if r.n != 3 {
			t.Fatalf("partial write = %d", r.n)
		}
		if e, ok := r.err.(interface{ Timeout() bool }); !ok || !e.Timeout() {
			t.Fatalf("error = %v", r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("write deadline did not wake writer")
	}
	if data, _ := batch.drain(); string(data) != "abc" {
		t.Fatalf("accepted bytes = %q", data)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	if n, err := conn.Write([]byte("def")); n != 3 || err != nil {
		t.Fatalf("write after reset = %d, %v", n, err)
	}
}
