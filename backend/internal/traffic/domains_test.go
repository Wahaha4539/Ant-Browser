package traffic

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parserRelay(scheme string) *relay {
	return &relay{scheme: scheme, domains: map[string]*domainCounter{"": {}}}
}
func meterWrite(t *testing.T, m *connectionMeter, upload bool, payload []byte, fragment int) {
	t.Helper()
	count := &m.relay.downloaded
	if upload {
		count = &m.relay.uploaded
	}
	w := countingWriter{dst: io.Discard, count: count, meter: m, upload: upload}
	for len(payload) > 0 {
		n := min(fragment, len(payload))
		if _, err := w.Write(payload[:n]); err != nil {
			t.Fatal(err)
		}
		payload = payload[n:]
	}
}
func assertDomainsReconcile(t *testing.T, r *relay) map[string]DomainSnapshot {
	t.Helper()
	up, down, rows := r.domainSnapshot()
	byDomain := map[string]DomainSnapshot{}
	var sumUp, sumDown int64
	for _, row := range rows {
		byDomain[row.Domain] = row
		sumUp += row.UploadedBytes
		sumDown += row.DownloadedBytes
		if row.UploadedBytes < 0 || row.DownloadedBytes < 0 {
			t.Fatalf("negative domain counters: %+v", row)
		}
	}
	if sumUp != up || sumDown != down {
		t.Fatalf("domain totals %d/%d != instance totals %d/%d", sumUp, sumDown, up, down)
	}
	return byDomain
}
func TestSOCKS5DomainAttributionFragmentationAndAuthentication(t *testing.T) {
	for _, scheme := range []string{"socks", "socks5", "socks5h"} {
		for _, auth := range []bool{false, true} {
			r := parserRelay(scheme)
			m := newConnectionMeter(r)
			greeting := []byte{5, 1, 0}
			if auth {
				greeting = []byte{5, 1, 2}
			}
			meterWrite(t, m, true, greeting, 1)
			meterWrite(t, m, false, []byte{5, greeting[2]}, 1)
			if auth {
				meterWrite(t, m, true, []byte{1, 3, 'u', 's', 'r', 3, 'p', 'w', 'd'}, 1)
				meterWrite(t, m, false, []byte{1, 0}, 1)
			}
			host := "CDN.Example.Test."
			request := append([]byte{5, 1, 0, 3, byte(len(host))}, host...)
			request = append(request, 1, 187)
			meterWrite(t, m, true, request, 1)
			meterWrite(t, m, false, []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}, 1)
			meterWrite(t, m, true, []byte("encrypted request"), 2)
			meterWrite(t, m, false, bytes.Repeat([]byte("stream"), 100), 7)
			rows := assertDomainsReconcile(t, r)
			if len(rows) != 1 || rows["cdn.example.test"].UploadedBytes != r.uploaded.Load() {
				t.Fatalf("scheme %s auth %v: %+v", scheme, auth, rows)
			}
		}
	}
}
func TestSOCKSIPAndUnknownTargets(t *testing.T) {
	for _, tc := range []struct {
		target []byte
		domain string
	}{
		{[]byte{5, 1, 0, 1, 192, 0, 2, 8, 1, 187}, "192.0.2.8"},
		{append(append([]byte{5, 1, 0, 4}, net.ParseIP("2001:db8::1").To16()...), 1, 187), "2001:db8::1"},
		{[]byte("invalid proxy stream"), ""},
	} {
		r := parserRelay("socks5")
		m := newConnectionMeter(r)
		meterWrite(t, m, true, append([]byte{5, 1, 0}, tc.target...), 100)
		meterWrite(t, m, false, []byte("reply"), 2)
		rows := assertDomainsReconcile(t, r)
		if len(rows) != 1 || rows[tc.domain].DownloadedBytes != 5 {
			t.Fatalf("target %q: %+v", tc.domain, rows)
		}
	}
}
func TestHTTPConnectDomainAndLiveStreaming(t *testing.T) {
	r := parserRelay("http")
	m := newConnectionMeter(r)
	request := []byte("CONNECT Secure.Example.Test:443 HTTP/1.1\r\nHost: Secure.Example.Test:443\r\nProxy-Authorization: Basic ignored\r\n\r\n")
	meterWrite(t, m, true, request, 1)
	meterWrite(t, m, false, []byte("HTTP/1.1 200 Connection Established\r\n\r\n"), 1)
	meterWrite(t, m, true, []byte("opaque TLS"), 100)
	meterWrite(t, m, false, bytes.Repeat([]byte{22, 3, 3}, 512), 100)
	rows := assertDomainsReconcile(t, r)
	if len(rows) != 1 || rows["secure.example.test"].DownloadedBytes != r.downloaded.Load() {
		t.Fatalf("CONNECT: %+v", rows)
	}
}
func TestHTTPKeepAlivePipeliningAndChunkedBoundaries(t *testing.T) {
	for _, fragment := range []int{1, 7, 65536} {
		r := parserRelay("http")
		m := newConnectionMeter(r)
		req1 := "POST http://one.example.test/a HTTP/1.1\r\nHost: one.example.test\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\nX-Trailer: ok\r\n\r\n"
		req2 := "GET http://two.example.test/b HTTP/1.1\r\nHost: two.example.test\r\n\r\n"
		res1 := "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4;ext=ok\r\ndata\r\n0\r\nX-Trailer: ok\r\n\r\n"
		res2 := "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"
		meterWrite(t, m, true, []byte(req1+req2), fragment)
		meterWrite(t, m, false, []byte(res1+res2), fragment)
		rows := assertDomainsReconcile(t, r)
		if len(rows) != 2 || rows["one.example.test"].UploadedBytes != int64(len(req1)) || rows["one.example.test"].DownloadedBytes != int64(len(res1)) || rows["two.example.test"].UploadedBytes != int64(len(req2)) || rows["two.example.test"].DownloadedBytes != int64(len(res2)) {
			t.Fatalf("fragment %d: %+v", fragment, rows)
		}
	}
}
func TestHTTPBodylessResponses(t *testing.T) {
	for _, response := range []string{"HTTP/1.1 200 OK\r\nContent-Length: 99\r\n\r\n", "HTTP/1.1 304 Not Modified\r\nContent-Length: 99\r\n\r\n"} {
		r := parserRelay("http")
		m := newConnectionMeter(r)
		meterWrite(t, m, true, []byte("HEAD http://one.test/ HTTP/1.1\r\nHost: one.test\r\n\r\nGET http://two.test/ HTTP/1.1\r\nHost: two.test\r\n\r\n"), 2)
		meterWrite(t, m, false, []byte(response+"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"), 2)
		rows := assertDomainsReconcile(t, r)
		if len(rows) != 2 || rows["one.test"].DownloadedBytes != int64(len(response)) || rows["two.test"].DownloadedBytes == 0 {
			t.Fatalf("bodyless response: %+v", rows)
		}
	}
}

type partialWriter struct{}

func (partialWriter) Write(p []byte) (int, error) { return min(3, len(p)), io.ErrClosedPipe }
func TestDomainCountingPartialWrites(t *testing.T) {
	r := parserRelay("http")
	m := newConnectionMeter(r)
	w := countingWriter{dst: partialWriter{}, count: &r.uploaded, meter: m, upload: true}
	w.Write([]byte("CONNECT one.test:443 HTTP/1.1\r\nHost: one.test:443\r\n\r\n"))
	rows := assertDomainsReconcile(t, r)
	if rows["one.test"].UploadedBytes != 3 {
		t.Fatalf("partial write: %+v", rows)
	}
}
func TestDomainLimitKeepsUnclassifiedBytes(t *testing.T) {
	r := parserRelay("http")
	for i := 0; i < maxTrackedDomains; i++ {
		r.domains["host-"+fmtPort(i)] = &domainCounter{}
	}
	m := newConnectionMeter(r)
	meterWrite(t, m, true, []byte("GET http://new.test/ HTTP/1.1\r\nHost: new.test\r\n\r\n"), 999)
	if _, found := r.domains["new.test"]; found {
		t.Fatal("domain cap exceeded")
	}
	if rows := assertDomainsReconcile(t, r); rows[""].UploadedBytes == 0 {
		t.Fatal("unclassified bytes lost")
	}
}
func TestDomainPersistenceRestartAndLegacyTotals(t *testing.T) {
	s := testService(t)
	old := Snapshot{ProfileID: "history", UploadedBytes: 20, DownloadedBytes: 30}
	s.entries["history"] = &entry{row: old}
	address := startRelay(t, s, "history", echoUpstream(t), "socks5")
	host := "new.test"
	payload := append([]byte{5, 1, 0, 5, 1, 0, 3, byte(len(host))}, host...)
	payload = append(payload, 1, 187)
	if err := transfer(address, payload); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return snapshot(s, "history").DownloadedBytes == 30+int64(len(payload)) })
	s.mu.Lock()
	s.stopLocked("history")
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := newService(s.root, "restart")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	row := snapshot(restored, "history")
	if len(row.Domains) != 2 || !row.DomainStatsAvailable {
		t.Fatalf("restored: %+v", row)
	}
	address = startRelay(t, restored, "history", echoUpstream(t), "socks5")
	if err := transfer(address, payload); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return snapshot(restored, "history").DownloadedBytes == 30+2*int64(len(payload)) })
	row = snapshot(restored, "history")
	for _, domain := range row.Domains {
		if domain.Domain == "new.test" && (domain.UploadedBytes != 2*int64(len(payload)) || domain.SessionUploadedBytes != int64(len(payload))) {
			t.Fatalf("session double counted: %+v", domain)
		}
		if domain.Domain == "" && (domain.UploadedBytes != 20 || domain.DownloadedBytes != 30) {
			t.Fatalf("legacy history: %+v", domain)
		}
	}
}
func TestDomainControlIsAuthenticatedAndListStaysSmall(t *testing.T) {
	s := testService(t)
	s.entries["test"] = &entry{row: Snapshot{ProfileID: "test", UploadedBytes: 50, Domains: []DomainSnapshot{{Domain: "one.test", UploadedBytes: 50}}}}
	for _, route := range []string{"/domains?profileId=test", "/snapshots"} {
		for _, authorized := range []bool{false, true} {
			req := httptest.NewRequest(http.MethodGet, route, nil)
			if authorized {
				req.Header.Set("Authorization", "Bearer test-token")
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if !authorized && w.Code != 401 {
				t.Fatal("unprotected domain endpoint")
			}
			if authorized && (w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("one.test")) != (route != "/snapshots")) {
				t.Fatalf("route %s: %d %s", route, w.Code, w.Body.String())
			}
		}
	}
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.root, "totals.json"))
	if err != nil || !bytes.Contains(data, []byte("one.test")) {
		t.Fatalf("domain history missing: %s %v", data, err)
	}
}

func TestHTTPProxyRelayWithRealClientsAndSharedUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", "8192")
		w.Write(bytes.Repeat([]byte("x"), 8192))
	}))
	defer upstream.Close()
	s := testService(t)
	one := startRelay(t, s, "one", strings.TrimPrefix(upstream.URL, "http://"), "http")
	two := startRelay(t, s, "two", strings.TrimPrefix(upstream.URL, "http://"), "http")
	clients := []*http.Client{}
	for _, address := range []string{one, two} {
		u, _ := url.Parse("http://" + address)
		transport := &http.Transport{Proxy: http.ProxyURL(u)}
		t.Cleanup(transport.CloseIdleConnections)
		clients = append(clients, &http.Client{Transport: transport, Timeout: 3 * time.Second})
	}
	for i, hosts := range [][]string{{"one.test", "two.test", "one.test"}, {"three.test"}} {
		for _, host := range hosts {
			response, err := clients[i].Get("http://" + host + "/resource")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || len(body) != 8192 {
				t.Fatalf("body changed: %d %v", len(body), err)
			}
		}
	}
	eventually(t, func() bool {
		return snapshot(s, "one").DownloadedBytes > 3*8192 && snapshot(s, "two").DownloadedBytes > 8192
	})
	rows := assertDomainsReconcile(t, s.entries["one"].relay)
	if len(rows) != 2 || rows["one.test"].DownloadedBytes <= 2*8192 || rows["two.test"].DownloadedBytes <= 8192 {
		t.Fatalf("keepalive domains: %+v", rows)
	}
	if rows := assertDomainsReconcile(t, s.entries["two"].relay); len(rows) != 1 || rows["three.test"].DownloadedBytes <= 8192 {
		t.Fatalf("profiles mixed: %+v", rows)
	}
}

func TestHTTPSConnectRelayPreservesTLS(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { w.Write([]byte("encrypted response")) }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			t.Error("expected CONNECT")
			http.Error(w, "bad method", 400)
			return
		}
		remote, err := net.Dial("tcp", target.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			return
		}
		defer remote.Close()
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		done := make(chan struct{})
		go func() { io.Copy(remote, rw); remote.(*net.TCPConn).CloseWrite(); close(done) }()
		io.Copy(client, remote)
		client.Close()
		<-done
	}))
	defer upstream.Close()
	s := testService(t)
	address := startRelay(t, s, "tls", strings.TrimPrefix(upstream.URL, "http://"), "http")
	u, _ := url.Parse("http://" + address)
	transport := &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // Isolated self-signed test server.
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get("https://secure.test/resource")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "encrypted response" {
		t.Fatalf("TLS changed: %q %v", body, err)
	}
	eventually(t, func() bool { return snapshot(s, "tls").DownloadedBytes > int64(len(body)) })
	rows := assertDomainsReconcile(t, s.entries["tls"].relay)
	if len(rows) != 1 || rows["secure.test"].DownloadedBytes != s.entries["tls"].relay.downloaded.Load() {
		t.Fatalf("TLS target: %+v", rows)
	}
}

func TestHTTPRejectedConnectRetryAndUpgrade(t *testing.T) {
	r := parserRelay("http")
	m := newConnectionMeter(r)
	meterWrite(t, m, true, []byte("CONNECT first.test:443 HTTP/1.1\r\nHost: first.test:443\r\n\r\n"), 3)
	meterWrite(t, m, false, []byte("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n"), 3)
	meterWrite(t, m, true, []byte("GET http://second.test/socket HTTP/1.1\r\nHost: second.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"), 3)
	meterWrite(t, m, false, []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"), 3)
	meterWrite(t, m, true, []byte("GET http://not-a-real-target.test/ HTTP/1.1\r\n\r\n"), 3)
	meterWrite(t, m, false, []byte("websocket payload"), 3)
	rows := assertDomainsReconcile(t, r)
	if len(rows) != 2 || rows["first.test"].DownloadedBytes == 0 || rows["second.test"].DownloadedBytes == 0 {
		t.Fatalf("CONNECT retry / upgraded connection: %+v", rows)
	}
}

func TestMalformedProxyMetadataDoesNotInterruptForwarding(t *testing.T) {
	for _, scheme := range []string{"http", "socks5"} {
		for _, payload := range [][]byte{bytes.Repeat([]byte("a"), maxProxyHeader+20), []byte("invalid\r\n\r\nbody"), {5, 1, 0, 5, 1, 0, 9, 0, 0}} {
			r := parserRelay(scheme)
			m := newConnectionMeter(r)
			meterWrite(t, m, true, payload, 257)
			meterWrite(t, m, false, payload, 257)
			rows := assertDomainsReconcile(t, r)
			if len(rows) != 1 || rows[""].UploadedBytes != int64(len(payload)) || rows[""].DownloadedBytes != int64(len(payload)) {
				t.Fatalf("malformed %s: %+v", scheme, rows)
			}
		}
	}
}
