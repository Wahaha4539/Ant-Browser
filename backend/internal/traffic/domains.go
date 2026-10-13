package traffic

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const maxTrackedDomains = 4096
const maxProxyHeader = 64 << 10

// All domain counters and connection parser state are protected by domainMu.
// Parsing is passive: original bytes, authentication and the chosen connector
// are forwarded unchanged. No URLs, credentials or response bodies are saved.
type domainCounter struct{ up, down int64 }
type attribution struct {
	meter    *connectionMeter
	domain   string
	up, down int64
}
type domainSpan struct {
	n      int
	target *attribution
}
type httpTarget struct {
	method string
	target *attribution
}
type connectionMeter struct {
	relay    *relay
	target   *attribution
	socks    socksTarget
	upload   httpStream
	download httpStream
	requests []httpTarget
}

func newConnectionMeter(r *relay) *connectionMeter {
	m := &connectionMeter{relay: r}
	m.target = &attribution{meter: m}
	m.upload = httpStream{meter: m, request: true}
	m.download = httpStream{meter: m}
	return m
}
func (m *connectionMeter) counter(domain string) *domainCounter {
	if c := m.relay.domains[domain]; c != nil {
		return c
	}
	c := &domainCounter{}
	m.relay.domains[domain] = c
	return c
}
func (a *attribution) identify(domain string) {
	m := a.meter
	if domain != "" && m.relay.domains[domain] == nil && len(m.relay.domains) >= maxTrackedDomains {
		domain = "" // Bound memory and persistence growth, without dropping bytes.
	}
	if domain == a.domain {
		return
	}
	old, next := m.counter(a.domain), m.counter(domain)
	old.up -= a.up
	old.down -= a.down
	next.up += a.up
	next.down += a.down
	a.domain = domain
}
func (m *connectionMeter) prepareUpload(p []byte) []domainSpan {
	if m.relay.scheme == "http" {
		return m.upload.consume(p)
	}
	if domain, found := m.socks.consume(p); found {
		m.target.identify(domain)
	}
	return []domainSpan{{len(p), m.target}}
}
func (m *connectionMeter) prepareDownload(p []byte) []domainSpan {
	if m.relay.scheme == "http" {
		return m.download.consume(p)
	}
	return []domainSpan{{len(p), m.target}}
}
func (m *connectionMeter) record(spans []domainSpan, n int, upload bool) {
	for _, span := range spans {
		count := min(n, span.n)
		c := m.counter(span.target.domain)
		if upload {
			c.up += int64(count)
			span.target.up += int64(count)
		} else {
			c.down += int64(count)
			span.target.down += int64(count)
		}
		n -= count
		if n == 0 {
			break
		}
	}
}
func (r *relay) domainSnapshot() (int64, int64, []DomainSnapshot) {
	r.domainMu.Lock()
	defer r.domainMu.Unlock()
	rows := make([]DomainSnapshot, 0, len(r.domains))
	for domain, c := range r.domains {
		if c.up == 0 && c.down == 0 {
			continue
		}
		rows = append(rows, DomainSnapshot{Domain: domain, UploadedBytes: c.up, DownloadedBytes: c.down, SessionUploadedBytes: c.up, SessionDownloadedBytes: c.down})
	}
	return r.uploaded.Load(), r.downloaded.Load(), rows
}

// Legacy totals and unrecognized traffic remain explicit instead of inventing
// domain attribution. Domain totals must reconcile with the instance totals.
func mergeDomains(history, session []DomainSnapshot, up, down int64) []DomainSnapshot {
	byDomain := make(map[string]DomainSnapshot, len(history)+len(session))
	for _, row := range history {
		previous := byDomain[row.Domain]
		previous.Domain = row.Domain
		previous.UploadedBytes += row.UploadedBytes
		previous.DownloadedBytes += row.DownloadedBytes
		previous.SessionUploadedBytes += row.SessionUploadedBytes
		previous.SessionDownloadedBytes += row.SessionDownloadedBytes
		byDomain[row.Domain] = previous
	}
	for _, row := range session {
		previous := byDomain[row.Domain]
		previous.Domain = row.Domain
		previous.UploadedBytes += row.UploadedBytes
		previous.DownloadedBytes += row.DownloadedBytes
		previous.SessionUploadedBytes += row.SessionUploadedBytes
		previous.SessionDownloadedBytes += row.SessionDownloadedBytes
		byDomain[row.Domain] = previous
	}
	var classifiedUp, classifiedDown int64
	for _, row := range byDomain {
		classifiedUp += row.UploadedBytes
		classifiedDown += row.DownloadedBytes
	}
	unknown := byDomain[""]
	unknown.UploadedBytes += max(0, up-classifiedUp)
	unknown.DownloadedBytes += max(0, down-classifiedDown)
	byDomain[""] = unknown
	rows := make([]DomainSnapshot, 0, len(byDomain))
	for _, row := range byDomain {
		if row.UploadedBytes != 0 || row.DownloadedBytes != 0 {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].UploadedBytes+rows[i].DownloadedBytes, rows[j].UploadedBytes+rows[j].DownloadedBytes
		if a == b {
			return rows[i].Domain < rows[j].Domain
		}
		return a > b
	})
	return rows
}

func normalizeTarget(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || len(host) > 253 {
		return ""
	}
	for _, c := range host {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return ""
		}
	}
	return host
}

// Incremental SOCKS5 greeting, optional RFC1929 authentication, then CONNECT.
// Fragmentation never delays forwarding. IP targets are kept as IPs, not reverse
// resolved (which would guess a domain and generate extra network requests).
type socksTarget struct {
	buffer []byte
	stage  int
	done   bool
}

func (s *socksTarget) consume(p []byte) (string, bool) {
	if s.done {
		return "", false
	}
	s.buffer = append(s.buffer, p[:min(len(p), 2048-len(s.buffer))]...)
	for {
		b := s.buffer
		switch s.stage {
		case 0:
			if len(b) < 2 {
				return "", false
			}
			if b[0] != 5 || b[1] == 0 {
				s.finish()
				return "", false
			}
			n := 2 + int(b[1])
			if len(b) < n {
				return "", false
			}
			s.buffer = b[n:]
			s.stage = 1
		case 1:
			if len(b) < 2 {
				return "", false
			}
			if b[0] == 5 {
				s.stage = 2
				continue
			}
			if b[0] != 1 {
				s.finish()
				return "", false
			}
			n := 2 + int(b[1])
			if len(b) <= n || len(b) < n+1+int(b[n]) {
				return "", false
			}
			s.buffer = b[n+1+int(b[n]):]
			s.stage = 2
		case 2:
			if len(b) < 5 {
				return "", false
			}
			if b[0] != 5 || b[1] != 1 || b[2] != 0 {
				s.finish()
				return "", false
			}
			var host string
			switch b[3] {
			case 1:
				if len(b) < 10 {
					return "", false
				}
				host = net.IP(b[4:8]).String()
			case 4:
				if len(b) < 22 {
					return "", false
				}
				host = net.IP(b[4:20]).String()
			case 3:
				if len(b) < 7+int(b[4]) {
					return "", false
				}
				host = string(b[5 : 5+int(b[4])])
			default:
				s.finish()
				return "", false
			}
			s.finish()
			return normalizeTarget(host), true
		}
	}
}
func (s *socksTarget) finish() { s.done = true; s.buffer = nil }

// HTTP framing only identifies proxy targets and byte boundaries. It never
// rewrites requests or reads decrypted HTTPS. Keep-alive/pipelined plain HTTP
// requests are matched to responses, rather than assigning the whole TCP
// connection to its first hostname.
type httpStream struct {
	meter     *connectionMeter
	request   bool
	phase     string
	buffer    []byte
	target    *attribution
	remaining int64
}

func (s *httpStream) reset() { s.phase = ""; s.buffer = nil; s.target = nil; s.remaining = 0 }
func (s *httpStream) consume(p []byte) []domainSpan {
	var spans []domainSpan
	for len(p) > 0 {
		if s.target == nil {
			if !s.request && len(s.meter.requests) > 0 {
				s.target = s.meter.requests[0].target
			} else {
				s.target = &attribution{meter: s.meter}
				if !s.request {
					s.phase = "opaque"
				}
			}
		}
		target := s.target
		n := len(p)
		switch s.phase {
		case "":
			n = s.header(p, "\r\n\r\n", maxProxyHeader)
			if bytes.HasSuffix(s.buffer, []byte("\r\n\r\n")) {
				s.parseHeader()
			}
		case "body", "chunk-data":
			n = int(min(int64(n), s.remaining))
			s.remaining -= int64(n)
			if s.remaining == 0 {
				if s.phase == "body" {
					s.reset()
				} else {
					s.phase = "chunk-end"
					s.remaining = 2
				}
			}
		case "chunk-size":
			n = s.header(p, "\r\n", 4096)
			if bytes.HasSuffix(s.buffer, []byte("\r\n")) {
				line := strings.TrimSpace(strings.SplitN(string(s.buffer), ";", 2)[0])
				size, err := strconv.ParseUint(line, 16, 63)
				s.buffer = nil
				if err != nil {
					s.phase = "unknown"
				} else if size == 0 {
					s.phase = "trailer"
				} else {
					s.phase = "chunk-data"
					s.remaining = int64(size)
				}
			}
		case "chunk-end":
			n = int(min(int64(n), s.remaining))
			for i, c := range p[:n] {
				want := byte('\r')
				if s.remaining-int64(i) == 1 {
					want = '\n'
				}
				if c != want {
					s.phase = "unknown"
				}
			}
			s.remaining -= int64(n)
			if s.phase != "unknown" && s.remaining == 0 {
				s.phase = "chunk-size"
			}
		case "trailer":
			n = s.header(p, "\r\n", maxProxyHeader)
			if bytes.HasSuffix(s.buffer, []byte("\r\n")) {
				if len(s.buffer) == 2 {
					s.reset()
				} else {
					s.buffer = nil
				}
			}
		case "unknown":
			target = &attribution{meter: s.meter}
		}
		spans = append(spans, domainSpan{n, target})
		p = p[n:]
	}
	return spans
}
func (s *httpStream) header(p []byte, delimiter string, limit int) int {
	old := len(s.buffer)
	candidate := append(s.buffer, p[:min(len(p), limit-old)]...)
	if end := bytes.Index(candidate, []byte(delimiter)); end >= 0 {
		n := end + len(delimiter) - old
		s.buffer = candidate[:end+len(delimiter)]
		return n
	}
	s.buffer = candidate
	if len(candidate) == limit {
		s.buffer = nil
		s.phase = "unknown"
	}
	return min(len(p), limit-old)
}
func (s *httpStream) body(length int64, encodings []string) {
	if len(encodings) > 0 && encodings[0] == "chunked" {
		s.phase = "chunk-size"
	} else if length > 0 {
		s.phase = "body"
		s.remaining = length
	} else if length < 0 {
		s.phase = "opaque" // Response delimited by closing this connection.
	} else {
		s.reset()
	}
}
func (s *httpStream) parseHeader() {
	reader := bufio.NewReader(bytes.NewReader(s.buffer))
	s.buffer = nil
	if s.request {
		req, err := http.ReadRequest(reader)
		if err != nil || len(s.meter.requests) >= 128 {
			s.phase = "unknown"
			return
		}
		host := req.URL.Host
		if host == "" {
			host = req.Host
		}
		s.target.identify(normalizeTarget(host))
		s.meter.requests = append(s.meter.requests, httpTarget{method: req.Method, target: s.target})
		if req.Method == http.MethodConnect {
			s.phase = "opaque"
		} else {
			s.body(req.ContentLength, req.TransferEncoding)
		}
		return
	}
	if len(s.meter.requests) == 0 {
		s.phase = "unknown"
		return
	}
	request := s.meter.requests[0]
	response, err := http.ReadResponse(reader, &http.Request{Method: request.method})
	if err != nil {
		s.phase = "unknown"
		return
	}
	if response.StatusCode >= 100 && response.StatusCode < 200 && response.StatusCode != 101 {
		s.reset() // Interim responses do not consume a request from the queue.
		return
	}
	s.meter.requests = s.meter.requests[1:]
	if response.StatusCode == 101 || request.method == http.MethodConnect && response.StatusCode >= 200 && response.StatusCode < 300 {
		s.phase = "opaque"
		s.meter.upload.phase = "opaque"
		s.meter.upload.target = request.target
	} else if request.method == http.MethodHead || response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotModified {
		s.reset()
	} else {
		if request.method == http.MethodConnect {
			s.meter.upload.reset()
		}
		s.body(response.ContentLength, response.TransferEncoding)
	}
}
