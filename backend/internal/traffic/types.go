// Package traffic meters each browser's TCP proxy stream without changing its
// upstream connector. The helper outlives the UI when browsers are kept open.
package traffic

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const HelperArgument = "--browser-traffic-helper"

type Snapshot struct {
	ProfileID              string  `json:"profileId"`
	UploadedBytes          int64   `json:"uploadedBytes"`
	DownloadedBytes        int64   `json:"downloadedBytes"`
	SessionUploadedBytes   int64   `json:"sessionUploadedBytes"`
	SessionDownloadedBytes int64   `json:"sessionDownloadedBytes"`
	UploadBytesPerSecond   float64 `json:"uploadBytesPerSecond"`
	DownloadBytesPerSecond float64 `json:"downloadBytesPerSecond"`
	StartedAt              string  `json:"startedAt"`
	UpdatedAt              string  `json:"updatedAt"`
	Running                bool    `json:"running"`
	State                  string  `json:"state"`
	Message                string  `json:"message"`
}

type Runtime struct {
	PID     int    `json:"pid"`
	Address string `json:"address"`
	Token   string `json:"token"`
}

type StartRequest struct {
	ProfileID string `json:"profileId"`
	ProxyURL  string `json:"proxyUrl"`
	DebugPort int    `json:"debugPort"`
}

type StartResponse struct {
	ProxyURL string `json:"proxyUrl"`
}
type OwnerRequest struct {
	ProfileID string `json:"profileId"`
	PID       int    `json:"pid"`
}
type StopRequest struct {
	ProfileID string `json:"profileId"`
}

// HTTPS proxies cannot be transparently renamed to localhost: doing so changes
// TLS SNI/certificate validation. Direct/UDP traffic is deliberately not claimed.
func UpstreamAddress(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Hostname() == "" {
		return "", fmt.Errorf("此连接不支持 TCP 代理流量计量")
	}
	port := u.Port()
	switch strings.ToLower(u.Scheme) {
	case "socks", "socks5", "socks5h":
		if port == "" {
			port = "1080"
		}
	case "http":
		if port == "" {
			port = "80"
		}
	default:
		return "", fmt.Errorf("此连接不支持 TCP 代理流量计量")
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

func ReadRuntime(root string) (Runtime, error) {
	var r Runtime
	b, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(b, &r)
	host, _, splitErr := net.SplitHostPort(r.Address)
	if err == nil && (splitErr != nil || host != "127.0.0.1" || len(r.Token) < 32) {
		err = fmt.Errorf("invalid traffic helper endpoint")
	}
	return r, err
}

func ReadSnapshots(root string) ([]Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(root, "totals.json"))
	if os.IsNotExist(err) {
		return []Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []Snapshot
	if err = json.Unmarshal(b, &rows); err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Running = false
		rows[i].State = "stopped"
		rows[i].UploadBytesPerSecond = 0
		rows[i].DownloadBytesPerSecond = 0
	}
	return rows, nil
}

func writeJSONFile(filename string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(filename), ".traffic-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, filename)
}
