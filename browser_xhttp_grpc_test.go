package cronet

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// BytesValue uses the same field number and wire type as Hunk.data, allowing
// tests to check interoperability with a generated protobuf implementation.
func browserTestGRPCFrame(payload []byte) []byte {
	message, err := proto.Marshal(wrapperspb.Bytes(payload))
	if err != nil {
		panic(err)
	}
	return browserTestGRPCWire(message)
}

func browserTestGRPCWire(message []byte) []byte {
	wire := make([]byte, 5, 5+len(message))
	binary.BigEndian.PutUint32(wire[1:], uint32(len(message)))
	return append(wire, message...)
}

func browserTestReadGRPC(r io.Reader) ([]byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	if header[0] != 0 || binary.BigEndian.Uint32(header[1:]) > browserMaxGRPCMessage {
		return nil, errors.New("invalid gRPC header")
	}
	message := make([]byte, binary.BigEndian.Uint32(header[1:]))
	if _, err := io.ReadFull(r, message); err != nil {
		return nil, err
	}
	var value wrapperspb.BytesValue
	if err := proto.Unmarshal(message, &value); err != nil {
		return nil, err
	}
	return value.Value, nil
}

func TestBrowserGRPCUploadProtobufInterop(t *testing.T) {
	payload := bytes.Repeat([]byte("protobuf payload\n"), 300000)
	upload := newBrowserGRPCUpload(io.NopCloser(bytes.NewReader(payload)))
	defer upload.Close()
	var got []byte
	var messages int
	for {
		chunk, err := browserTestReadGRPC(upload)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		messages++
		got = append(got, chunk...)
	}
	if messages < 2 || !bytes.Equal(got, payload) {
		t.Fatalf("%d messages, %d/%d payload bytes", messages, len(got), len(payload))
	}
}

func TestBrowserGRPCReadDeadlineResume(t *testing.T) {
	for _, offset := range []int{2, 5, 8} {
		t.Run(string(rune('0'+offset)), func(t *testing.T) {
			payload := []byte("response after resetting deadline")
			wire := browserTestGRPCFrame(payload)
			position, resumed := 0, false
			reader := &browserGRPCReader{read: func(ctx context.Context, p []byte) (int, error) {
				if position == offset && !resumed {
					<-ctx.Done()
					resumed = true
					return 0, ctx.Err()
				}
				end := len(wire)
				if !resumed {
					end = offset
				}
				if position == end {
					return 0, io.EOF
				}
				n := copy(p, wire[position:end])
				position += n
				return n, nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			got := make([]byte, len(payload))
			if n, err := reader.readContext(ctx, got); n != 0 || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline read = %d, %v", n, err)
			}
			if n, err := reader.readContext(context.Background(), got); n != len(payload) || err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("resumed read = %q, %d, %v", got, n, err)
			}
		})
	}
}

func TestBrowserGRPCReaderProtobufSemantics(t *testing.T) {
	// Empty messages, unknown fields, and repeated bytes fields are all legal.
	wire := browserTestGRPCFrame(nil)
	wire = append(wire, browserTestGRPCWire([]byte{0x10, 42, 0x0a, 1, 'a', 0x0a, 1, 'b'})...)
	source := bytes.NewReader(wire)
	reader := &browserGRPCReader{read: func(_ context.Context, p []byte) (int, error) { return source.Read(p) }}
	got := make([]byte, 16)
	if n, err := reader.readContext(context.Background(), got); n != 1 || err != nil || got[0] != 'b' {
		t.Fatalf("protobuf result = %q, %d, %v", got, n, err)
	}
	if n, err := reader.readContext(context.Background(), got); n != 0 || err != io.EOF {
		t.Fatalf("EOF = %d, %v", n, err)
	}
}

func TestBrowserGRPCMalformedMessageIsSticky(t *testing.T) {
	oversized := make([]byte, 5)
	binary.BigEndian.PutUint32(oversized[1:], browserMaxGRPCMessage+1)
	for name, wire := range map[string][]byte{
		"compressed":         {1, 0, 0, 0, 0},
		"oversized":          oversized,
		"truncated header":   {0, 0},
		"truncated message":  {0, 0, 0, 0, 2, 0x0a},
		"truncated protobuf": browserTestGRPCWire([]byte{0x0a, 3, 'a'}),
		"invalid after data": browserTestGRPCWire([]byte{0x0a, 1, 'a', 0x0f}),
	} {
		t.Run(name, func(t *testing.T) {
			source := bytes.NewReader(wire)
			reader := &browserGRPCReader{read: func(_ context.Context, p []byte) (int, error) { return source.Read(p) }}
			got := make([]byte, 16)
			n, err := reader.readContext(context.Background(), got)
			if n != 0 || err == nil || err == io.EOF {
				t.Fatalf("malformed read = %d, %v", n, err)
			}
			if nextN, nextErr := reader.readContext(context.Background(), got); nextN != 0 || nextErr != err {
				t.Fatalf("read after failure = %d, %v; want %v", nextN, nextErr, err)
			}
		})
	}
}

func TestBrowserGRPCConfiguration(t *testing.T) {
	base := BrowserXHTTPOptions{Mode: BrowserXHTTPModeStreamOne, GRPCFraming: true}
	for _, path := range []string{"", "/", "xhttp", "/xhttp", "/package.Service/", "/_service"} {
		options := base
		options.Path = path
		if err := validateBrowserOptions(options); err != nil {
			t.Fatalf("path %q: %v", path, err)
		}
	}
	for name, mutate := range map[string]func(*BrowserXHTTPOptions){
		"packet":       func(o *BrowserXHTTPOptions) { o.Mode = BrowserXHTTPModePacketUp },
		"method":       func(o *BrowserXHTTPOptions) { o.Method = http.MethodPut },
		"no headers":   func(o *BrowserXHTTPOptions) { o.NoGRPCHeader = true },
		"query":        func(o *BrowserXHTTPOptions) { o.Path = "/service?query=1" },
		"method path":  func(o *BrowserXHTTPOptions) { o.Path = "/service/Tun" },
		"service name": func(o *BrowserXHTTPOptions) { o.Path = "/1service" },
		"uplink":       func(o *BrowserXHTTPOptions) { o.UplinkPlace = BrowserXHTTPPlacementHeader },
		"padding": func(o *BrowserXHTTPOptions) {
			o.XPaddingObfs, o.XPaddingPlace = true, BrowserXHTTPPlacementQuery
		},
	} {
		t.Run(name, func(t *testing.T) {
			options := base
			mutate(&options)
			if err := validateBrowserOptions(options); err == nil {
				t.Fatal("invalid framed configuration accepted")
			}
		})
	}
	for _, rawURL := range []string{"http://localhost", "https://localhost/service?query=1", "https://localhost/invalid/service"} {
		if client, err := NewBrowserXHTTPClient(context.Background(), rawURL, base); err == nil {
			client.Close()
			t.Fatalf("invalid framed URL accepted: %s", rawURL)
		}
	}
	baseURL, _ := url.Parse("https://example.com")
	base.Mode, base.Path, base.SessionPlace = BrowserXHTTPModeStreamUp, "/package.Service", BrowserXHTTPPlacementCookie
	client := &BrowserXHTTPClient{opts: base, baseURL: *baseURL, codec: newBrowserXHTTPCodec(base), host: "front.test"}
	rawURL, headers := client.newStreamRequest("session")
	if rawURL != "https://example.com/package.Service/Tun" || headers.Get("X-Xhttp-Session") != "session" || headers.Get("TE") != "trailers" || headers.Get("Content-Type") != "application/grpc" {
		t.Fatalf("RPC metadata: %s, %v", rawURL, headers)
	}
	_, headers = client.newRequest(http.MethodGet, "session", "")
	if !strings.Contains(headers.Get("Cookie"), "x_session=session") {
		t.Fatalf("download metadata = %v", headers)
	}
}
