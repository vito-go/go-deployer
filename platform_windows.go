//go:build windows

package deployer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Windows 下 Deployer 充当 supervisor:
//
//  1. 首次部署:deployer spawn 子进程,记录 PID(内存 + 磁盘 child.pid)
//  2. 新版本到来:
//       a. 读 childPID,TerminateProcess 干掉老的
//       b. 等老的释放端口(poll 到端口可 bind 为止,或固定 sleep)
//       c. spawn 新子进程(DETACHED_PROCESS + HideWindow),新 PID 落盘
//       d. 可选 health check:轮询 /health 直到 200
//  3. deployer 自身重启时:从 child.pid 恢复 PID(但不重新 spawn,因为 detached
//     child 本来就独立运行,deployer 重启只是重新持有 PID 引用)
//
// 代价:3-5s 端口空窗期,client 靠 P2P cluster 切到其他节点。
// 优势:无须 Windows 实现 SO_REUSEPORT(不可能),无须 SSH 权限,沿用 HTTP 部署接口。

const childPIDFile = "child.pid"

func (s *Deployer) platformStart(targetPath string) error {
	// 1. kill 旧子进程(如果 tracked 到 PID)
	s.childMu.Lock()
	oldPID := s.childPID
	if oldPID == 0 {
		oldPID = s.readChildPIDFile()
	}
	s.childMu.Unlock()

	if oldPID > 0 {
		if p, err := os.FindProcess(oldPID); err == nil {
			s.logChan <- fmt.Sprintf(">> KILL old child pid=%d", oldPID)
			_ = p.Kill()
			// Wait 最多 5s,避免 zombie + 让端口释放
			done := make(chan struct{})
			go func() { _, _ = p.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				s.logChan <- "WARN: old child did not exit within 5s"
			}
		}
	}

	// 2. 给端口让点时间释放(Windows TIME_WAIT 有时拖)
	time.Sleep(500 * time.Millisecond)

	// 3. spawn 新子进程
	args := strings.Fields(s.cfg.appArgs)
	cmd := exec.Command(targetPath, args...)
	cmd.Dir = s.cfg.workingDir

	// 日志重定向到文件;subsystem=windowsgui 的 exe 没有 console 可输出
	logPath := filepath.Join(s.cfg.workingDir, "app.log")
	if lf, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
	}

	// DETACHED_PROCESS: 子进程和 deployer 父进程解耦,deployer 重启子不受影响
	// HideWindow: 防万一子进程想弹 console,不显示
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008, // DETACHED_PROCESS
		HideWindow:    true,
	}

	s.logChan <- ">> RUN: " + targetPath + " " + s.cfg.appArgs
	if err := cmd.Start(); err != nil {
		return err
	}

	newPID := cmd.Process.Pid
	s.childMu.Lock()
	s.childPID = newPID
	s.childMu.Unlock()
	_ = s.writeChildPIDFile(newPID)
	s.logChan <- fmt.Sprintf("   new child pid=%d", newPID)

	// 4. 后台 reap,避免 Go 进程表 zombie
	go func() { _ = cmd.Wait() }()

	return nil
}

// killManagedPID: Windows 下用 Process.Kill (TerminateProcess)。
func killManagedPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// countPortListeners: Windows 下 deployer 自己就是唯一 supervisor,同一时刻
// 至多 1 个子进程监听端口。为了让 /kill 端点的"last instance standing"
// 保护不误伤正常情况,这里恒返 2(允许 kill)。
// 语义退化:Windows 没有 SO_REUSEPORT 多实例共存场景,所以"last instance"
// 这个 Linux 专属的并发存留概念本来就不适用。
func countPortListeners(port uint) int {
	return 2
}

func (s *Deployer) writeChildPIDFile(pid int) error {
	path := filepath.Join(s.cfg.metadataDir, childPIDFile)
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0644)
}

func (s *Deployer) readChildPIDFile() int {
	path := filepath.Join(s.cfg.metadataDir, childPIDFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}
