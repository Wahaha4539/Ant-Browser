package proxy

import (
	"ant-chrome/backend/internal/config"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBrowserDownloadProtectionIsolatedBridgeKeys(t *testing.T) {
	normal := context.Background()
	protected := browserBridgeContext(&config.Config{})
	if !browserDownloadsProtected(protected) || browserDownloadsProtected(normal) {
		t.Fatal("incorrect context")
	}
	for _, material := range []string{"proxy", "proxy\x00dns", "proxy\x00mihomo"} {
		if bridgeKeyForContext(normal, material) != computeNodeKey(material) {
			t.Fatal("application cache key changed")
		}
		if bridgeKeyForContext(normal, material) == bridgeKeyForContext(protected, material) {
			t.Fatal("protected and unprotected bridges collide")
		}
	}
	for _, policy := range []string{"components", "off"} {
		ctx := browserBridgeContext(&config.Config{Browser: config.BrowserConfig{BackgroundDownloadPolicy: policy}})
		if browserDownloadsProtected(ctx) {
			t.Fatalf("unexpected network filter for %s", policy)
		}
	}
}

func TestBrowserDownloadProtectionNativeRoutesStayInConnector(t *testing.T) {
	for _, connector := range []string{"xray", "mihomo"} {
		for _, src := range []string{"", "direct://", "http://127.0.0.1:8080", "socks5://127.0.0.1:1080"} {
			result, err := ResolveBrowserProxyKernelForConnector(src, nil, "", connector, "strict")
			if err != nil {
				t.Fatal(err)
			}
			if result.Kernel != connector {
				t.Fatalf("%s %s: got %s", connector, src, result.Kernel)
			}
			for _, policy := range []string{"components", "off"} {
				expected, err := ResolveProxyKernelForConnector(src, nil, "", connector)
				if err != nil {
					t.Fatal(err)
				}
				result, err := ResolveBrowserProxyKernelForConnector(src, nil, "", connector, policy)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(result, expected) {
					t.Fatalf("%s changed ordinary resolution", policy)
				}
			}
		}
	}
	for _, scheme := range []string{"hysteria2://secret@example.test:443", "tuic://00000000-0000-0000-0000-000000000001:secret@example.test:443", "anytls://secret@example.test:443"} {
		result, err := ResolveBrowserProxyKernelForConnector(scheme, nil, "", "xray", "strict")
		if err != nil || result.Kernel != ProxyKernelSingBox {
			t.Fatalf("combination stack lost sing-box: %+v %v", result, err)
		}
	}
	proxies := []config.BrowserProxy{{ProxyId: "test", ProxyConfig: "http://127.0.0.1:8080", PreferredKernel: "mihomo"}}
	if _, err := ResolveBrowserProxyKernelForConnector("", proxies, "test", "xray", "strict"); err == nil {
		t.Fatal("cross-stack override accepted")
	}
}

func TestBrowserNativeOutboundParsing(t *testing.T) {
	for _, src := range []string{"", "direct://", "http://localhost", "https://localhost", "socks5://localhost", "socks5://user:pass@localhost:1080"} {
		outbound, ok, err := browserNativeXrayOutbound(src)
		if err != nil || !ok || outbound["tag"] != "proxy-out" {
			t.Fatalf("%s: %v %v %v", src, outbound, ok, err)
		}
		node, err := browserMihomoNode(src)
		if err != nil {
			t.Fatal(err)
		}
		if src == "" || src == "direct://" {
			if node != nil {
				t.Fatal("direct should use DIRECT")
			}
		} else if node == nil {
			t.Fatal("missing upstream")
		}
	}
	if _, ok, err := browserNativeXrayOutbound("vmess://abc"); err != nil || ok {
		t.Fatal("non-native URI wrongly intercepted")
	}
	if _, _, err := browserNativeXrayOutbound("http://localhost:70000"); err == nil {
		t.Fatal("invalid port accepted")
	}
}

func TestBrowserDownloadProtectionGeneratedConfigs(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Browser: config.BrowserConfig{UserDataRoot: root}}
	original := []interface{}{map[string]interface{}{"type": "field", "inboundTag": []string{"socks-in"}, "outboundTag": "second-hop"}}
	filtered := browserXrayDownloadRules(original)
	if len(filtered) != 2 || len(original) != 1 || filtered[1].(map[string]interface{})["outboundTag"] != "second-hop" {
		t.Fatal("chain route lost")
	}
	xray := &XrayManager{Config: cfg, AppRoot: root}
	path, err := xray.buildRuntimeConfigWithRoute("xray", []interface{}{map[string]interface{}{"protocol": "freedom", "tag": "second-hop"}}, filtered, 1080, "")
	if err != nil {
		t.Fatal(err)
	}
	x := readProtectionJSON(t, path)
	rules := x["routing"].(map[string]interface{})["rules"].([]interface{})
	rule := rules[0].(map[string]interface{})
	if rule["outboundTag"] != "block" {
		t.Fatal("Xray block not first")
	}
	domains := rule["domain"].([]interface{})
	if !reflect.DeepEqual(domains, []interface{}{"full:edgedl.me.gvt1.com", "full:dl.google.com"}) {
		t.Fatalf("overbroad domain filter: %v", domains)
	}

	sing := &SingBoxManager{Config: cfg, AppRoot: root}
	for _, protected := range []bool{false, true} {
		path, err := sing.buildConfigWithBrowserProtection("sing", map[string]interface{}{"type": "direct", "tag": "proxy-out"}, 1080, protected)
		if err != nil {
			t.Fatal(err)
		}
		rules := readProtectionJSON(t, path)["route"].(map[string]interface{})["rules"].([]interface{})
		if got := rules[0].(map[string]interface{})["action"] == "reject"; got != protected {
			t.Fatal("sing-box protection scope wrong")
		}
	}
	clash := &ClashManager{Config: cfg, AppRoot: root}
	for _, protected := range []bool{false, true} {
		path, err := clash.buildMihomoNodeConfigWithBrowserProtection("mihomo", nil, 1080, 9090, protected)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]interface{}
		if err := yaml.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		rules := payload["rules"].([]interface{})
		if protected && !reflect.DeepEqual(rules, browserMihomoDownloadRules()) {
			t.Fatalf("wrong Mihomo rules: %v", rules)
		}
		if !protected && !reflect.DeepEqual(rules, []interface{}{"MATCH,proxy-out"}) {
			t.Fatal("application rules altered")
		}
		if strings.Contains(string(data), "googleapis.com") || strings.Contains(string(data), "DOMAIN-SUFFIX") {
			t.Fatal("overbroad filtering")
		}
	}
}

func readProtectionJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
