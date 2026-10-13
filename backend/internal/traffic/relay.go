package traffic

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type relay struct {
	listener      net.Listener
	upstream      string
	scheme        string
	domainMu      sync.Mutex
	domains       map[string]*domainCounter
	proxyURL      string
	debugPort     int
	ownerPID      atomic.Int64
	uploaded      atomic.Int64
	downloaded    atomic.Int64
	started       time.Time
	lastOwnerSeen time.Time
	mu            sync.Mutex
	connections   map[net.Conn]struct{}
	closing       bool
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	done          chan struct{}
}

func newRelay(upstream string, debugPort int, scheme string, history []DomainSnapshot) (*relay, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &relay{listener: ln, upstream: upstream, debugPort: debugPort, started: time.Now(), lastOwnerSeen: time.Now(), connections: make(map[net.Conn]struct{}), done: make(chan struct{})}
	r.scheme = scheme
	r.domains = make(map[string]*domainCounter)
	for _, row := range history {
		r.domains[row.Domain] = &domainCounter{}
	}
	r.domains[""] = &domainCounter{}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		defer close(r.done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.mu.Lock()
			if r.closing {
				r.mu.Unlock()
				c.Close()
				return
			}
			r.connections[c] = struct{}{}
			r.wg.Add(1)
			r.mu.Unlock()
			go r.forward(ctx, c)
		}
	}()
	return r, nil
}

// Count writes, including long-lived/streaming responses, not just completed
// requests. Wrapper prevents io.Copy from bypassing the counter via ReaderFrom.
type countingWriter struct {
	dst    io.Writer
	count  *atomic.Int64
	meter  *connectionMeter
	upload bool
}

func (w countingWriter) Write(p []byte) (int, error) {
	// Discover request metadata before writing so an immediate response can be
	// attributed. Only successful writes are counted, including partial writes.
	var spans []domainSpan
	if w.upload {
		w.meter.relay.domainMu.Lock()
		spans = w.meter.prepareUpload(p)
		w.meter.relay.domainMu.Unlock()
	}
	n, err := w.dst.Write(p)
	w.meter.relay.domainMu.Lock()
	w.count.Add(int64(n))
	if !w.upload {
		spans = w.meter.prepareDownload(p[:n])
	}
	w.meter.record(spans, n, w.upload)
	w.meter.relay.domainMu.Unlock()
	return n, err
}

func (r *relay) forward(ctx context.Context, client net.Conn) {
	defer r.wg.Done()
	defer func() { client.Close(); r.mu.Lock(); delete(r.connections, client); r.mu.Unlock() }()
	upstream, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", r.upstream)
	if err != nil {
		return
	}
	defer upstream.Close()
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return
	}
	r.connections[upstream] = struct{}{}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.connections, upstream); r.mu.Unlock() }()
	meter := newConnectionMeter(r)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := io.Copy(countingWriter{dst: upstream, count: &r.uploaded, meter: meter, upload: true}, client)
		if err != nil {
			upstream.Close()
			client.Close()
		} else if tcp, ok := upstream.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
	}()
	_, err = io.Copy(countingWriter{dst: client, count: &r.downloaded, meter: meter}, upstream)
	if err != nil {
		client.Close()
		upstream.Close()
	} else if tcp, ok := client.(*net.TCPConn); ok {
		tcp.CloseWrite()
	}
	<-done
}

func (r *relay) close() {
	r.mu.Lock()
	r.closing = true
	r.cancel()
	r.listener.Close()
	for c := range r.connections {
		c.Close()
	}
	r.mu.Unlock()
	<-r.done
	r.wg.Wait()
}

func (r *relay) ownerAlive(now time.Time) bool {
	pid := int(r.ownerPID.Load())
	if pid > 0 && processAlive(pid) {
		r.lastOwnerSeen = now
		return true
	}
	if pid == 0 && now.Sub(r.started) < 2*time.Minute {
		return true
	}
	// Extension installation can hand off to a different browser PID. Keep the
	// relay while that browser's original, loopback CDP port is still alive.
	if r.debugPort > 0 {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmtPort(r.debugPort)), 150*time.Millisecond)
		if err == nil {
			c.Close()
			r.lastOwnerSeen = now
			return true
		}
	}
	return now.Sub(r.lastOwnerSeen) < 15*time.Second
}
