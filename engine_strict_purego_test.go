//go:build with_purego && with_cronet_test

package cronet

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The loader runs once per process. Use a subprocess to test a real older
// library without affecting tests that need the current native build.
func TestStrictECHOldLibrary(t *testing.T) {
	path := os.Getenv("CRONET_TEST_OLD_LIBRARY")
	if path == "" {
		t.Skip("set CRONET_TEST_OLD_LIBRARY to a pre-Strict native library")
	}
	if os.Getenv("CRONET_TEST_OLD_LIBRARY_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStrictECHOldLibrary$", "-test.v")
		cmd.Env = append(os.Environ(), "CRONET_TEST_OLD_LIBRARY_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("old library check: %v\n%s", err, output)
		}
		return
	}
	if err := LoadLibrary(path); err != nil {
		t.Fatal(err)
	}
	client, err := NewBrowserXHTTPClient(context.Background(), "https://inner.test", BrowserXHTTPOptions{
		ECHConfigList: []byte{0, 4, 0xfe, 0x0c, 0, 0},
		DialContext: func(context.Context) (net.Conn, error) {
			panic("attempted TCP connection with unsupported Strict ECH")
		},
	})
	if client != nil || err == nil || !strings.Contains(err.Error(), "does not support Strict ECH") {
		t.Fatalf("expected explicit capability failure, got client=%v, error=%v", client, err)
	}
	client, err = NewBrowserXHTTPClient(context.Background(), "https://inner.test", BrowserXHTTPOptions{Reality: &BrowserRealityOptions{PublicKey: [32]byte{9}}})
	if client != nil || err == nil || !strings.Contains(err.Error(), "does not support REALITY") {
		t.Fatalf("expected REALITY capability failure, got client=%v, error=%v", client, err)
	}
	client, err = NewBrowserXHTTPClient(context.Background(), "https://inner.test", BrowserXHTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
}
