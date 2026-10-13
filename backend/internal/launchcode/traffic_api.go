package launchcode

import (
	"ant-chrome/backend/internal/traffic"
	"net/http"
)

type browserTrafficProvider interface {
	BrowserTrafficList() ([]traffic.Snapshot, error)
}

// Traffic uses the existing local API authentication/localhost middleware.
func (s *LaunchServer) handleTraffic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "method not allowed"})
		return
	}
	provider, ok := s.starter.(browserTrafficProvider)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]interface{}{"ok": false, "error": "traffic statistics unavailable"})
		return
	}
	rows, err := provider.BrowserTrafficList()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "items": rows})
}
