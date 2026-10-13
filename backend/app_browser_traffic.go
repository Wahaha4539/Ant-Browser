package backend

import (
	"ant-chrome/backend/internal/traffic"
	"fmt"
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
