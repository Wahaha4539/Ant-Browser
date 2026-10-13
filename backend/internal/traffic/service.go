package traffic

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func fmtPort(port int) string { return strconv.Itoa(port) }

type entry struct {
	row              Snapshot
	relay            *relay
	baseUp, baseDown int64
	lastUp, lastDown int64
	lastSample       time.Time
}

type service struct {
	mu               sync.Mutex
	root             string
	entries          map[string]*entry
	token            string
	shutdown         chan struct{}
	stopping         bool
	persistenceError string
}

func newService(root, token string) (*service, error) {
	rows, err := ReadSnapshots(root)
	if err != nil {
		return nil, fmt.Errorf("read traffic history: %w", err)
	}
	s := &service{root: root, entries: make(map[string]*entry), token: token, shutdown: make(chan struct{})}
	for _, row := range rows {
		s.entries[row.ProfileID] = &entry{row: row}
	}
	return s, nil
}

func (e *entry) current(details bool) Snapshot {
	row := e.row
	if e.relay != nil {
		up, down := e.relay.uploaded.Load(), e.relay.downloaded.Load()
		if details {
			var domains []DomainSnapshot
			up, down, domains = e.relay.domainSnapshot()
			row.Domains = mergeDomains(e.row.Domains, domains, e.baseUp+up, e.baseDown+down)
		}
		row.UploadedBytes = e.baseUp + up
		row.DownloadedBytes = e.baseDown + down
		row.SessionUploadedBytes = up
		row.SessionDownloadedBytes = down
	} else if details {
		row.Domains = mergeDomains(row.Domains, nil, row.UploadedBytes, row.DownloadedBytes)
	}
	if !details {
		row.Domains = nil
	}
	return row
}

func (s *service) snapshotsLocked(includeDomains bool) []Snapshot {
	rows := make([]Snapshot, 0, len(s.entries))
	for _, e := range s.entries {
		row := e.current(includeDomains)
		if s.persistenceError != "" {
			row.Message = "流量历史保存失败：" + s.persistenceError
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ProfileID < rows[j].ProfileID })
	return rows
}
func (s *service) saveLocked() error {
	err := writeJSONFile(filepath.Join(s.root, "totals.json"), s.snapshotsLocked(true))
	if err != nil {
		s.persistenceError = err.Error()
	} else {
		s.persistenceError = ""
	}
	return err
}

func (s *service) start(req StartRequest) (StartResponse, error) {
	if req.ProfileID == "" || len(req.ProfileID) > 128 || req.DebugPort < 0 || req.DebugPort > 65535 {
		return StartResponse{}, fmt.Errorf("invalid traffic session")
	}
	upstream, err := UpstreamAddress(req.ProxyURL)
	if err != nil {
		return StartResponse{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return StartResponse{}, fmt.Errorf("traffic helper is stopping")
	}
	previous := s.entries[req.ProfileID]
	if previous != nil && previous.relay != nil {
		return StartResponse{}, fmt.Errorf("实例已有正在运行的计量连接，请先停止实例")
	}
	var history []DomainSnapshot
	if previous != nil {
		history = append(history, previous.current(true).Domains...)
		for i := range history {
			history[i].SessionUploadedBytes = 0
			history[i].SessionDownloadedBytes = 0
		}
	}
	u, _ := url.Parse(req.ProxyURL)
	r, err := newRelay(upstream, req.DebugPort, strings.ToLower(u.Scheme), history)
	if err != nil {
		return StartResponse{}, err
	}
	u.Host = r.listener.Addr().String()
	r.proxyURL = u.String()
	now := time.Now()
	row := Snapshot{Domains: history, DomainStatsAvailable: true, ProfileID: req.ProfileID, Running: true, State: "active", StartedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}
	if previous != nil {
		row.UploadedBytes = previous.row.UploadedBytes
		row.DownloadedBytes = previous.row.DownloadedBytes
	}
	s.entries[req.ProfileID] = &entry{row: row, relay: r, baseUp: row.UploadedBytes, baseDown: row.DownloadedBytes, lastSample: now}
	if err = s.saveLocked(); err != nil {
		r.close()
		if previous == nil {
			delete(s.entries, req.ProfileID)
		} else {
			s.entries[req.ProfileID] = previous
		}
		return StartResponse{}, err
	}
	return StartResponse{ProxyURL: r.proxyURL}, nil
}

func (s *service) stopLocked(id string) {
	e := s.entries[id]
	if e == nil || e.relay == nil {
		return
	}
	e.relay.close()
	e.row = e.current(true)
	e.relay = nil
	e.row.Running = false
	e.row.State = "stopped"
	e.row.UploadBytesPerSecond = 0
	e.row.DownloadBytesPerSecond = 0
	e.row.UpdatedAt = time.Now().Format(time.RFC3339)
}
func (s *service) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping = true
	for id := range s.entries {
		s.stopLocked(id)
	}
	s.saveLocked()
}

func (s *service) sample(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := 0
	for id, e := range s.entries {
		if e.relay == nil {
			continue
		}
		if !e.relay.ownerAlive(now) {
			s.stopLocked(id)
			continue
		}
		up, down := e.relay.uploaded.Load(), e.relay.downloaded.Load()
		seconds := now.Sub(e.lastSample).Seconds()
		if seconds > 0 {
			e.row.UploadBytesPerSecond = float64(up-e.lastUp) / seconds
			e.row.DownloadBytesPerSecond = float64(down-e.lastDown) / seconds
		}
		e.lastUp = up
		e.lastDown = down
		e.lastSample = now
		e.row.UpdatedAt = now.Format(time.RFC3339)
		active++
	}
	s.saveLocked()
	return active
}

func decode(r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
func respond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/health":
			respond(w, map[string]bool{"ok": true})
			return
		case "/domains":
			s.mu.Lock()
			e := s.entries[r.URL.Query().Get("profileId")]
			if e == nil {
				s.mu.Unlock()
				http.NotFound(w, r)
				return
			}
			row := e.current(true)
			if s.persistenceError != "" {
				row.Message = "流量历史保存失败：" + s.persistenceError
			}
			s.mu.Unlock()
			respond(w, row)
			return
		case "/snapshots":
			s.mu.Lock()
			rows := s.snapshotsLocked(false)
			s.mu.Unlock()
			respond(w, rows)
			return
		}
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/start":
		var req StartRequest
		if err := decode(r, &req); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		result, err := s.start(req)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		respond(w, result)
	case "/owner":
		var req OwnerRequest
		if err := decode(r, &req); err != nil || req.PID <= 0 {
			http.Error(w, "invalid owner", 400)
			return
		}
		s.mu.Lock()
		e := s.entries[req.ProfileID]
		if e != nil && e.relay != nil {
			e.relay.ownerPID.Store(int64(req.PID))
		}
		s.mu.Unlock()
		respond(w, map[string]bool{"ok": true})
	case "/stop":
		var req StopRequest
		if err := decode(r, &req); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		s.mu.Lock()
		s.stopLocked(req.ProfileID)
		err := s.saveLocked()
		s.mu.Unlock()
		if err != nil {
			http.Error(w, "could not persist traffic totals", 500)
			return
		}
		respond(w, map[string]bool{"ok": true})
	case "/shutdown":
		s.mu.Lock()
		if !s.stopping {
			s.stopping = true
			close(s.shutdown)
		}
		s.mu.Unlock()
		respond(w, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

// Serve runs in a detached helper process: stopping only the desktop UI must
// neither interrupt browser connections nor lose subsequent traffic.
func Serve(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("traffic directory must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	s, err := newService(root, hex.EncodeToString(secret))
	if err != nil {
		return err
	}
	defer func() {
		s.close() // Flush totals before allowing a new desktop/helper to reconnect.
		current, readErr := ReadRuntime(root)
		if readErr == nil && current.Token == s.token {
			os.Remove(filepath.Join(root, "runtime.json"))
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	info := Runtime{PID: os.Getpid(), Address: ln.Addr().String(), Token: s.token}
	if err = writeJSONFile(filepath.Join(root, "runtime.json"), info); err != nil {
		return err
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8192}
	defer server.Close()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(ln) }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastActive := time.Now()
	for {
		select {
		case <-s.shutdown:
			return nil
		case err := <-serverErrors:
			return err
		case now := <-ticker.C:
			if s.sample(now) > 0 {
				lastActive = now
			} else if now.Sub(lastActive) > time.Minute {
				s.mu.Lock()
				active := false
				for _, e := range s.entries {
					if e.relay != nil {
						active = true
						break
					}
				}
				if !active {
					s.stopping = true
				}
				s.mu.Unlock()
				if !active {
					return nil
				}
			}
		}
	}
}
