package backend

import (
	"ant-chrome/backend/internal/browser"
	"ant-chrome/backend/internal/config"
	"ant-chrome/backend/internal/traffic"
	"encoding/json"
	"os"
	"path/filepath"
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
