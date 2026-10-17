package integration

import (
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"overseer/internal/config"
)

// pgProxy is a TCP fault proxy between the application and PostgreSQL
// (PLAN Decision 4): Cut drops every connection and refuses new ones
// ("database down / restarting"), Restore accepts again, SetLatency
// delays traffic. The shared PostgreSQL server itself is never touched;
// test inspection (Harness.DBScan / DBExec) connects directly.
type pgProxy struct {
	t       *testing.T
	ln      net.Listener
	target  string
	mu      sync.Mutex
	refuse  bool
	latency time.Duration
	conns   map[net.Conn]struct{}
	cuts    int
}

func newPGProxy(t *testing.T) *pgProxy {
	t.Helper()
	host, port, _, _, _ := parseDSNFields(t, baseDSN())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pgProxy{t: t, ln: ln, target: net.JoinHostPort(host, strconv.Itoa(port)), conns: map[net.Conn]struct{}{}}
	go p.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		p.dropAll()
	})
	return p
}

// configure points the application's database at the proxy.
func (p *pgProxy) configure(c *config.Config) {
	c.Database.Host = "127.0.0.1"
	c.Database.Port = p.ln.Addr().(*net.TCPAddr).Port
}

func (p *pgProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refuse := p.refuse
		p.mu.Unlock()
		if refuse {
			_ = c.Close()
			continue
		}
		go p.pipe(c)
	}
}

func (p *pgProxy) pipe(client net.Conn) {
	server, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	if p.refuse {
		p.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	p.conns[client], p.conns[server] = struct{}{}, struct{}{}
	p.mu.Unlock()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn, delayed bool) {
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if delayed {
					p.mu.Lock()
					d := p.latency
					p.mu.Unlock()
					if d > 0 {
						time.Sleep(d)
					}
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil { // io.EOF or a dropped connection
				break
			}
		}
		done <- struct{}{}
	}
	go cp(server, client, true)
	go cp(client, server, false)
	<-done
	_ = client.Close()
	_ = server.Close()
	p.mu.Lock()
	delete(p.conns, client)
	delete(p.conns, server)
	p.mu.Unlock()
}

func (p *pgProxy) dropAll() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Cut drops every live connection and refuses new ones.
func (p *pgProxy) Cut() {
	p.mu.Lock()
	p.refuse = true
	p.cuts++
	p.mu.Unlock()
	p.dropAll()
}

// Restore accepts connections again.
func (p *pgProxy) Restore() {
	p.mu.Lock()
	p.refuse = false
	p.mu.Unlock()
}

// SetLatency delays client → server traffic.
func (p *pgProxy) SetLatency(d time.Duration) {
	p.mu.Lock()
	p.latency = d
	p.mu.Unlock()
}
