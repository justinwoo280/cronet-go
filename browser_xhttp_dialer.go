package cronet

import (
	"io"
	"net"
)

func (c *BrowserXHTTPClient) dialSocket(address string, port uint16) int {
	if c.echEnabled() && address == browserDNSAddress && port == browserDNSPort {
		fd, conn, err := createSocketPair()
		if err != nil {
			return NetErrorConnectionFailed.Code()
		}
		c.relayWG.Add(1)
		go func() {
			defer c.relayWG.Done()
			_ = serveDNSStreamConn(c.ctx, conn, c.resolveECH)
		}()
		return fd
	}
	conn, err := c.opts.DialContext(c.ctx)
	if err != nil {
		return toNetError(err).Code()
	}
	// Only a concrete TCP connection can be handed over without bypassing
	// behavior implemented by a wrapper (for example a proxy or TLS layer).
	if tcp, ok := conn.(*net.TCPConn); ok {
		fd, err := dupSocketFD(tcp)
		if err == nil {
			_ = conn.Close()
			return fd
		}
	}
	fd, pipe, err := createSocketPair()
	if err != nil {
		_ = conn.Close()
		return NetErrorConnectionFailed.Code()
	}
	c.relayWG.Add(1)
	go func() {
		defer c.relayWG.Done()
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(pipe, conn); done <- struct{}{} }()
		go func() { _, _ = io.Copy(conn, pipe); done <- struct{}{} }()
		select {
		case <-done:
			_ = conn.Close()
			_ = pipe.Close()
			<-done
		case <-c.ctx.Done():
			_ = conn.Close()
			_ = pipe.Close()
			<-done
			<-done
		}
	}()
	return fd
}
