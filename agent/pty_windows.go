//go:build windows

package agent

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"unsafe"

	"github.com/vito-go/go-deployer/protocol"
	"golang.org/x/sys/windows"
)

var (
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole  = kernel32.NewProc("ClosePseudoConsole")
)

type conPTY struct {
	hpc     windows.Handle
	pipeIn  *os.File // write to this → goes to console stdin
	pipeOut *os.File // read from this ← console stdout
}

type ptySession struct {
	con  *conPTY
	proc windows.Handle
	once sync.Once
}

func createConPTY(cols, rows uint16) (*conPTY, error) {
	var hPipeInRead, hPipeInWrite windows.Handle
	if err := windows.CreatePipe(&hPipeInRead, &hPipeInWrite, nil, 0); err != nil {
		return nil, err
	}
	var hPipeOutRead, hPipeOutWrite windows.Handle
	if err := windows.CreatePipe(&hPipeOutRead, &hPipeOutWrite, nil, 0); err != nil {
		windows.CloseHandle(hPipeInRead)
		windows.CloseHandle(hPipeInWrite)
		return nil, err
	}

	coord := uintptr(cols) | (uintptr(rows) << 16)
	var hpc windows.Handle
	ret, _, _ := procCreatePseudoConsole.Call(
		coord,
		uintptr(hPipeInRead),
		uintptr(hPipeOutWrite),
		0,
		uintptr(unsafe.Pointer(&hpc)),
	)
	if ret != 0 {
		windows.CloseHandle(hPipeInRead)
		windows.CloseHandle(hPipeInWrite)
		windows.CloseHandle(hPipeOutRead)
		windows.CloseHandle(hPipeOutWrite)
		return nil, windows.Errno(ret)
	}

	// Close the console-side pipe ends (owned by the pseudo console now)
	windows.CloseHandle(hPipeInRead)
	windows.CloseHandle(hPipeOutWrite)

	return &conPTY{
		hpc:     hpc,
		pipeIn:  os.NewFile(uintptr(hPipeInWrite), "conpty-stdin"),
		pipeOut: os.NewFile(uintptr(hPipeOutRead), "conpty-stdout"),
	}, nil
}

func (c *conPTY) resize(cols, rows uint16) {
	coord := uintptr(cols) | (uintptr(rows) << 16)
	procResizePseudoConsole.Call(uintptr(c.hpc), coord)
}

func (c *conPTY) close() {
	procClosePseudoConsole.Call(uintptr(c.hpc))
	c.pipeIn.Close()
	c.pipeOut.Close()
}

func startProcessWithConPTY(con *conPTY) (windows.Handle, error) {
	cmdLine, _ := windows.UTF16PtrFromString("powershell.exe")

	attrList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, err
	}
	defer attrList.Delete()

	if err := attrList.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		unsafe.Pointer(con.hpc),
		unsafe.Sizeof(con.hpc),
	); err != nil {
		return 0, err
	}

	si := &windows.StartupInfoEx{
		StartupInfo:             windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))},
		ProcThreadAttributeList: attrList.List(),
	}

	pi := &windows.ProcessInformation{}
	err = windows.CreateProcess(
		nil, cmdLine, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT,
		nil, nil, &si.StartupInfo, pi,
	)
	if err != nil {
		return 0, err
	}
	windows.CloseHandle(pi.Thread)
	return pi.Process, nil
}

func (a *agentClient) handlePtyStart(msg protocol.Message) {
	con, err := createConPTY(120, 30)
	if err != nil {
		a.sendLog(msg.ID, "ERR: Failed to create ConPTY: "+err.Error())
		return
	}

	proc, err := startProcessWithConPTY(con)
	if err != nil {
		con.close()
		a.sendLog(msg.ID, "ERR: Failed to start PowerShell: "+err.Error())
		return
	}

	session := &ptySession{con: con, proc: proc}

	a.ptyMu.Lock()
	a.ptySessions[msg.ID] = session
	a.ptyMu.Unlock()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := con.pipeOut.Read(buf)
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

	_, _ = session.con.pipeIn.WriteString(req.Data)
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

	session.con.resize(req.Cols, req.Rows)
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
			session.con.close()
			windows.TerminateProcess(session.proc, 0)
			windows.CloseHandle(session.proc)
		})
	}
}

func (a *agentClient) startSSHServer() {}
