//go:build !windows

package agent

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/creack/pty"
	"github.com/vito-go/go-deployer/protocol"
)

type ptySession struct {
	tty  *os.File
	cmd  *exec.Cmd
	once sync.Once
}

func (a *agentClient) handlePtyStart(msg protocol.Message) {
	// macOS defaults to zsh since Catalina; Linux typically uses bash.
	var shells []string
	if runtime.GOOS == "darwin" {
		shells = []string{"zsh", "bash", "sh"}
	} else {
		shells = []string{"bash", "sh"}
	}
	var shellPath string
	for _, name := range shells {
		if p, err := exec.LookPath(name); err == nil {
			shellPath = p
			break
		}
	}
	if shellPath == "" {
		a.sendLog(msg.ID, "ERR: no shell found")
		return
	}
	shell := exec.Command(shellPath)
	// Set TERM for proper terminal support (clear, htop, vi, etc.).
	// Filter any pre-existing TERM first — on some systems (e.g. macOS
	// daemons) it defaults to "unknown", and duplicate env keys cause
	// the shell to pick the first (wrong) one.
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TERM=") {
			env = append(env, e)
		}
	}
	shell.Env = append(env, "TERM=xterm-256color")

	tty, err := pty.Start(shell)
	if err != nil {
		a.sendLog(msg.ID, "ERR: Failed to start PTY: "+err.Error())
		return
	}

	session := &ptySession{tty: tty, cmd: shell}

	a.ptyMu.Lock()
	a.ptySessions[msg.ID] = session
	a.ptyMu.Unlock()

	// Read PTY output and send to control plane
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				_ = a.sendMsg(protocol.TypePtyOutput, msg.ID, protocol.PtyOutputData{
					Data: string(buf[:n]),
				})
			}
			if err != nil {
				if err != io.EOF {
					_ = err
				}
				break
			}
		}
		// PTY closed
		_ = a.sendMsg(protocol.TypePtyClose, msg.ID, nil)
		a.cleanupPty(msg.ID)
	}()
}

func (a *agentClient) handlePtyInput(msg protocol.Message) {
	var req protocol.PtyInputData
	_ = json.Unmarshal(msg.Data, &req)

	a.ptyMu.Lock()
	session, ok := a.ptySessions[msg.ID]
	a.ptyMu.Unlock()
	if !ok {
		return
	}

	_, _ = session.tty.WriteString(req.Data)
}

func (a *agentClient) handlePtyResize(msg protocol.Message) {
	var req protocol.PtyResizeData
	_ = json.Unmarshal(msg.Data, &req)

	a.ptyMu.Lock()
	session, ok := a.ptySessions[msg.ID]
	a.ptyMu.Unlock()
	if !ok {
		return
	}

	ws := struct {
		Height uint16
		Width  uint16
		x      uint16
		y      uint16
	}{Height: req.Rows, Width: req.Cols}
	syscall.Syscall(syscall.SYS_IOCTL, session.tty.Fd(), uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(&ws)))
}

func (a *agentClient) handlePtyClose(msg protocol.Message) {
	a.cleanupPty(msg.ID)
}

func (a *agentClient) cleanupPty(sessionID string) {
	a.ptyMu.Lock()
	session, ok := a.ptySessions[sessionID]
	if ok {
		delete(a.ptySessions, sessionID)
	}
	a.ptyMu.Unlock()

	if ok {
		session.once.Do(func() {
			session.tty.Close()
			if session.cmd.Process != nil {
				session.cmd.Process.Wait()
			}
			_ = sessionID
		})
	}
}
