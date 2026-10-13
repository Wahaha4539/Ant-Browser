package proxy

import (
	"ant-chrome/backend/internal/config"
	"context"
	"strings"
)

type browserDownloadProtectionKey struct{}

func browserBridgeContext(cfg *config.Config) context.Context {
	ctx := context.Background()
	if config.BrowserDownloadPolicy(cfg) == config.BrowserDownloadsStrict {
		return context.WithValue(ctx, browserDownloadProtectionKey{}, true)
	}
	return ctx
}

func browserDownloadsProtected(ctx context.Context) bool {
	return ctx != nil && ctx.Value(browserDownloadProtectionKey{}) == true
}

// Separate protected browser bridges from application downloads and probes.
// Bump the policy version whenever the protected host list or routing changes.
func bridgeKeyForContext(ctx context.Context, material string) string {
	if browserDownloadsProtected(ctx) {
		material += "\x00browser-download-protection-v1"
	}
	return computeNodeKey(material)
}

func browserBlockedDownloadHosts() []string {
	return []string{"edgedl.me.gvt1.com", "dl.google.com"}
}

func (m *XrayManager) AcquireBrowserBridge(src string, proxies []config.BrowserProxy, id string) (string, string, error) {
	return m.ensureBridgeContext(browserBridgeContext(m.Config), src, proxies, id, true)
}

func (m *SingBoxManager) AcquireBrowserBridge(src string, proxies []config.BrowserProxy, id string) (string, string, error) {
	return m.ensureBridgeContext(browserBridgeContext(m.Config), src, proxies, id, true)
}

func (m *ClashManager) AcquireBrowserNodeBridge(src string, proxies []config.BrowserProxy, id string) (string, string, error) {
	return m.ensureNodeBridgeContext(browserBridgeContext(m.Config), src, proxies, id, true)
}

// Only browser starts wrap native/direct connections. Application HTTP clients
// retain their original resolver and are not subject to browser download rules.
func ResolveBrowserProxyKernelForConnector(src string, proxies []config.BrowserProxy, id, connector, policy string) (ProxyKernelResolution, error) {
	resolution, err := ResolveProxyKernelForConnector(src, proxies, id, connector)
	if err != nil || config.NormalizeBrowserDownloadPolicy(policy) != config.BrowserDownloadsStrict || resolution.Kernel != ProxyKernelNative {
		return resolution, err
	}
	resolution.Kernel = ProxyKernelXray
	if config.NormalizeBrowserConnectorType(connector) == config.BrowserConnectorMihomo {
		resolution.Kernel = ProxyKernelMihomo
	}
	resolution.SupportedKernels = []string{resolution.Kernel}
	resolution.Reason = "浏览器下载保护：使用当前连接栈的本地过滤入口，保持原出站方式"
	return resolution, nil
}

func browserXrayDownloadRules(rules []interface{}) []interface{} {
	domains := browserBlockedDownloadHosts()
	for i := range domains {
		domains[i] = "full:" + domains[i]
	}
	return append([]interface{}{map[string]interface{}{
		"type": "field", "inboundTag": []string{"socks-in"},
		"domain": domains, "outboundTag": "block",
	}}, rules...)
}

func browserSingBoxDownloadRules(rules []interface{}) []interface{} {
	return append([]interface{}{map[string]interface{}{
		"inbound": []string{"socks-in"}, "domain": browserBlockedDownloadHosts(), "action": "reject",
	}}, rules...)
}

func browserMihomoDownloadRules() []interface{} {
	rules := make([]interface{}, 0, 3)
	for _, host := range browserBlockedDownloadHosts() {
		rules = append(rules, "DOMAIN,"+host+",REJECT")
	}
	return append(rules, "MATCH,proxy-out")
}

func browserNativeXrayOutbound(src string) (map[string]interface{}, bool, error) {
	if strings.TrimSpace(src) == "" || strings.EqualFold(strings.TrimSpace(src), "direct://") {
		return map[string]interface{}{"protocol": "freedom", "tag": "proxy-out"}, true, nil
	}
	spec, err := parseProxyBridgeSpec(src, false)
	if err != nil || spec == nil {
		return nil, false, err
	}
	return chainSocks5Outbound(chainSocks5Hop{
		Protocol: spec.Scheme, Server: spec.Server, Port: spec.Port,
		Username: spec.Username, Password: spec.Password, TLS: spec.Scheme == "https",
	}, "proxy-out", ""), true, nil
}

func browserMihomoNode(src string) (map[string]interface{}, error) {
	if strings.TrimSpace(src) == "" || strings.EqualFold(strings.TrimSpace(src), "direct://") {
		return nil, nil
	}
	spec, err := parseProxyBridgeSpec(src, false)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return buildMihomoNode(src)
	}
	protocol := spec.Scheme
	if protocol == "https" {
		protocol = "http"
	}
	node := map[string]interface{}{"name": "browser-upstream", "type": protocol, "server": spec.Server, "port": spec.Port}
	if spec.Username != "" {
		node["username"] = spec.Username
		node["password"] = spec.Password
	}
	if spec.Scheme == "https" {
		node["tls"] = true
	}
	return node, nil
}
