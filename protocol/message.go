package protocol

import "encoding/json"

// Message is the envelope for all WebSocket communication between agent and control plane.
type Message struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"` // request ID for request-response pairing
	Data json.RawMessage `json:"data,omitempty"`
}

// --- Agent → Control Plane ---

const (
	TypeRegister = "register"
	TypeLog      = "log"
	TypeFiles    = "files" // agent → control plane: available binary files
)

// --- Control Plane → Agent ---

const (
	TypeDeploy        = "deploy"
	TypeRestart       = "restart"
	TypeKill          = "kill"
	TypeRollback      = "rollback"
	TypeExec          = "exec"
	TypeComplete      = "complete"
	TypeResourceStart = "resource_start"
	TypeResourceStop  = "resource_stop"
	TypeResourceData  = "resource_data"
	TypePtyStart      = "pty_start"
	TypePtyInput      = "pty_input"
	TypePtyOutput     = "pty_output"
	TypePtyResize     = "pty_resize"
	TypePtyClose      = "pty_close"
	TypeFetchFiles    = "fetch_files" // control plane → agent: list available binaries
)

// RegisterData is sent by the agent when it first connects.
type RegisterData struct {
	ServiceName string `json:"serviceName"`
	Group       string `json:"group,omitempty"`
	BinaryDir   string `json:"binaryDir,omitempty"`
	Host        string `json:"host"`
	PID         int    `json:"pid"`
	Port        uint   `json:"port"`
	Version     string `json:"version"`
	CommitHash  string `json:"commitHash,omitempty"`
	CommitTime  string `json:"commitTime,omitempty"`
	GoVersion   string `json:"goVersion,omitempty"`
	StartTimeMs int64  `json:"startTimeMs"`
	ExePath     string `json:"exePath"`
	AppArgs     string `json:"appArgs,omitempty"`
}

type LogData struct {
	Line string `json:"line"`
}

type DeployData struct {
	FileName string `json:"fileName"`           // binary file to deploy
	FileHash string `json:"fileHash,omitempty"` // MD5 hash for download URL
	AppArgs  string `json:"appArgs,omitempty"`  // override agent default AppArgs if non-empty
}

type RestartData struct {
	AppArgs string `json:"appArgs,omitempty"`
}

type RollbackData struct{}

type ExecData struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
}

type KillData struct {
	PID int `json:"pid"`
}

type CompleteData struct {
	Input string `json:"input"`
	Cwd   string `json:"cwd,omitempty"`
}

type ResourceData struct {
	PID          int     `json:"pid"`
	CPUPercent   float64 `json:"cpuPercent"`
	MemRSS       uint64  `json:"memRSS"`
	MemUsed      uint64  `json:"memUsed"`
	MemTotal     uint64  `json:"memTotal"`
	NumGoroutine int     `json:"numGoroutine"`
}

type PtyInputData struct {
	Data string `json:"data"`
}

type PtyOutputData struct {
	Data string `json:"data"`
}

type PtyResizeData struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type CompleteResult struct {
	Candidates []string `json:"candidates"`
}

type FileInfo struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

type FilesData struct {
	Files []FileInfo `json:"files"`
}
