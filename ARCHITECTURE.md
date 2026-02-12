# Architecture Documentation

This document provides a detailed technical overview of go-deployer's architecture and design decisions.

## 🏛️ System Architecture

### High-Level Overview

```
┌─────────────────────────────────────────────────────────────┐
│                     HTTP Request                             │
└──────────────────────┬──────────────────────────────────────┘
                       ↓
┌─────────────────────────────────────────────────────────────┐
│                 PreHandler Chain                             │
│  (Authentication → Authorization → Logging → ...)            │
└──────────────────────┬──────────────────────────────────────┘
                       ↓
┌─────────────────────────────────────────────────────────────┐
│                   Route Handler                              │
│  ┌─────────────────┐           ┌─────────────────┐          │
│  │   Backend       │           │   Frontend      │          │
│  │   Deployment    │           │   Build         │          │
│  └────────┬────────┘           └────────┬────────┘          │
│           │                              │                   │
│           ↓                              ↓                   │
│  ┌─────────────────┐           ┌─────────────────┐          │
│  │ Git Operations  │           │ npm Operations  │          │
│  │ Go Build        │           │ Atomic Swap     │          │
│  │ Process Control │           │ File Deploy     │          │
│  └─────────────────┘           └─────────────────┘          │
└─────────────────────────────────────────────────────────────┘
                       ↓
┌─────────────────────────────────────────────────────────────┐
│                  File System Layer                           │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
│  │   repo/  │  │   bin/   │  │ worker/  │  │metadata/ │   │
│  │  (Git)   │  │(Binaries)│  │ (Runtime)│  │  (JSON)  │   │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘   │
└─────────────────────────────────────────────────────────────┘
```

## 📦 Core Components

### 1. Config System

**Purpose**: Centralized configuration management for all deployment operations.

**Key Responsibilities**:
- Parse and validate configuration parameters
- Generate derived paths (bin/, worker/, metadata/)
- Provide immutable configuration to all components

**Design Decisions**:
- **Immutability**: Config is read-only after creation, preventing accidental modifications
- **Path Generation**: Automatically derives all required paths from base directory
- **Validation**: Validates critical fields at creation time (fail-fast principle)

```go
type Config struct {
    // User-provided fields
    GithubRepo      string
    Env             string
    BuildEntry      string
    Port            uint
    BasePath        string
    FrontendGitURL  string

    // Auto-generated fields
    BaseDir         string
    RepoDir         string
    BinDir          string
    WorkerDir       string
    MetadataDir     string
    FrontendDir     string
    FrontendDistDir string
}
```

### 2. PreHandler Middleware

**Purpose**: Flexible request preprocessing before route handlers execute.

**Signature**:
```go
type PreHandler func(w http.ResponseWriter, r *http.Request) (newR *http.Request, next bool)
```

**Execution Flow**:
```
Request → PreHandler₁ → PreHandler₂ → ... → PreHandlerₙ → Route Handler
           ↓ false (stop)
        Response
```

**Design Pattern**: Chain of Responsibility
- Each handler can modify the request
- Each handler decides whether to continue the chain
- Handlers are executed in registration order

**Common Use Cases**:
1. **Authentication**: Verify tokens, set user context
2. **Authorization**: Check permissions
3. **Logging**: Record access patterns
4. **Rate Limiting**: Throttle requests
5. **Request Enrichment**: Add headers, context values

### 3. Deployment Pipeline

#### Backend Deployment

**Steps**:
1. **Git Sync** - Fetch latest code, checkout specific commit
2. **Build** - Compile Go binary with optimizations
3. **Launch** - Start new process in background with `nohup`
4. **Metadata** - Record instance info (PID, version, timestamp)

**Zero-Downtime Strategy**:
- New instances start before old instances stop
- Multiple instances can run simultaneously
- Load balancer handles traffic distribution
- Old instances are manually killed via UI

**Error Handling**:
- Each step validates success before proceeding
- Logs are streamed in real-time via SSE
- Failed deployments don't affect running instances

#### Frontend Deployment

**Steps**:
1. **Git Sync** - Fetch frontend code, checkout commit
2. **Dependencies** - Run `npm install`
3. **Build** - Execute `npm run build`
4. **Atomic Swap** - Replace dist directory atomically
5. **Metadata** - Record build info

**Atomic Swap Mechanism**:

Traditional approach (has downtime):
```bash
rm -rf dist/          # ← Directory empty here (404s!)
cp -r build/* dist/
```

Our approach (zero downtime):
```bash
cp -r build/* dist-new/           # 1. Copy to temp
mv dist/ dist-old/                # 2. Atomic rename
mv dist-new/ dist/                # 3. Atomic rename
rm -rf dist-old/                  # 4. Cleanup (async)
```

**Why Atomic?**
- `mv` on same filesystem is a single syscall (~1μs)
- Directory always exists - no 404 window
- If step 2 fails, step 3 won't execute (transactional)
- OS guarantees atomicity (POSIX standard)

### 4. Instance Management

**Metadata Structure**:
```json
{
  "pid": 12345,
  "version": "abc1234",
  "uptime": "2h 30m 15s",
  "deployedAt": "2026-02-12 10:30:00"
}
```

**Heartbeat System**:
- Background goroutine updates metadata every 3 seconds
- Metadata includes uptime calculation
- Stale files are cleaned up automatically

**Process Detection**:
```bash
lsof -t -i:8001 -sTCP:LISTEN
```
- Lists PIDs listening on configured port
- Used to validate instance health
- Removes metadata for dead processes

**Safety Mechanisms**:
1. **Last Instance Protection**: Prevents killing the only running instance
2. **Permission Check**: Only allows killing PIDs with metadata files
3. **Graceful Shutdown**: Uses `SIGTERM` (not `SIGKILL`)

### 5. Git Integration

**Branch Management**:
```bash
git fetch --all --prune          # Sync with remote
git branch -r                    # List remote branches
```

**Commit History**:
```bash
git log origin/<branch> -n 10 --pretty=format:%h|%an|%ar|%s
```
Format: `hash|author|relative_date|message`

**Checkout Strategy**:
```bash
git checkout -f <branch>         # Force checkout (ignore local changes)
git reset --hard <commit>        # Hard reset to specific commit
```

**Why Force Reset?**
- Deployment environment should match source exactly
- Local modifications are not supported
- Prevents merge conflicts during deployment

### 6. Web UI System

**Technology Stack**:
- **React 18** (via CDN) - UI framework
- **Tailwind CSS** (via CDN) - Styling
- **Server-Sent Events** - Real-time log streaming
- **Local Storage** - Theme persistence

**Embedded Files**:
```go
//go:embed index.html backend.html frontend.html
var embedFS embed.FS
```

**Benefits**:
- Single binary distribution
- No external file dependencies
- Version-locked UI (no CDN issues)

**Path Resolution**:
```javascript
const BASE_PATH = window.location.pathname.replace(/\/$/, '');
// Example: /deploy/backend/ → /deploy/backend
```

Relative links work correctly because routes end with `/`:
```html
<a href="../">Home</a>          <!-- /deploy/backend/ → /deploy/ -->
<a href="backend/">Backend</a>  <!-- /deploy/ → /deploy/backend/ -->
```

## 🔒 Security Considerations

### 1. PreHandler Authentication

**Recommended Approach**:
```go
func jwtAuth(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
    token := extractToken(r)
    if token == "" {
        http.Error(w, "Unauthorized", 401)
        return r, false
    }

    claims, err := validateJWT(token)
    if err != nil {
        http.Error(w, "Invalid token", 401)
        return r, false
    }

    ctx := context.WithValue(r.Context(), "user", claims)
    return r.WithContext(ctx), true
}
```

### 2. Process Isolation

**PID Namespace**:
```go
newCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
```
- Creates new process session
- Prevents signal propagation from parent
- Allows independent process lifecycle

### 3. File System Security

**Directory Permissions**:
```go
os.MkdirAll(dir, 0755)  // rwxr-xr-x
```
- Owner has full control
- Others have read+execute only
- Prevents unauthorized modifications

**Metadata Protection**:
- Only PIDs with metadata files can be killed
- Prevents killing arbitrary processes
- Audit trail for all operations

## 🚀 Performance Characteristics

### Build Performance

**Optimization Flags**:
```bash
go build -ldflags "-s -w" -trimpath
```
- `-s`: Strip symbol table
- `-w`: Strip DWARF debug info
- `-trimpath`: Remove build path from binary
- **Result**: ~30% smaller binaries

### Concurrent Operations

**Goroutine Usage**:
1. Heartbeat loop (per instance)
2. Deployment pipeline (per deploy)
3. Frontend build (per build)
4. SSE log streaming (per client)

**Channel Buffering**:
```go
logChan: make(chan string, 100)
```
- Buffer size: 100 messages
- Prevents blocking on slow clients
- Handles burst log output

### Memory Management

**Embedded Files**:
- Loaded once at startup
- Shared across all requests
- No repeated disk I/O

**Metadata Caching**:
- Read from disk on each status check
- No in-memory cache (simplicity over speed)
- Acceptable for infrequent operations

## 🔄 State Management

### Stateless Design

**No in-memory state** (except config and channels):
- Instance info → Metadata files
- Build info → Metadata files
- Process list → `lsof` command

**Benefits**:
- Survives process restarts
- No state synchronization needed
- Simple debugging (inspect files)

### Eventual Consistency

**Heartbeat Interval**: 3 seconds
- Uptime updates every 3s
- Acceptable staleness for deployment UI
- Reduces disk I/O overhead

## 📊 Error Handling Strategy

### Error Categories

1. **Configuration Errors** - Fail fast at startup
2. **Git Errors** - Log and return error response
3. **Build Errors** - Stream error logs, mark deployment failed
4. **Process Errors** - Log and continue (already running instances unaffected)

### Logging Levels

```go
mylog.Info()   // Normal operations
mylog.Warn()   // Recoverable issues
mylog.Error()  // Unexpected errors (but handled)
mylog.Fatal()  // Unrecoverable errors (exit process)
```

## 🧪 Testing Strategy

### Unit Tests
- Config validation
- Path generation
- PreHandler chaining
- Metadata parsing

### Integration Tests
- Git operations (clone, fetch, checkout)
- File system operations (atomic swap)
- Process management (start, kill, detect)

### Manual Tests
- Full deployment workflow
- Concurrent deployments
- UI interactions
- SSE log streaming

---

**Last Updated**: 2026-02-12
**Version**: 1.0.0
