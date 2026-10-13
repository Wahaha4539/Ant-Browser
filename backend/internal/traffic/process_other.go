//go:build !windows

package traffic

import (
	"os"
	"syscall"
)

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}
