package backend

import (
	"ant-chrome/backend/internal/traffic"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type BrowserTrafficSnapshot = traffic.Snapshot

// RunBrowserTrafficHelper is invoked before Wails/single-instance initialization.
// A separate lightweight process preserves connections on "quit app only".
func RunBrowserTrafficHelper(root string) error { return traffic.Serve(root) }

func (a *App) browserTrafficRoot() string {
	return ResolveRuntimePath(a.appRoot, filepath.Join("data", "traffic"))
}

func (a *App) ensureBrowserTrafficHelper() (traffic.Runtime, error) {
	root := a.browserTrafficRoot()
	info, err := traffic.ReadRuntime(root)
	if err == nil {
		if err = traffic.Call(info, "/health", nil, nil); err == nil {
			return info, nil
		}
		if isProcessAlive(info.PID) {
			return traffic.Runtime{}, fmt.Errorf("流量统计服务暂时不可用：%w", err)
		}
	} else if !os.IsNotExist(err) {
		return traffic.Runtime{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return traffic.Runtime{}, err
	}
	command := exec.Command(executable, traffic.HelperArgument, root)
	command.Dir = a.appRoot
	hideWindow(command)
	if err = command.Start(); err != nil {
		return traffic.Runtime{}, err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-exited:
			return traffic.Runtime{}, fmt.Errorf("流量统计服务启动失败：%v", err)
		case <-deadline.C:
			command.Process.Kill()
			return traffic.Runtime{}, fmt.Errorf("流量统计服务启动超时")
		case <-ticker.C:
			candidate, readErr := traffic.ReadRuntime(root)
			if readErr == nil && candidate.PID == command.Process.Pid && traffic.Call(candidate, "/health", nil, nil) == nil {
				return candidate, nil
			}
		}
	}
}

func (a *App) startProfileTraffic(profileID, proxyURL string, debugPort int) (string, bool, error) {
	if _, err := traffic.UpstreamAddress(proxyURL); err != nil {
		return proxyURL, false, nil
	}
	a.trafficMu.Lock()
	defer a.trafficMu.Unlock()
	info, err := a.ensureBrowserTrafficHelper()
	if err != nil {
		return "", false, err
	}
	var result traffic.StartResponse
	err = traffic.Call(info, "/start", traffic.StartRequest{ProfileID: profileID, ProxyURL: proxyURL, DebugPort: debugPort}, &result)
	if err != nil {
		return "", false, err
	}
	return result.ProxyURL, true, nil
}

func (a *App) setProfileTrafficOwner(profileID string, pid int) {
	if info, err := traffic.ReadRuntime(a.browserTrafficRoot()); err == nil {
		_ = traffic.Call(info, "/owner", traffic.OwnerRequest{ProfileID: profileID, PID: pid}, nil)
	}
}
func (a *App) stopProfileTraffic(profileID string) {
	a.trafficMu.Lock()
	defer a.trafficMu.Unlock()
	if info, err := traffic.ReadRuntime(a.browserTrafficRoot()); err == nil {
		_ = traffic.Call(info, "/stop", traffic.StopRequest{ProfileID: profileID}, nil)
	}
}
func (a *App) stopBrowserTrafficHelper() {
	a.trafficMu.Lock()
	defer a.trafficMu.Unlock()
	if info, err := traffic.ReadRuntime(a.browserTrafficRoot()); err == nil {
		_ = traffic.Call(info, "/shutdown", struct{}{}, nil)
	}
}

// BrowserTrafficList includes historical counters for stopped profiles. Missing
// coverage is explicit instead of presenting an unmeasured connection as 0 B.
func (a *App) BrowserTrafficList() ([]BrowserTrafficSnapshot, error) {
	if a == nil || a.browserMgr == nil {
		return []BrowserTrafficSnapshot{}, nil
	}
	root := a.browserTrafficRoot()
	var rows []BrowserTrafficSnapshot
	live := false
	if info, readErr := traffic.ReadRuntime(root); readErr == nil {
		if callErr := traffic.Call(info, "/snapshots", nil, &rows); callErr == nil {
			live = true
		} else if isProcessAlive(info.PID) {
			return nil, fmt.Errorf("流量统计暂不可用：%w", callErr)
		}
	} else if !os.IsNotExist(readErr) {
		return nil, readErr
	}
	if !live {
		var err error
		rows, err = traffic.ReadSnapshots(root)
		if err != nil {
			return nil, err
		}
	}
	byID := make(map[string]BrowserTrafficSnapshot, len(rows))
	for _, row := range rows {
		row.Domains = nil // Keep the periodically refreshed instance list lightweight.
		byID[row.ProfileID] = row
	}
	profiles := a.browserMgr.List()
	result := make([]BrowserTrafficSnapshot, 0, len(profiles))
	for _, profile := range profiles {
		row, found := byID[profile.ProfileId]
		row.ProfileID = profile.ProfileId
		if !found {
			row.State = "not_started"
			row.Message = "启动实例后开始统计"
		}
		if profile.Running && !row.Running {
			row.State = "unmetered"
			row.Message = "当前连接未计量：请重启实例；原生直连和 HTTPS 代理暂不支持"
		}
		result = append(result, row)
	}
	return result, nil
}

// BrowserTrafficDetail reads domain counters only when the detail view is open.
func (a *App) BrowserTrafficDetail(profileID string) (BrowserTrafficSnapshot, error) {
	rows, err := a.BrowserTrafficList()
	if err != nil {
		return BrowserTrafficSnapshot{}, err
	}
	var row BrowserTrafficSnapshot
	found := false
	for _, candidate := range rows {
		if candidate.ProfileID == profileID {
			row = candidate
			found = true
			break
		}
	}
	if !found {
		return BrowserTrafficSnapshot{}, fmt.Errorf("实例不存在")
	}
	root := a.browserTrafficRoot()
	info, readErr := traffic.ReadRuntime(root)
	live := false
	if readErr == nil {
		if callErr := traffic.Call(info, "/health", nil, nil); callErr == nil {
			live = true
		} else if isProcessAlive(info.PID) {
			return BrowserTrafficSnapshot{}, fmt.Errorf("域名流量统计暂不可用：%w", callErr)
		}
	} else if !os.IsNotExist(readErr) {
		return BrowserTrafficSnapshot{}, readErr
	}
	if live && row.DomainStatsAvailable {
		var detail BrowserTrafficSnapshot
		if err = traffic.Call(info, "/domains?profileId="+url.QueryEscape(profileID), nil, &detail); err != nil {
			return BrowserTrafficSnapshot{}, fmt.Errorf("域名流量统计暂不可用：%w", err)
		}
		// Preserve the browser manager's unmetered status, if applicable.
		if row.State == "unmetered" {
			detail.State = row.State
			detail.Message = row.Message
		}
		row = detail
	} else if !live {
		history, err := traffic.ReadSnapshots(root)
		if err != nil {
			return BrowserTrafficSnapshot{}, err
		}
		for _, candidate := range history {
			if candidate.ProfileID == profileID {
				row.Domains = candidate.Domains
				row.UploadedBytes = candidate.UploadedBytes
				row.DownloadedBytes = candidate.DownloadedBytes
				row.SessionUploadedBytes = candidate.SessionUploadedBytes
				row.SessionDownloadedBytes = candidate.SessionDownloadedBytes
				break
			}
		}
	}
	var up, down, sessionUp, sessionDown int64
	unknown := -1
	for i, domain := range row.Domains {
		up += domain.UploadedBytes
		down += domain.DownloadedBytes
		sessionUp += domain.SessionUploadedBytes
		sessionDown += domain.SessionDownloadedBytes
		if domain.Domain == "" {
			unknown = i
		}
	}
	if row.UploadedBytes > up || row.DownloadedBytes > down || row.SessionUploadedBytes > sessionUp || row.SessionDownloadedBytes > sessionDown {
		if unknown == -1 {
			unknown = len(row.Domains)
			row.Domains = append(row.Domains, traffic.DomainSnapshot{})
		}
		row.Domains[unknown].UploadedBytes += max(0, row.UploadedBytes-up)
		row.Domains[unknown].DownloadedBytes += max(0, row.DownloadedBytes-down)
		row.Domains[unknown].SessionUploadedBytes += max(0, row.SessionUploadedBytes-sessionUp)
		row.Domains[unknown].SessionDownloadedBytes += max(0, row.SessionDownloadedBytes-sessionDown)
	}
	if row.Domains == nil {
		row.Domains = []traffic.DomainSnapshot{}
	}
	return row, nil
}
