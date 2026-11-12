//go:build with_cronet_test

package cronet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"
)

// The oracle comes from the checked-in root-store metadata, independently of
// Chromium's generated C++ tables. Capture actual ClientHello bytes to verify
// that both bindings advertise the roots selected by the engine.
func TestBrowserTrustAnchorIDs(t *testing.T) {
	if err := checkLibrary(); err != nil {
		t.Fatal(err)
	}
	ids, publicRoot, publicID := trustAnchorFixture(t)
	_, privateRoot := strictECHCertificate(t, "inner.test")
	_, echConfig := strictECHKey(t)
	for _, mode := range []string{BrowserXHTTPModePacketUp, BrowserXHTTPModeStreamOne} {
		for _, tc := range []struct {
			name    string
			options BrowserXHTTPOptions
			want    []string
		}{
			{name: "chrome-roots", want: ids},
			{name: "explicit-public-root", options: BrowserXHTTPOptions{TrustedRootCertificates: publicRoot}, want: []string{publicID}},
			{name: "mixed-roots", options: BrowserXHTTPOptions{TrustedRootCertificates: publicRoot + privateRoot}, want: []string{publicID}},
			{name: "private-root", options: BrowserXHTTPOptions{TrustedRootCertificates: privateRoot}},
			{name: "empty-roots", options: BrowserXHTTPOptions{UseTrustedRootCertificates: true}},
			{name: "ech-chrome-roots", options: BrowserXHTTPOptions{ECHConfigList: echConfig}, want: ids},
			{name: "ech-public-root", options: BrowserXHTTPOptions{ECHConfigList: echConfig, TrustedRootCertificates: publicRoot}, want: []string{publicID}},
			{name: "reality-chrome-roots", options: BrowserXHTTPOptions{Reality: &BrowserRealityOptions{PublicKey: [32]byte{9}}}, want: ids},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				type capture struct {
					hello []byte
					err   error
				}
				captured := make(chan capture, 1)
				finished := make(chan struct{})
				t.Cleanup(func() {
					listener.Close()
					<-finished
				})
				go func() {
					defer close(finished)
					conn, err := listener.Accept()
					if err != nil {
						captured <- capture{err: err}
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
					hello, err := readTrustAnchorClientHello(conn)
					captured <- capture{hello, err}
				}()
				options := tc.options
				options.Mode = mode
				options.DisableQUIC = true
				options.DialContext = func(ctx context.Context) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				}
				client, err := NewBrowserXHTTPClient(ctx, "https://inner.test", options)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				conn, err := client.DialContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				select {
				case result := <-captured:
					if result.err != nil {
						t.Fatal(result.err)
					}
					got, present, err := trustAnchorIDsFromClientHello(result.hello)
					if err != nil {
						t.Fatal(err)
					}
					if present != (len(tc.want) > 0) || !slices.Equal(got, tc.want) {
						t.Fatalf("trust_anchors present=%v IDs=%v, want %v", present, got, tc.want)
					}
					t.Logf("ClientHello trust_anchors: present=%v, IDs=%d", present, len(got))
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			})
		}
	}
}

func trustAnchorFixture(t *testing.T) (ids []string, publicRoot, publicID string) {
	t.Helper()
	directory := "naiveproxy/src/net/data/ssl/chrome_root_store"
	certificates := make(map[string]string)
	for _, name := range []string{"root_store.certs", "additional.certs"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		for {
			block, rest := pem.Decode(data)
			if block == nil {
				break
			}
			data = rest
			if block.Type == "CERTIFICATE" {
				digest := sha256.Sum256(block.Bytes)
				certificates[hex.EncodeToString(digest[:])] = string(pem.EncodeToMemory(block))
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(directory, "root_store.textproto"))
	if err != nil {
		t.Fatal(err)
	}
	anchors := regexp.MustCompile(`(?ms)^(trust_anchors|additional_certs)\s*\{(.*?)^\}`)
	idField := regexp.MustCompile(`(?m)^\s*trust_anchor_id:\s*("[^"]+")`)
	hashField := regexp.MustCompile(`sha256_hex:\s*"([0-9a-f]{64})"`)
	tlsAnchor := regexp.MustCompile(`(?m)^\s*tls_trust_anchor:\s*true\s*$`)
	for _, anchor := range anchors.FindAllSubmatch(data, -1) {
		if string(anchor[1]) == "additional_certs" && !tlsAnchor.Match(anchor[2]) {
			continue
		}
		id := idField.FindSubmatch(anchor[2])
		if id == nil {
			continue
		}
		decoded, err := strconv.Unquote(string(id[1]))
		if err != nil {
			t.Fatal(err)
		}
		hexID := hex.EncodeToString([]byte(decoded))
		ids = append(ids, hexID)
		hash := hashField.FindSubmatch(anchor[2])
		if hash == nil || certificates[string(hash[1])] == "" {
			t.Fatalf("missing certificate for trust anchor %s", hexID)
		}
		if publicRoot == "" {
			publicRoot, publicID = certificates[string(hash[1])], hexID
		}
	}
	if len(ids) == 0 {
		t.Fatal("Chrome root store has no classic Trust Anchor IDs")
	}
	slices.Sort(ids)
	return
}

func readTrustAnchorClientHello(r io.Reader) ([]byte, error) {
	var handshake []byte
	for {
		var header [5]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return nil, err
		}
		if header[0] != 22 {
			return nil, fmt.Errorf("expected handshake record, got %d", header[0])
		}
		payload := make([]byte, binary.BigEndian.Uint16(header[3:]))
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
		handshake = append(handshake, payload...)
		if len(handshake) < 4 {
			continue
		}
		size := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
		if handshake[0] != 1 || size > 65536 {
			return nil, fmt.Errorf("invalid ClientHello header %x", handshake[:4])
		}
		if len(handshake) >= size+4 {
			return handshake[4 : size+4], nil
		}
	}
}

func trustAnchorIDsFromClientHello(hello []byte) ([]string, bool, error) {
	if len(hello) < 34 {
		return nil, false, io.ErrUnexpectedEOF
	}
	rest := hello[34:] // legacy_version and random
	vector := func(width int) ([]byte, error) {
		if len(rest) < width {
			return nil, io.ErrUnexpectedEOF
		}
		n := int(rest[0])
		if width == 2 {
			n = int(binary.BigEndian.Uint16(rest))
		}
		rest = rest[width:]
		if len(rest) < n {
			return nil, io.ErrUnexpectedEOF
		}
		value := rest[:n]
		rest = rest[n:]
		return value, nil
	}
	for _, width := range []int{1, 2, 1} { // session ID, ciphers, compression
		if _, err := vector(width); err != nil {
			return nil, false, err
		}
	}
	extensions, err := vector(2)
	if err != nil || len(rest) != 0 {
		return nil, false, fmt.Errorf("invalid ClientHello extensions: %v", err)
	}
	rest = extensions
	var ids []string
	present := false
	for len(rest) > 0 {
		if len(rest) < 2 {
			return nil, false, io.ErrUnexpectedEOF
		}
		kind := binary.BigEndian.Uint16(rest)
		rest = rest[2:]
		data, err := vector(2)
		if err != nil {
			return nil, false, err
		}
		if kind != 0xca34 {
			continue
		}
		if present || len(data) < 2 || int(binary.BigEndian.Uint16(data)) != len(data)-2 {
			return nil, false, fmt.Errorf("invalid trust_anchors extension %x", data)
		}
		present = true
		for list := data[2:]; len(list) > 0; {
			n := int(list[0])
			list = list[1:]
			if n == 0 || n > len(list) {
				return nil, false, fmt.Errorf("invalid trust anchor length %d", n)
			}
			ids = append(ids, hex.EncodeToString(list[:n]))
			list = list[n:]
		}
	}
	return ids, present, nil
}
