package config

import "strings"

const (
	BrowserDownloadsStrict     = "strict"
	BrowserDownloadsComponents = "components"
	BrowserDownloadsOff        = "off"
)

// Missing settings (including existing installations) default to protection.
func NormalizeBrowserDownloadPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case BrowserDownloadsOff:
		return BrowserDownloadsOff
	case BrowserDownloadsComponents:
		return BrowserDownloadsComponents
	default:
		return BrowserDownloadsStrict
	}
}

func BrowserDownloadPolicy(cfg *Config) string {
	if cfg == nil {
		return BrowserDownloadsStrict
	}
	return NormalizeBrowserDownloadPolicy(cfg.Browser.BackgroundDownloadPolicy)
}
