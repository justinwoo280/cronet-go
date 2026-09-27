package cronet

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing/common/buf"
)

func TestNaiveConnConcurrentIO(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	_ = left.SetDeadline(time.Now().Add(5 * time.Second))
	_ = right.SetDeadline(time.Now().Add(5 * time.Second))
	sender, receiver := &naiveConn{Conn: left}, &naiveConn{Conn: right}
	payload := bytes.Repeat([]byte{'a'}, 8192)
	var readers, writers sync.WaitGroup
	var received atomic.Int64
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			data := make([]byte, 73)
			for {
				n, err := receiver.Read(data)
				received.Add(int64(n))
				if !bytes.Equal(data[:n], bytes.Repeat([]byte{'a'}, n)) {
					t.Error("padding was exposed as application data")
				}
				if err != nil {
					if err != io.EOF {
						t.Error(err)
					}
					return
				}
			}
		}()
	}
	for i := 0; i < 16; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			if i%2 == 0 {
				_, err := sender.Write(payload)
				if err != nil {
					t.Error(err)
				}
			} else {
				buffer := buf.NewSize(3 + len(payload) + 255)
				buffer.Advance(3)
				_, _ = buffer.Write(payload)
				if err := sender.WriteBuffer(buffer); err != nil {
					t.Error(err)
				}
			}
			_ = sender.FrontHeadroom()
			_ = sender.RearHeadroom()
			_ = sender.WriterMTU()
			_ = sender.WriterReplaceable()
		}()
	}
	writers.Wait()
	_ = left.Close()
	readers.Wait()
	if got, want := received.Load(), int64(16*len(payload)); got != want {
		t.Fatalf("received %d bytes, want %d", got, want)
	}
}

type delayedNaiveClose struct {
	NaiveConn
	called chan struct{}
}

func (c *delayedNaiveClose) Close() error { close(c.called); return nil }

func TestTrackedNaiveConnWaitsForTermination(t *testing.T) {
	client := &NaiveClient{}
	client.activeConnections.Add(1)
	underlying := &delayedNaiveClose{called: make(chan struct{})}
	conn := &trackedNaiveConn{NaiveConn: underlying, client: client}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	<-underlying.called
	released := make(chan struct{})
	go func() { client.activeConnections.Wait(); close(released) }()
	select {
	case <-released:
		t.Fatal("connection released before native termination")
	case <-time.After(20 * time.Millisecond):
	}
	conn.release()
	conn.release() // A terminal callback must release exactly once.
	<-released
}
