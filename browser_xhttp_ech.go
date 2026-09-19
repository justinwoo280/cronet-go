package cronet

import (
	"context"
	"errors"
	"fmt"
	"net"

	mDNS "github.com/miekg/dns"
)

const browserDNSAddress = "127.0.0.1"
const browserDNSPort = 53

func (c *BrowserXHTTPClient) echEnabled() bool {
	return len(c.opts.ECHConfigList) > 0 || c.opts.GetECHConfigList != nil
}

func (c *BrowserXHTTPClient) prepareECH(ctx context.Context) error {
	if !c.echEnabled() {
		return nil
	}
	config := c.opts.ECHConfigList
	if c.opts.GetECHConfigList != nil {
		lookupCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(c.ctx, cancel)
		defer stop()
		defer cancel()
		var err error
		config, err = c.opts.GetECHConfigList(lookupCtx)
		if err != nil {
			return fmt.Errorf("cronet xhttp: fetch ECH config: %w", err)
		}
	}
	if len(config) == 0 {
		return errors.New("cronet xhttp: empty ECH config list")
	}
	c.echMu.Lock()
	c.echConfig = append([]byte(nil), config...)
	c.echMu.Unlock()
	return nil
}

// Feed the existing Chromium HTTPS-RR -> endpoint metadata -> BoringSSL path.
// All DNS sockets terminate inside this process. Actual endpoint resolution is
// still done by DialContext, and ECH retry configs remain managed by Chromium.
func (c *BrowserXHTTPClient) configureECH(params EngineParams) {
	_ = params.SetAsyncDNS(true)
	_ = params.SetDNSServerOverride([]string{"127.0.0.1:53"})
	_ = params.SetUseDnsHttpsSvcb(true)
	c.engine.SetUDPDialer(func(address string, port uint16) (int, string, uint16, func()) {
		if address != browserDNSAddress || port != browserDNSPort {
			return NetErrorConnectionFailed.Code(), "", 0, nil
		}
		fd, conn, err := createPacketSocketPair(false)
		if err != nil {
			return NetErrorConnectionFailed.Code(), "", 0, nil
		}
		ctx, cancel := context.WithCancel(c.ctx)
		c.relayWG.Add(1)
		go func() {
			defer c.relayWG.Done()
			defer cancel()
			_ = serveDNSPacketConn(ctx, conn, c.resolveECH)
		}()
		return fd, "", 0, cancel
	})
}

func (c *BrowserXHTTPClient) resolveECH(_ context.Context, request *mDNS.Msg) *mDNS.Msg {
	response := new(mDNS.Msg)
	response.SetReply(request)
	if len(request.Question) != 1 || !matchesServerName(request.Question[0].Name, c.baseURL.Hostname()) {
		response.Rcode = mDNS.RcodeNameError
		return response
	}
	q := request.Question[0]
	switch q.Qtype {
	case mDNS.TypeA:
		response.Answer = []mDNS.RR{&mDNS.A{
			Hdr: mDNS.RR_Header{Name: q.Name, Rrtype: mDNS.TypeA, Class: mDNS.ClassINET},
			A:   net.IPv4(127, 0, 0, 1),
		}}
	case mDNS.TypeHTTPS:
		c.echMu.Lock()
		config := append([]byte(nil), c.echConfig...)
		c.echMu.Unlock()
		if len(config) == 0 {
			response.Rcode = mDNS.RcodeServerFailure
			return response
		}
		response = injectECHConfig(request, response, config, []string{"h2"})
		// The embedding application's getter owns the TTL. Do not retain stale
		// synthetic records after it supplies a refreshed configuration.
		for _, rr := range response.Answer {
			rr.Header().Ttl = 0
		}
	}
	return response
}
