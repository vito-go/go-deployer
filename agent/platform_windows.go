//go:build windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func getSystemMemory() uint64 {
	var memStatus [64]byte // MEMORYSTATUSEX
	*(*uint32)(unsafe.Pointer(&memStatus[0])) = uint32(len(memStatus))
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("GlobalMemoryStatusEx")
	ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&memStatus[0])))
	if ret == 0 {
		return 0
	}
	return *(*uint64)(unsafe.Pointer(&memStatus[8])) // ullTotalPhys at offset 8
}

func getSystemMemUsed() uint64 {
	var memStatus [64]byte // MEMORYSTATUSEX
	*(*uint32)(unsafe.Pointer(&memStatus[0])) = uint32(len(memStatus))
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("GlobalMemoryStatusEx")
	ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&memStatus[0])))
	if ret == 0 {
		return 0
	}
	total := *(*uint64)(unsafe.Pointer(&memStatus[8]))  // ullTotalPhys
	avail := *(*uint64)(unsafe.Pointer(&memStatus[16])) // ullAvailPhys
	return total - avail
}

// hiddenCmd 包 exec.Command,加 CREATE_NO_WINDOW 防止在 GUI subsystem
// (-H windowsgui) 父进程下,fork console 子进程(wmic/taskkill/cmd)时
// 弹出一闪而过的黑窗——父进程无 console 时 Windows 会给 console 子进程
// 新建一个 console 窗口。
func hiddenCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	return cmd
}

func collectProcessStats(pid int) (cpuPercent float64, memRSS uint64) {
	// Use wmic to get working set size
	out, err := hiddenCmd("wmic", "process", "where",
		fmt.Sprintf("ProcessId=%d", pid), "get", "WorkingSetSize", "/value").Output()
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "WorkingSetSize=") {
			v, _ := strconv.ParseUint(strings.TrimPrefix(line, "WorkingSetSize="), 10, 64)
			memRSS = v
		}
	}
	return 0, memRSS
}

func startBackgroundProcess(binPath, appArgs, workDir string) error {
	args := splitArgs(appArgs)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd.Start()
}

func killProcess(pid int) error {
	return hiddenCmd("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

func selfTerminate() {
	// Try graceful shutdown first (taskkill without /F sends WM_CLOSE)
	hiddenCmd("taskkill", "/PID", strconv.Itoa(os.Getpid())).Run()
	// Give business code 10s to clean up
	time.Sleep(10 * time.Second)
	// Fallback 1: os.Exit
	go os.Exit(0)
	time.Sleep(500 * time.Millisecond)
	// Fallback 2: force kill self via taskkill /F
	hiddenCmd("taskkill", "/F", "/PID", strconv.Itoa(os.Getpid())).Run()
	time.Sleep(500 * time.Millisecond)
	// Fallback 3: os.Exit again (in case something held it up)
	os.Exit(1)
}

func shellCommand(cmdStr string) *exec.Cmd {
	return hiddenCmd("cmd", "/C", cmdStr)
}

func shellComplete(word, dir string, isCommand bool) []string {
	// No equivalent to bash compgen on Windows
	return nil
}
