package config

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestBrowserDownloadPolicyDefaultsAndPersistence(t *testing.T) {
	for _, value := range []string{"", "strict", "unknown"} {
		if got := BrowserDownloadPolicy(&Config{Browser: BrowserConfig{BackgroundDownloadPolicy: value}}); got != BrowserDownloadsStrict {
			t.Fatalf("%q: %s", value, got)
		}
	}
	if BrowserDownloadPolicy(nil) != BrowserDownloadsStrict {
		t.Fatal("nil configuration must protect")
	}
	for _, value := range []string{"components", "off", "strict"} {
		input := Config{Browser: BrowserConfig{BackgroundDownloadPolicy: value}}
		data, err := yaml.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var output Config
		if err := yaml.Unmarshal(data, &output); err != nil {
			t.Fatal(err)
		}
		if BrowserDownloadPolicy(&output) != value {
			t.Fatalf("lost %s", value)
		}
	}
}
