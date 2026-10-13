package backend

import (
	"ant-chrome/backend/internal/browser"
	"ant-chrome/backend/internal/config"
	"ant-chrome/backend/internal/traffic"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBrowserTrafficListDistinguishesHistoryFromUnmetered(t *testing.T) {
	root := t.TempDir()
	app := NewApp(root)
	cfg := config.DefaultConfig()
	app.config = cfg
	app.browserMgr = browser.NewManager(cfg, root)
	app.browserMgr.Profiles = map[string]*browser.Profile{
		"stopped": {ProfileId: "stopped"}, "running": {ProfileId: "running", Running: true}, "new": {ProfileId: "new"},
	}
	dir := app.browserTrafficRoot()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	history := []traffic.Snapshot{{ProfileID: "stopped", UploadedBytes: 30, DownloadedBytes: 900, UploadBytesPerSecond: 4, Running: true}, {ProfileID: "deleted", UploadedBytes: 9999}}
	data, _ := json.Marshal(history)
	if err := os.WriteFile(filepath.Join(dir, "totals.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := app.BrowserTrafficList()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("deleted profiles leaked into statistics: %+v", rows)
	}
	byID := map[string]BrowserTrafficSnapshot{}
	for _, row := range rows {
		byID[row.ProfileID] = row
	}
	if got := byID["stopped"]; got.State != "stopped" || got.UploadedBytes != 30 || got.DownloadedBytes != 900 || got.UploadBytesPerSecond != 0 {
		t.Fatalf("history: %+v", got)
	}
	if got := byID["running"]; got.State != "unmetered" || got.Message == "" {
		t.Fatalf("unmeasured live profile presented as zero: %+v", got)
	}
	if byID["new"].State != "not_started" {
		t.Fatalf("new profile: %+v", byID["new"])
	}
}

func TestBrowserTrafficDoesNotRewriteUnsupportedProxy(t *testing.T) {
	app := NewApp(t.TempDir())
	for _, original := range []string{"direct://", "", "https://proxy.example:443"} {
		got, metered, err := app.startProfileTraffic("test", original, 12345)
		if err != nil || metered || got != original {
			t.Fatalf("unsupported proxy changed: %q %v %v", got, metered, err)
		}
	}
	if _, err := os.Stat(filepath.Join(app.browserTrafficRoot(), "runtime.json")); !os.IsNotExist(err) {
		t.Fatal("helper started for unsupported proxy")
	}
}

func TestBrowserTrafficDetailHistoryLegacyAndProfileIsolation(t *testing.T) {
	root := t.TempDir()
	app := NewApp(root)
	cfg := config.DefaultConfig()
	app.config = cfg
	app.browserMgr = browser.NewManager(cfg, root)
	app.browserMgr.Profiles = map[string]*browser.Profile{
		"history": {ProfileId: "history"}, "legacy": {ProfileId: "legacy"}, "new": {ProfileId: "new"},
	}
	dir := app.browserTrafficRoot()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rows := []traffic.Snapshot{
		{ProfileID: "history", UploadedBytes: 100, DownloadedBytes: 500, SessionUploadedBytes: 30, DomainStatsAvailable: true, Domains: []traffic.DomainSnapshot{{Domain: "one.test", UploadedBytes: 80, DownloadedBytes: 500, SessionUploadedBytes: 20}, {Domain: "", UploadedBytes: 10}}},
		{ProfileID: "legacy", UploadedBytes: 30, DownloadedBytes: 900, SessionUploadedBytes: 10},
		{ProfileID: "deleted", Domains: []traffic.DomainSnapshot{{Domain: "private.test", UploadedBytes: 999}}},
	}
	data, _ := json.Marshal(rows)
	if err := os.WriteFile(filepath.Join(dir, "totals.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	row, err := app.BrowserTrafficDetail("history")
	if err != nil || len(row.Domains) != 2 || row.Domains[1].UploadedBytes != 20 || row.Domains[1].SessionUploadedBytes != 10 {
		t.Fatalf("history: %+v %v", row, err)
	}
	row, err = app.BrowserTrafficDetail("legacy")
	if err != nil || row.DomainStatsAvailable || len(row.Domains) != 1 || row.Domains[0].Domain != "" || row.Domains[0].DownloadedBytes != 900 || row.Domains[0].SessionUploadedBytes != 10 {
		t.Fatalf("legacy: %+v %v", row, err)
	}
	row, err = app.BrowserTrafficDetail("new")
	if err != nil || row.State != "not_started" || row.Domains == nil || len(row.Domains) != 0 {
		t.Fatalf("new: %+v %v", row, err)
	}
	if _, err := app.BrowserTrafficDetail("deleted"); err == nil {
		t.Fatal("deleted profile history was exposed")
	}
	summary, err := app.BrowserTrafficList()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range summary {
		if len(row.Domains) != 0 {
			t.Fatal("domain table included in instance polling")
		}
	}
}

func TestBrowserTrafficDetailLiveHelperAndFailure(t *testing.T) {
	root := t.TempDir()
	app := NewApp(root)
	cfg := config.DefaultConfig()
	app.config = cfg
	app.browserMgr = browser.NewManager(cfg, root)
	id := "profile + /?&"
	app.browserMgr.Profiles = map[string]*browser.Profile{id: {ProfileId: id, Running: true}}
	var fail atomic.Bool
	row := traffic.Snapshot{ProfileID: id, Running: true, State: "active", DomainStatsAvailable: true, UploadedBytes: 99, Domains: []traffic.DomainSnapshot{{Domain: "live.test", UploadedBytes: 99}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 32) {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch req.URL.Path {
		case "/health":
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		case "/snapshots":
			summary := row
			summary.Domains = nil
			json.NewEncoder(w).Encode([]traffic.Snapshot{summary})
		case "/domains":
			if req.URL.Query().Get("profileId") != id {
				t.Error("profile id not encoded correctly")
			}
			if fail.Load() {
				http.Error(w, "offline", 500)
				return
			}
			json.NewEncoder(w).Encode(row)
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	dir := app.browserTrafficRoot()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(traffic.Runtime{PID: os.Getpid(), Address: strings.TrimPrefix(server.URL, "http://"), Token: strings.Repeat("a", 32)})
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := app.BrowserTrafficDetail(id)
	if err != nil || len(result.Domains) != 1 || result.Domains[0].Domain != "live.test" {
		t.Fatalf("live helper: %+v %v", result, err)
	}
	fail.Store(true)
	if _, err := app.BrowserTrafficDetail(id); err == nil {
		t.Fatal("domain helper failure displayed as empty data")
	}
}
