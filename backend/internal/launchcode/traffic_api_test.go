package launchcode

import (
	"ant-chrome/backend/internal/browser"
	"ant-chrome/backend/internal/traffic"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type trafficStarterStub struct{ err error }

func (s trafficStarterStub) StartInstance(string) (*browser.Profile, error) { return nil, nil }
func (s trafficStarterStub) BrowserTrafficList() ([]traffic.Snapshot, error) {
	return []traffic.Snapshot{{ProfileID: "test", DownloadedBytes: 1024}}, s.err
}

func TestTrafficAPI(t *testing.T) {
	for _, tt := range []struct {
		name, method string
		provider     BrowserStarter
		status       int
	}{
		{"success", http.MethodGet, trafficStarterStub{}, 200},
		{"service failure", http.MethodGet, trafficStarterStub{err: errors.New("offline")}, 503},
		{"unsupported", http.MethodGet, nil, 501},
		{"read only", http.MethodPost, trafficStarterStub{}, 405},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := NewLaunchServer(nil, nil, nil, 0)
			s.starter = tt.provider
			w := httptest.NewRecorder()
			s.buildMux().ServeHTTP(w, httptest.NewRequest(tt.method, "/api/traffic", nil))
			if w.Code != tt.status {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
			if tt.status == 200 && !strings.Contains(w.Body.String(), `"downloadedBytes":1024`) {
				t.Fatal(w.Body.String())
			}
		})
	}
}
