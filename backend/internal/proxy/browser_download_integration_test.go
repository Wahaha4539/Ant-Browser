package proxy

import (
	"ant-chrome/backend/internal/config"
	"ant-chrome/backend/internal/logger"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// All successful requests terminate at this loopback fixture, regardless of
// target hostname. A failed filter can never download a real Google component.
func localComponentUpstream(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, "local-only")
	}))
	t.Cleanup(target.Close)
	targetAddress := strings.TrimPrefix(target.URL, "http://")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodConnect {
			_, _ = io.WriteString(w, "local-only")
			return
		}
		remote, err := net.DialTimeout("tcp", targetAddress, time.Second)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			remote.Close()
			return
		}
		defer client.Close()
		defer remote.Close()
		_ = client.SetDeadline(time.Now().Add(6 * time.Second))
		_ = remote.SetDeadline(time.Now().Add(6 * time.Second))
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffer.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, buffer); remote.Close(); close(done) }()
		_, _ = io.Copy(client, remote)
		client.Close()
		<-done
	}))
	t.Cleanup(upstream.Close)
	return upstream.URL, hits
}

func protectionTestBinary(t *testing.T, key string) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(key))
	if path == "" {
		t.Skip("set " + key + " for real-kernel loopback tests")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(absolute); err != nil {
		t.Fatal(err)
	}
	return absolute
}

func checkProtectionRequests(t *testing.T, proxyURL string, hits *atomic.Int64, protected bool) {
	t.Helper()
	var client *http.Client
	var err error
	if strings.HasPrefix(proxyURL, "socks5://") {
		client, err = buildSocks5HTTPClient(strings.TrimPrefix(proxyURL, "socks5://"), 1500*time.Millisecond)
	} else {
		client, err = buildHTTPProxyClient(proxyURL, 1500*time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	for _, target := range []string{"http://www.google.com/", "http://arena.ai/", "http://dl.google.com.example.test/", "http://edgedl.me.gvt1.com/", "http://dl.google.com:443/"} {
		before := hits.Load()
		response, requestErr := client.Get(target)
		denied := protected && (target == "http://edgedl.me.gvt1.com/" || target == "http://dl.google.com:443/")
		if denied {
			if response != nil {
				response.Body.Close()
			}
			if hits.Load() != before {
				t.Fatalf("blocked target reached upstream: %s", target)
			}
			if requestErr == nil && response.StatusCode == http.StatusOK {
				t.Fatalf("blocked target succeeded: %s", target)
			}
		} else {
			if requestErr != nil {
				t.Fatalf("allowed %s failed: %v", target, requestErr)
			}
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil || string(body) != "local-only" {
				t.Fatalf("unexpected local response: %s %v", body, readErr)
			}
			if hits.Load() <= before {
				t.Fatal("request did not use fixture upstream")
			}
		}
	}
}

func TestBrowserDownloadProtectionXrayLive(t *testing.T) {
	binary := protectionTestBinary(t, "XRAY_TEST_BINARY")
	upstream, hits := localComponentUpstream(t)
	// Auth forces an application bridge as well, testing real cache separation.
	upstream = strings.Replace(upstream, "http://", "http://user:pass@", 1)
	root := t.TempDir()
	manager := &XrayManager{Config: &config.Config{Browser: config.BrowserConfig{UserDataRoot: root, XrayBinaryPath: binary}}, AppRoot: root, Bridges: map[string]*XrayBridge{}}
	t.Cleanup(manager.StopBridges)
	protectedURL, protectedKey, err := manager.AcquireBrowserBridge(upstream, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ReleaseBridge(protectedKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	applicationURL, applicationKey, err := manager.ensureBridgeContext(ctx, upstream, nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if protectedKey == applicationKey || protectedURL == applicationURL {
		t.Fatal("browser reused application bridge")
	}
	checkProtectionRequests(t, protectedURL, hits, true)
	checkProtectionRequests(t, applicationURL, hits, false)
	// An unauthenticated native HTTP proxy must not escape via ParseProxyNode.
	noAuth := strings.Replace(upstream, "user:pass@", "", 1)
	nativeURL, nativeKey, err := manager.AcquireBrowserBridge(noAuth, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ReleaseBridge(nativeKey)
	if nativeURL == noAuth || nativeKey == "" {
		t.Fatal("native proxy bypassed filter")
	}
	checkProtectionRequests(t, nativeURL, hits, true)
	directURL, directKey, err := manager.AcquireBrowserBridge("direct://", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ReleaseBridge(directKey)
	if directURL == "direct://" || directKey == "" {
		t.Fatal("direct bypassed filter")
	}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "direct-local") }))
	defer local.Close()
	client, err := buildSocks5HTTPClient(strings.TrimPrefix(directURL, "socks5://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(local.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "direct-local" {
		t.Fatalf("direct exit broken: %s %v", body, err)
	}
}

func TestBrowserDownloadProtectionSingBoxLive(t *testing.T) {
	binary := protectionTestBinary(t, "SINGBOX_TEST_BINARY")
	upstream, hits := localComponentUpstream(t)
	parsed, _ := url.Parse(upstream)
	port, _ := strconv.Atoi(parsed.Port())
	root := t.TempDir()
	manager := &SingBoxManager{Config: &config.Config{Browser: config.BrowserConfig{UserDataRoot: root, SingBoxBinaryPath: binary}}, AppRoot: root, Bridges: map[string]*SingBoxBridge{}}
	for _, protected := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if protected {
			ctx = context.WithValue(ctx, browserDownloadProtectionKey{}, true)
		}
		listenPort, err := nextAvailablePort()
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		bridge, err := manager.launchBridgeOnPortContext(ctx, logger.New("DownloadProtectionTest"), computeNodeKey(fmt.Sprint(protected)), binary,
			map[string]interface{}{"type": "http", "tag": "proxy-out", "server": parsed.Hostname(), "server_port": port}, listenPort, 1)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { manager.stopBridgeProcess(bridge) })
		checkProtectionRequests(t, fmt.Sprintf("socks5://127.0.0.1:%d", listenPort), hits, protected)
		if protected {
			key := bridge.NodeKey
			manager.Bridges[key] = bridge
			_ = bridge.Cmd.Process.Kill()
			_ = bridge.waitExit()
			if err := manager.restartBridgeOnSamePort(logger.New("DownloadProtectionRecoveryTest"), key, bridge, 1); err != nil {
				t.Fatal(err)
			}
			restarted := manager.Bridges[key]
			t.Cleanup(func() {
				manager.mu.Lock()
				restarted.Stopping = true
				manager.mu.Unlock()
				manager.stopBridgeProcess(restarted)
			})
			if !restarted.BrowserDownloadProtection {
				t.Fatal("recovery lost protection")
			}
			checkProtectionRequests(t, fmt.Sprintf("socks5://127.0.0.1:%d", listenPort), hits, true)
		}
	}
}

func TestBrowserDownloadProtectionMihomoLive(t *testing.T) {
	binary := protectionTestBinary(t, "MIHOMO_TEST_BINARY")
	upstream, hits := localComponentUpstream(t)
	root := t.TempDir()
	manager := &ClashManager{Config: &config.Config{Browser: config.BrowserConfig{UserDataRoot: root}}, AppRoot: root}
	node, err := browserMihomoNode(upstream)
	if err != nil {
		t.Fatal(err)
	}
	for _, protected := range []bool{false, true} {
		port, err := nextAvailablePort()
		if err != nil {
			t.Fatal(err)
		}
		control, err := nextAvailablePort()
		if err != nil {
			t.Fatal(err)
		}
		path, err := manager.buildMihomoNodeConfigWithBrowserProtection(fmt.Sprint(protected), node, port, control, protected)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-f", path, "-d", filepath.Dir(path))
		hideWindow(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		if err := waitTCPPortReady("127.0.0.1", port, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		checkProtectionRequests(t, fmt.Sprintf("http://127.0.0.1:%d", port), hits, protected)
	}
}
