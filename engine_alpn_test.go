package cronet

import (
	"context"
	"net"
	"testing"
)

func TestBrowserALPNValidation(t *testing.T) {
	udp := func(context.Context) (net.Conn, error) { return nil, net.ErrClosed }
	for _, test := range []struct {
		name string
		opts BrowserXHTTPOptions
		ok   bool
	}{
		{"default", BrowserXHTTPOptions{}, true},
		{"h2", BrowserXHTTPOptions{ALPN: []string{"h2"}}, true},
		{"h1", BrowserXHTTPOptions{ALPN: []string{"http/1.1"}}, true},
		{"ordered", BrowserXHTTPOptions{ALPN: []string{"http/1.1", "h2"}}, true},
		{"h3", BrowserXHTTPOptions{ALPN: []string{"h3"}}, true},
		{"udp", BrowserXHTTPOptions{ALPN: []string{"h3"}, DialUDPContext: udp}, true},
		{"unknown", BrowserXHTTPOptions{ALPN: []string{"h3-29"}}, false},
		{"empty entry", BrowserXHTTPOptions{ALPN: []string{""}}, false},
		{"duplicate", BrowserXHTTPOptions{ALPN: []string{"h2", "h2"}}, false},
		{"mixed transports", BrowserXHTTPOptions{ALPN: []string{"h3", "h2"}}, false},
		{"tcp only dialer", BrowserXHTTPOptions{ALPN: []string{"h3"}, DialContext: udp}, false},
		{"disabled quic", BrowserXHTTPOptions{ALPN: []string{"h3"}, DisableQUIC: true}, false},
		{"reality", BrowserXHTTPOptions{ALPN: []string{"h3"}, Reality: &BrowserRealityOptions{}}, false},
		{"strict ech", BrowserXHTTPOptions{ALPN: []string{"h3"}, ECHConfigList: []byte{1}, DialContext: udp}, false},
		{"shared engine", BrowserXHTTPOptions{ALPN: []string{"h2"}, Engine: Engine{ptr: 1}}, false},
		{"h1 stream one", BrowserXHTTPOptions{ALPN: []string{"http/1.1"}, Mode: BrowserXHTTPModeStreamOne}, false},
		{"h1 grpc", BrowserXHTTPOptions{ALPN: []string{"http/1.1"}, Mode: BrowserXHTTPModeStreamUp, GRPCFraming: true}, false},
		{"udp with tcp alpn", BrowserXHTTPOptions{ALPN: []string{"h2"}, DialUDPContext: udp}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateBrowserOptions(test.opts); (err == nil) != test.ok {
				t.Fatalf("valid=%v, error=%v", test.ok, err)
			}
		})
	}
}
