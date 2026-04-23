//go:build !windows

package deployer

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// platformStart: Unix 下用 nohup + setsid 拉起新进程,同时老进程继续运行,
// 依赖 SO_REUSEPORT 让新老共存几秒,老进程通过 /kill 端点或自然 drain 后退出。
// 实现 zero-downtime hand-off。
func (s *Deployer) platformStart(targetPath string) error {
	fullCmd := fmt.Sprintf("nohup %s %s > /dev/null 2>&1 &", targetPath, s.cfg.appArgs)
	newCmd := exec.Command("sh", "-c", fullCmd)
	newCmd.Dir = s.cfg.workingDir
	newCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	s.logChan <- ">> RUN: " + fullCmd
	if err := newCmd.Start(); err != nil {
		return err
	}
	_ = newCmd.Wait() // sh -c 一启完就退,不 block
	return nil
}

// killManagedPID: Unix 下发 SIGTERM。
func killManagedPID(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// countPortListeners: Unix 下用 lsof 数端口上的 LISTEN 进程。
// 支持 SO_REUSEPORT 场景下的多实例并存计数。
func countPortListeners(port uint) int {
	out, _ := exec.Command("sh", "-c", fmt.Sprintf("lsof -t -i:%d -sTCP:LISTEN", port)).Output()
	return len(strings.Fields(string(out)))
}
