package proxy

import (
	"ant-chrome/backend/internal/config"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in only: credentials are supplied via environment, never checked in.
// Requests are bounded IP lookups or HEAD requests, never component packages.
func TestBrowserDownloadProtectionSuppliedProxy(t *testing.T) {
	src := strings.TrimSpace(os.Getenv("BROWSER_DOWNLOAD_TEST_PROXY"))
	if src == "" {
		t.Skip("set BROWSER_DOWNLOAD_TEST_PROXY for an explicitly authorized live proxy test")
	}
	binary := protectionTestBinary(t, "XRAY_TEST_BINARY")
	parsed, err := url.Parse(src)
	if err != nil || parsed.Hostname() == "" {
		t.Fatal("invalid proxy URL")
	}
	root := t.TempDir()
	manager := &XrayManager{
		Config:  &config.Config{Browser: config.BrowserConfig{UserDataRoot: root, XrayBinaryPath: binary}},
		AppRoot: root, Bridges: map[string]*XrayBridge{},
	}
	t.Cleanup(manager.StopBridges)
	proxyURL, key, err := manager.AcquireBrowserBridge(src, nil, "")
	if err != nil {
		t.Fatalf("protected proxy launch failed: %v", err)
	}
	defer manager.ReleaseBridge(key)
	client, err := buildSocks5HTTPClient(strings.TrimPrefix(proxyURL, "socks5://"), 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	started := time.Now()
	response, err := client.Get("https://api.ipify.org?format=json")
	if err != nil {
		t.Fatalf("exit IP lookup failed: %v", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("exit IP lookup status=%d read_error=%v", response.StatusCode, readErr)
	}
	var result struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(data, &result); err != nil || net.ParseIP(result.IP) == nil {
		t.Fatal("invalid exit IP response")
	}
	t.Logf("proxy=%s exit_ip=%s ip_lookup=%s", parsed.Host, result.IP, time.Since(started).Round(time.Millisecond))

	for _, target := range []string{"https://www.google.com/generate_204", "https://arena.ai/"} {
		started := time.Now()
		request, _ := http.NewRequest(http.MethodHead, target, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Errorf("normal HEAD %s failed: %v", target, err)
			continue
		}
		response.Body.Close()
		t.Logf("normal HEAD %s status=%d elapsed=%s", target, response.StatusCode, time.Since(started).Round(time.Millisecond))
		if response.StatusCode >= 500 {
			t.Errorf("normal endpoint server error: %s status=%d", target, response.StatusCode)
		}
	}
	client.Timeout = 4 * time.Second
	for _, target := range []string{"http://edgedl.me.gvt1.com/", "https://edgedl.me.gvt1.com/", "https://dl.google.com/"} {
		started := time.Now()
		request, _ := http.NewRequest(http.MethodHead, target, nil)
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Errorf("download host was not locally denied: %s status=%d", target, response.StatusCode)
			continue
		}
		t.Logf("blocked HEAD %s elapsed=%s", target, time.Since(started).Round(time.Millisecond))
	}
}
