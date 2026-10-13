package proxy

import (
	"ant-chrome/backend/internal/config"
	"ant-chrome/backend/internal/logger"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newXrayIntegrationManager(t *testing.T) (*XrayManager, string) {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv("XRAY_TEST_BINARY"))
	if binary == "" {
		t.Skip("set XRAY_TEST_BINARY to run local Xray integration tests")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("Xray test binary: %v", err)
	}
	root := t.TempDir()
	return &XrayManager{
		Config: &config.Config{
			Browser: config.BrowserConfig{UserDataRoot: root},
		},
		AppRoot: root,
		Bridges: make(map[string]*XrayBridge),
	}, binary
}

// The blackhole outbound ensures these tests never use a real proxy or account.
func launchLocalTestXray(t *testing.T, manager *XrayManager, binary string, port int) (*XrayBridge, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, bridge, err := manager.launchBridgeAttemptContext(
		ctx, logger.New("XrayTest"), "local-test", binary,
		[]interface{}{map[string]interface{}{"protocol": "blackhole", "tag": "proxy-out"}},
		[]interface{}{map[string]interface{}{
			"type": "field", "inboundTag": []string{"socks-in"}, "outboundTag": "proxy-out",
		}},
		port, "", false, 1,
	)
	if bridge != nil {
		t.Cleanup(func() {
			manager.stopBridgeProcess(bridge)
			select {
			case <-bridge.ExitDone:
			case <-time.After(5 * time.Second):
				t.Error("test Xray child did not exit")
			}
		})
	}
	return bridge, err
}

func TestXrayStartupCapturesUDPBindFailure(t *testing.T) {
	manager, binary := newXrayIntegrationManager(t)
	port := occupyUDPOnlyPort(t)
	_, err := launchLocalTestXray(t, manager, binary, port)
	if err == nil {
		t.Fatal("Xray unexpectedly started on an occupied UDP port")
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "udp") || !strings.Contains(message, "bind") {
		t.Fatalf("startup error lost the UDP bind reason: %v", err)
	}
	if !isRetryableXrayLaunchError(err) {
		t.Fatalf("UDP bind failure should be retryable: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(manager.resolveWorkdir("local-test"), "xray-stderr.log"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(strings.ToLower(string(data)), "failed to start:") {
		t.Fatalf("Xray stdout failure was not preserved: %s", data)
	}
}

func TestXrayStartupOnAllocatedTCPUDPPort(t *testing.T) {
	manager, binary := newXrayIntegrationManager(t)
	bridge, err := launchLocalTestXray(t, manager, binary, 0)
	if err != nil {
		t.Fatalf("Xray should start on an allocated TCP/UDP port: %v", err)
	}
	if bridge == nil || !bridge.Running {
		t.Fatal("expected a running Xray bridge")
	}
	if err := checkSocks5Handshake(fmt.Sprintf("127.0.0.1:%d", bridge.Port), time.Second); err != nil {
		t.Fatal(err)
	}
}
