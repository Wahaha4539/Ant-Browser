package backend

import (
	"ant-chrome/backend/internal/config"
	"strings"
)

var blockedBrowserDownloadFeatures = []string{
	"PreemptiveSodaDownload",
	"LiveCaption",
	"OnDeviceWebSpeech",
	"OptimizationGuideOnDeviceModel",
}

// Apply after all user arguments, before URLs, so existing profiles and one-off
// launches receive the same protection without rewriting their saved settings.
func applyBrowserBackgroundDownloadPolicy(args []string, policy string) []string {
	policy = config.NormalizeBrowserDownloadPolicy(policy)
	if policy == config.BrowserDownloadsOff {
		return args
	}
	out := make([]string, 0, len(args)+3)
	disabled, enabled := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		name, value, hasValue := strings.Cut(arg, "=")
		if name == "--disable-component-update" || name == "--disable-quic" {
			continue
		}
		if name != "--disable-features" && name != "--enable-features" {
			out = append(out, args[i])
			continue
		}
		if !hasValue && i+1 < len(args) && !strings.HasPrefix(strings.TrimSpace(args[i+1]), "--") {
			i++
			value = args[i]
		}
		for _, feature := range strings.Split(value, ",") {
			feature = strings.TrimSpace(feature)
			if feature == "" {
				continue
			}
			if name == "--disable-features" {
				disabled = appendUniqueString(disabled, feature)
			} else if !isBlockedBrowserDownloadFeature(feature) {
				enabled = appendUniqueString(enabled, feature)
			}
		}
	}
	for _, feature := range blockedBrowserDownloadFeatures {
		disabled = appendUniqueString(disabled, feature)
	}
	out = append(out, "--disable-component-update", "--disable-quic")
	out = append(out, "--disable-features="+strings.Join(disabled, ","))
	if len(enabled) > 0 {
		out = append(out, "--enable-features="+strings.Join(enabled, ","))
	}
	return out
}

func isBlockedBrowserDownloadFeature(feature string) bool {
	name := strings.TrimPrefix(strings.TrimSpace(feature), "*")
	if i := strings.IndexAny(name, "<:"); i >= 0 {
		name = name[:i]
	}
	for _, blocked := range blockedBrowserDownloadFeatures {
		if strings.EqualFold(name, blocked) {
			return true
		}
	}
	return false
}
