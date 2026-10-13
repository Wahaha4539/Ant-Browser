package traffic

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func echoUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	return ln.Addr().String()
}
func testService(t *testing.T) *service {
	t.Helper()
	s, err := newService(t.TempDir(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
}
func startRelay(t *testing.T, s *service, id, address, scheme string) string {
	t.Helper()
	result, err := s.start(StartRequest{ProfileID: id, ProxyURL: scheme + "://" + address})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(result.ProxyURL)
	if u.Scheme != scheme {
		t.Fatalf("scheme changed: %s", result.ProxyURL)
	}
	return u.Host
}
func transfer(address string, payload []byte) error {
	c, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err = c.Write(payload); err != nil {
		return err
	}
	c.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, payload) {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func snapshot(s *service, id string) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[id].current(true)
}
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestRelayIsolationHalfCloseAndPersistence(t *testing.T) {
	upstream := echoUpstream(t)
	s := testService(t)
	one := startRelay(t, s, "one", upstream, "socks5")
	two := startRelay(t, s, "two", upstream, "http")
	p1 := bytes.Repeat([]byte("a"), 32768)
	p2 := bytes.Repeat([]byte("b"), 777)
	if err := transfer(one, p1); err != nil {
		t.Fatal(err)
	}
	if err := transfer(two, p2); err != nil {
		t.Fatal(err)
	}
	s.sample(time.Now().Add(time.Second))
	first, second := snapshot(s, "one"), snapshot(s, "two")
	if first.UploadedBytes != int64(len(p1)) || first.DownloadedBytes != int64(len(p1)) || second.UploadedBytes != int64(len(p2)) || second.DownloadedBytes != int64(len(p2)) {
		t.Fatalf("incorrect per-profile totals: %+v %+v", first, second)
	}
	if first.UploadBytesPerSecond <= 0 || first.DownloadBytesPerSecond <= 0 {
		t.Fatal("live rates were not sampled")
	}
	s.mu.Lock()
	s.stopLocked("one")
	s.stopLocked("two")
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	history, err := ReadSnapshots(s.root)
	if err != nil || len(history) != 2 {
		t.Fatalf("history: %v %v", history, err)
	}
	restored, err := newService(s.root, "new-token")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	restarted := startRelay(t, restored, "one", upstream, "socks5")
	if err := transfer(restarted, []byte("new")); err != nil {
		t.Fatal(err)
	}
	result := snapshot(restored, "one")
	if result.UploadedBytes != int64(len(p1)+3) || result.SessionUploadedBytes != 3 {
		t.Fatalf("restart lost/double-counted history: %+v", result)
	}
	if got := snapshot(s, "two"); got.Running || got.UploadBytesPerSecond != 0 || got.DownloadBytesPerSecond != 0 {
		t.Fatalf("stopped rate: %+v", got)
	}
}

func TestRelayCountsStreamingBeforeConnectionCloses(t *testing.T) {
	s := testService(t)
	address := startRelay(t, s, "stream", echoUpstream(t), "socks5")
	c, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	payload := bytes.Repeat([]byte("z"), 64*1024)
	go c.Write(payload)
	received := make([]byte, len(payload))
	if _, err = io.ReadFull(c, received); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return snapshot(s, "stream").DownloadedBytes == int64(len(payload)) })
	s.mu.Lock()
	s.stopLocked("stream")
	s.mu.Unlock()
	if _, err = c.Read(make([]byte, 1)); err == nil {
		t.Fatal("stop did not close live connection")
	}
}

func TestRelayConcurrentConnections(t *testing.T) {
	s := testService(t)
	address := startRelay(t, s, "concurrent", echoUpstream(t), "http")
	const count = 12
	payload := bytes.Repeat([]byte("x"), 96*1024)
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() { errs <- transfer(address, payload) }()
	}
	for i := 0; i < count; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	expected := int64(count * len(payload))
	eventually(t, func() bool {
		row := snapshot(s, "concurrent")
		return row.UploadedBytes == expected && row.DownloadedBytes == expected
	})
}

func TestUnsupportedConnectionsAndDuplicateSession(t *testing.T) {
	for _, raw := range []string{"", "direct://", "https://proxy.test:443", "pac://proxy.test"} {
		if _, err := UpstreamAddress(raw); err == nil {
			t.Errorf("unsupported connection accepted: %s", raw)
		}
	}
	s := testService(t)
	upstream := echoUpstream(t)
	startRelay(t, s, "same", upstream, "socks5")
	if _, err := s.start(StartRequest{ProfileID: "same", ProxyURL: "socks5://" + upstream}); err == nil {
		t.Fatal("duplicate session accepted")
	}
}

func TestControlAuthorizationAndMethods(t *testing.T) {
	s := testService(t)
	for _, tt := range []struct {
		method, path, auth, origin string
		status                     int
	}{
		{"GET", "/snapshots", "", "", 401}, {"GET", "/snapshots", "Bearer wrong", "", 401},
		{"GET", "/snapshots", "Bearer test-token", "https://example.test", 401},
		{"GET", "/stop", "Bearer test-token", "", 405}, {"GET", "/snapshots", "Bearer test-token", "", 200},
	} {
		r := httptest.NewRequest(tt.method, tt.path, nil)
		r.Header.Set("Authorization", tt.auth)
		r.Header.Set("Origin", tt.origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Errorf("%s %s: %d", tt.method, tt.path, w.Code)
		}
	}
}

func TestCorruptHistoryDoesNotResetTotals(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "totals.json")
	os.WriteFile(file, []byte("broken"), 0600)
	if _, err := newService(root, "test"); err == nil {
		t.Fatal("corrupt history silently reset")
	}
	data, _ := os.ReadFile(file)
	if string(data) != "broken" {
		t.Fatal("history overwritten")
	}
}

func TestHelperClientReconnectAndFinalFlush(t *testing.T) {
	root := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- Serve(root) }()
	var info Runtime
	eventually(t, func() bool {
		var err error
		info, err = ReadRuntime(root)
		return err == nil && Call(info, "/health", nil, nil) == nil
	})
	t.Cleanup(func() { Call(info, "/shutdown", struct{}{}, nil) })
	var result StartResponse
	if err := Call(info, "/start", StartRequest{ProfileID: "kept-browser", ProxyURL: "socks5://" + echoUpstream(t)}, &result); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(result.ProxyURL)
	if err := transfer(u.Host, []byte("before")); err != nil {
		t.Fatal(err)
	}
	// A new desktop process reads the endpoint from disk; no UI context is needed.
	reconnected, err := ReadRuntime(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = transfer(u.Host, []byte("after")); err != nil {
		t.Fatal(err)
	}
	if err = Call(reconnected, "/stop", StopRequest{ProfileID: "kept-browser"}, nil); err != nil {
		t.Fatal(err)
	}
	if err = Call(reconnected, "/shutdown", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not exit")
	}
	rows, err := ReadSnapshots(root)
	if err != nil || len(rows) != 1 || rows[0].UploadedBytes != 11 || rows[0].DownloadedBytes != 11 {
		t.Fatalf("final totals: %+v %v", rows, err)
	}
}

func TestReadRuntimeRejectsRemoteEndpoint(t *testing.T) {
	root := t.TempDir()
	b, _ := json.Marshal(Runtime{Address: "example.com:80", Token: "12345678901234567890123456789012"})
	os.WriteFile(filepath.Join(root, "runtime.json"), b, 0600)
	if _, err := ReadRuntime(root); err == nil {
		t.Fatal("remote control endpoint accepted")
	}
}

func TestControlBodyLimit(t *testing.T) {
	s := testService(t)
	r := httptest.NewRequest(http.MethodPost, "/start", bytes.NewReader(bytes.Repeat([]byte("x"), 70<<10)))
	r.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("large body status %d", w.Code)
	}
}

func TestOwnerExitStopsRelayWithoutDesktop(t *testing.T) {
	s := testService(t)
	address := startRelay(t, s, "orphan", echoUpstream(t), "socks5")
	if err := transfer(address, []byte("background")); err != nil {
		t.Fatal(err)
	}
	s.entries["orphan"].relay.ownerPID.Store(2147483647)
	s.entries["orphan"].relay.lastOwnerSeen = time.Now().Add(-time.Minute)
	if active := s.sample(time.Now()); active != 0 {
		t.Fatal("orphan relay remained active")
	}
	row := snapshot(s, "orphan")
	if row.Running || row.DownloadedBytes != 10 {
		t.Fatalf("orphan final counters: %+v", row)
	}
	if c, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("orphan port is still open")
	}
}

func TestPersistenceFailureDoesNotLeaveLiveRelay(t *testing.T) {
	s := testService(t)
	if err := os.Mkdir(filepath.Join(s.root, "totals.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.start(StartRequest{ProfileID: "no-disk", ProxyURL: "socks5://" + echoUpstream(t)}); err == nil {
		t.Fatal("persistence failure ignored")
	}
	if len(s.entries) != 0 {
		t.Fatal("failed session was not rolled back")
	}
}
