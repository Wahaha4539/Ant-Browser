package backend

import (
	"reflect"
	"strings"
	"testing"
)

func TestBrowserBackgroundDownloadsMergeAllLaunchArguments(t *testing.T) {
	args := buildBrowserLaunchArgs("profile-dir", 9222, "socks5://127.0.0.1:1080",
		[]string{"--fingerprint=123", "--disable-features=Translate"},
		[]string{"--no-first-run", "--disable-features", "Translate,ExistingFeature", "--enable-features=LiveCaption,KeepMe"},
		[]string{"--enable-features", "PreemptiveSodaDownload<Trial,OnDeviceWebSpeech:param/value,OtherFeature", "--disable-component-update", "--disable-quic"},
		[]string{"https://example.test/"}, false, "strict")
	disabled, enabled := "", ""
	componentCount, quicCount, disabledCount, enabledCount := 0, 0, 0, 0
	for _, arg := range args {
		if arg == "--disable-component-update" {
			componentCount++
		}
		if arg == "--disable-quic" {
			quicCount++
		}
		if strings.HasPrefix(arg, "--disable-features=") {
			disabled = strings.TrimPrefix(arg, "--disable-features=")
			disabledCount++
		}
		if strings.HasPrefix(arg, "--enable-features=") {
			enabled = strings.TrimPrefix(arg, "--enable-features=")
			enabledCount++
		}
	}
	if componentCount != 1 || quicCount != 1 || disabledCount != 1 || enabledCount != 1 {
		t.Fatalf("duplicate or missing protection: %v", args)
	}
	if enabled != "KeepMe,OtherFeature" {
		t.Fatalf("enabled = %q", enabled)
	}
	for _, feature := range append([]string{"Translate", "ExistingFeature"}, blockedBrowserDownloadFeatures...) {
		count := 0
		for _, item := range strings.Split(disabled, ",") {
			if item == feature {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("feature %s occurs %d times: %s", feature, count, disabled)
		}
	}
	if args[len(args)-1] != "https://example.test/" {
		t.Fatalf("URL must follow managed switches: %v", args)
	}
	if !strings.Contains(strings.Join(args, " "), "--fingerprint=123") {
		t.Fatal("lost fingerprint")
	}
}

func TestBrowserBackgroundDownloadsDefaultProtectsOldProfiles(t *testing.T) {
	for _, policy := range []string{"", "strict", "components", "typo"} {
		input := []string{"--disable-sync", "--no-first-run"}
		snapshot := append([]string{}, input...)
		result := applyBrowserBackgroundDownloadPolicy(input, policy)
		if !reflect.DeepEqual(input, snapshot) {
			t.Fatal("modified saved profile arguments")
		}
		if !strings.Contains(strings.Join(result, " "), "--disable-component-update") {
			t.Fatalf("policy %q is unprotected", policy)
		}
		if got := applyBrowserBackgroundDownloadPolicy(result, policy); !reflect.DeepEqual(got, result) {
			t.Fatalf("policy not idempotent: %v", got)
		}
	}
}

func TestBrowserBackgroundDownloadsOffPreservesArguments(t *testing.T) {
	args := []string{"--enable-features=LiveCaption", "--disable-features=Translate"}
	if got := applyBrowserBackgroundDownloadPolicy(args, "off"); !reflect.DeepEqual(got, args) {
		t.Fatalf("off changed args: %v", got)
	}
}

func TestBrowserLaunchSanitizationPreventsProxyBypass(t *testing.T) {
	args := []string{"--proxy-server=http://evil.invalid", "--no-proxy-server", "--proxy-auto-detect", "--proxy-pac-url", "http://evil.invalid/pac", "--proxy-bypass-list=*", "--lang=en-US"}
	got, _ := sanitizeManagedLaunchArgs(args)
	if !reflect.DeepEqual(got, []string{"--lang=en-US"}) {
		t.Fatalf("proxy bypass survived: %v", got)
	}
}
