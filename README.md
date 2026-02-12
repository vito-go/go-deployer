# go-deployer

<div align="center">

[![Go Version](https://img.shields.io/badge/Go-%3E%3D%201.21-blue)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](https://github.com/yourusername/go-deployer/pulls)

**Zero-downtime deployment platform for Go backends and frontend assets**

[English](README.md) | [中文文档](README_ZH.md)

</div>

---

## 🎯 Features

- 🚀 **Zero-downtime deployment** - Atomic swapping ensures no service interruption
- 📦 **Dual deployment modes** - Go backend processes + Frontend npm builds
- 🌐 **Unified web console** - Beautiful React-based management interface
- 🔐 **Flexible authentication** - PreHandler middleware for custom auth logic
- 📊 **Real-time logging** - SSE streaming for deployment progress
- 🔄 **Git integration** - Deploy any branch/commit with one click
- 🎨 **Modern UI** - Dark mode, responsive design, professional aesthetics
- 🛡️ **Production-ready** - Multi-instance management, graceful shutdown, process monitoring

## ⚠️ Important Prerequisites

**Your application MUST support SO_REUSEPORT** to enable multiple instances to listen on the same port simultaneously, which is essential for zero-downtime deployment.

### How to Enable SO_REUSEPORT in Go Applications

```go
package main

import (
    "context"
    "net"
    "net/http"
    "syscall"

    "golang.org/x/sys/unix"
)

func main() {
    // Create listener with SO_REUSEPORT enabled
    lc := net.ListenConfig{
        Control: func(network, address string, c syscall.RawConn) error {
            return c.Control(func(fd uintptr) {
                // Enable SO_REUSEPORT
                syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
            })
        },
    }

    ln, err := lc.Listen(context.Background(), "tcp", ":8080")
    if err != nil {
        panic(err)
    }

    // Start your HTTP server
    http.Serve(ln, yourHandler)
}
```

**Note**: Add `golang.org/x/sys` to your dependencies:
```bash
go get golang.org/x/sys/unix
```

---

## 🚀 Quick Start

### Installation

\`\`\`bash
go get github.com/vito-go/go-deployer
\`\`\`

### Basic Usage

\`\`\`go
package main

import (
    "net/http"
    "github.com/vito-go/go-deployer"
)

func main() {
    // Create configuration
    cfg, _ := deployer.NewConfig(deployer.ConfigParams{
        GithubRepo:     "git@github.com:yourorg/backend.git",
        Env:            "production",
        BuildEntry:     "./cmd/app",
        Port:           8001,
        BasePath:       "/deploy",
        FrontendGitURL: "git@github.com:yourorg/frontend.git",
    })

    // Create deployer and mount routes
    dep := deployer.NewDeployer(cfg)
    mux := http.NewServeMux()
    dep.Mount(mux)

    // Start server
    http.ListenAndServe(":8001", mux)
}
\`\`\`

Access at: **http://localhost:8001/deploy/**

## 📖 Documentation

See [README_ZH.md](README_ZH.md) for complete Chinese documentation with detailed examples, API reference, and best practices.

## ⚡ Zero-Downtime Mechanism

Traditional deployment:
\`\`\`bash
rm -rf dist/* && cp -r build/* dist/
         ↑ Users get 404 during this gap
\`\`\`

Our atomic swap:
\`\`\`bash
mv dist dist-old && mv dist-new dist  # ~1μs, atomic
\`\`\`

**Result**: Zero 404 errors, imperceptible to users.

## 🔒 Authentication Example

\`\`\`go
func authHandler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
    token := r.Header.Get("Authorization")
    if token != "Bearer secret" {
        http.Error(w, "Unauthorized", 401)
        return r, false
    }
    return r, true
}

dep := deployer.NewDeployer(cfg)
mux := http.NewServeMux()
dep.Mount(mux, authHandler)
\`\`\`

## 📄 License

MIT License - see [LICENSE](LICENSE) file.

---

**Made with ❤️ by the go-deployer community**
