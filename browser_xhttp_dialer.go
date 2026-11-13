package cronet

import (
	"context"
	"io"
	"net"
	"strconv"
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
	if c.opts.http3() || c.opts.DialContext == nil {
		return NetErrorConnectionFailed.Code()
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

func (c *BrowserXHTTPClient) dialUDPSocket(_ string, _ uint16) (int, string, uint16, func()) {
	conn, err := c.opts.DialUDPContext(c.ctx)
	if err != nil {
		return toNetError(err).Code(), "", 0, nil
	}
	var localAddress string
	var localPort uint16
	if addr := conn.LocalAddr(); addr != nil {
		host, port, err := net.SplitHostPort(addr.String())
		if err == nil && net.ParseIP(host) != nil {
			if number, err := strconv.ParseUint(port, 10, 16); err == nil {
				localAddress, localPort = host, uint16(number)
			}
		}
	}
	// Do not unwrap proxy or accounting connections: their packet processing must
	// remain in the data path, just like wrappers around the TCP dialer.
	if udp, ok := conn.(*net.UDPConn); ok {
		if fd, err := dupSocketFD(udp); err == nil {
			_ = conn.Close()
			return fd, localAddress, localPort, nil
		}
	}
	fd, pipe, err := createPacketSocketPair(false)
	if err != nil {
		_ = conn.Close()
		return NetErrorConnectionFailed.Code(), "", 0, nil
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.relayWG.Add(1)
	go func() {
		defer c.relayWG.Done()
		defer cancel()
		relayBrowserPackets(ctx, conn, pipe.(net.Conn))
	}()
	// The native socket close callback runs on Cronet's network thread. Only
	// cancel here; the relay goroutine performs potentially blocking cleanup.
	return fd, localAddress, localPort, cancel
}

func relayBrowserPackets(ctx context.Context, conn, pipe net.Conn) {
	done := make(chan struct{}, 2)
	copyPackets := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buffer := make([]byte, 65535)
		for {
			n, err := src.Read(buffer)
			if err != nil {
				return
			}
			if written, err := dst.Write(buffer[:n]); err != nil || written != n {
				return
			}
		}
	}
	go copyPackets(pipe, conn)
	go copyPackets(conn, pipe)
	remaining := 2
	select {
	case <-done:
		remaining--
	case <-ctx.Done():
	}
	_ = conn.Close()
	_ = pipe.Close()
	for ; remaining > 0; remaining-- {
		<-done
	}
}
