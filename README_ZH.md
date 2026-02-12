# go-deployer

<div align="center">

[![Go Version](https://img.shields.io/badge/Go-%3E%3D%201.21-blue)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](https://github.com/vito-go/go-deployer/pulls)

**零停机的 Go 后端和前端资源部署平台**

[English](README.md) | [中文文档](README_ZH.md)

</div>

---

## 🎯 核心特性

- 🚀 **零停机部署** - 原子交换确保服务无中断
- 📦 **双模式部署** - Go 后端进程 + 前端 npm 构建
- 🌐 **统一管理控制台** - 精美的 React 管理界面
- 🔐 **灵活的认证机制** - PreHandler 中间件支持自定义认证逻辑
- 📊 **实时日志流** - SSE 流式传输部署进度
- 🔄 **Git 集成** - 一键部署任意分支/提交
- 🎨 **现代化 UI** - 深色模式、响应式设计、专业美观
- 🛡️ **生产就绪** - 多实例管理、优雅关闭、进程监控

## 📖 目录

- [快速开始](#-快速开始)
- [架构设计](#-架构设计)
- [配置说明](#-配置说明)
- [API 文档](#-api-文档)
- [零停机机制](#-零停机机制)
- [使用示例](#-使用示例)
- [PreHandler 中间件](#-prehandler-中间件)
- [Web UI 功能](#-web-ui-功能)
- [最佳实践](#-最佳实践)
- [故障排查](#-故障排查)
- [贡献指南](#-贡献指南)

## ⚠️ 重要前提条件

**你的应用必须支持 SO_REUSEPORT**，这样多个进程才能同时监听同一端口，实现零停机部署。

### 如何在 Go 应用中启用 SO_REUSEPORT

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
    // 创建支持 SO_REUSEPORT 的监听器
    lc := net.ListenConfig{
        Control: func(network, address string, c syscall.RawConn) error {
            return c.Control(func(fd uintptr) {
                // 启用 SO_REUSEPORT
                syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
            })
        },
    }

    ln, err := lc.Listen(context.Background(), "tcp", ":8080")
    if err != nil {
        panic(err)
    }

    // 启动 HTTP 服务器
    http.Serve(ln, yourHandler)
}
```

**注意**：需要添加 `golang.org/x/sys` 依赖：
```bash
go get golang.org/x/sys/unix
```

---

## 🚀 快速开始

### 安装

\`\`\`bash
go get github.com/vito-go/go-deployer
\`\`\`

### 基础用法

\`\`\`go
package main

import (
    "net/http"
    deployer "github.com/vito-go/go-deployer"
)

func main() {
    // 1. 创建配置
    cfg, err := deployer.NewConfig(deployer.ConfigParams{
        GithubRepo:     "git@github.com:yourorg/backend.git",
        Env:            "production",
        BuildEntry:     "./cmd/app",
        AppArgs:        "",
        Port:           8001,
        BasePath:       "/deploy",
        FrontendGitURL: "git@github.com:yourorg/frontend.git",
    })
    if err != nil {
        panic(err)
    }

    // 2. 创建部署器
    dep := deployer.NewDeployer(cfg)

    // 3. 创建 HTTP 路由器并挂载路由
    mux := http.NewServeMux()
    dep.Mount(mux)

    // 4. 启动服务器
    http.ListenAndServe(":8001", mux)
}
\`\`\`

### 添加认证

\`\`\`go
// 定义认证处理器
func authHandler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
    // 检查授权（JWT、Basic Auth 等）
    token := r.Header.Get("Authorization")
    if token != "Bearer secret-token" {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return r, false // 不继续处理
    }
    return r, true // 继续下一个处理器
}

// 创建部署器并传递认证处理器
dep := deployer.NewDeployer(cfg)
mux := http.NewServeMux()
dep.Mount(mux, authHandler)
\`\`\`

### 访问控制台

在浏览器中打开：
\`\`\`
http://localhost:8001/deploy/
\`\`\`

你将看到一个精美的导航界面，包含两个选项：
- **后端部署** - 管理 Go 进程实例
- **前端构建** - 构建和部署前端资源

## 🏗️ 架构设计

### 目录结构

\`\`\`
~/.{项目名}/{环境名}/
├── repo/                        # 后端 Git 仓库
├── bin/                         # 编译的 Go 二进制文件
│   ├── app-abc1234              # 版本 abc1234
│   └── app-def5678              # 版本 def5678
├── worker/                      # 后端工作目录
│   └── dist/                    # 前端静态文件（部署到这里）
├── metadata/                    # 实例元数据
│   ├── 12345.json              # 后端实例信息
│   └── frontend-build.json     # 前端构建信息
└── frontend-repo/               # 前端 Git 仓库
\`\`\`

### 组件概览

\`\`\`
┌─────────────────────────────────────────────┐
│               导航首页                       │
│  ┌──────────────┐    ┌──────────────┐      │
│  │   后端部署   │    │   前端构建   │      │
│  └──────────────┘    └──────────────┘      │
└─────────────────────────────────────────────┘
         │                      │
         ▼                      ▼
┌─────────────────┐    ┌─────────────────┐
│    后端部署     │    │    前端构建     │
│ ├─ Git 拉取    │    │ ├─ Git 拉取    │
│ ├─ Go 构建     │    │ ├─ npm install │
│ ├─ 启动进程    │    │ ├─ npm build   │
│ └─ 监控 PID    │    │ └─ 原子交换    │
└─────────────────┘    └─────────────────┘
\`\`\`

## ⚙️ 配置说明

### 配置结构

\`\`\`go
type Config struct {
    GithubRepo      string    // 后端 Git 仓库 URL
    Env             string    // 环境名称（dev, staging, prod）
    BuildEntry      string    // Go 构建入口（如 ./cmd/app）
    AppArgs         string    // 应用参数
    Port            uint      // 服务器端口
    BasePath        string    // 基础 URL 路径（必填，无默认值）
    FrontendGitURL  string    // 前端 Git 仓库 URL（可选）

    // 自动生成的字段
    BaseDir         string    // 基础目录（~/.ProjectName/env）
    RepoDir         string    // 仓库目录
    BinDir          string    // 二进制目录
    WorkerDir       string    // 工作目录
    MetadataDir     string    // 元数据目录
    FrontendDir     string    // 前端仓库目录
    FrontendDistDir string    // 前端 dist 目录（worker/dist）
}
\`\`\`

### 创建配置

\`\`\`go
cfg, err := deployer.NewConfig(deployer.ConfigParams{
    GithubRepo:     githubRepo,     // 必填：Git URL（如 git@github.com:org/repo.git）
    Env:            env,            // 必填：环境名称
    BuildEntry:     buildEntry,     // 必填：构建入口（如 ./cmd/app）
    AppArgs:        appArgs,        // 可选：应用参数（如 "--verbose"）
    Port:           port,           // 必填：服务器端口
    BasePath:       basePath,       // 必填：基础路径（如 "/deploy"）
    FrontendGitURL: frontendGitURL, // 可选：前端仓库 URL（空字符串 = 禁用）
})
\`\`\`

**重要**：\`basePath\` 是必填项，它定义了部署控制台的挂载路径。

## 🔌 API 文档

### 导航路由

\`\`\`
GET  /deploy/              # 导航首页
GET  /deploy/backend/      # 后端管理界面
GET  /deploy/frontend/     # 前端构建界面
\`\`\`

### 后端部署

\`\`\`
GET  /deploy/backend/status                 # 实例状态
GET  /deploy/backend/git/branches           # 列出分支
GET  /deploy/backend/git/commits            # 列出提交
POST /deploy/backend/deploy                 # 触发部署
GET  /deploy/backend/deploy/logs            # 部署日志（SSE）
POST /deploy/backend/kill?pid={pid}         # 停止实例
\`\`\`

### 前端构建

\`\`\`
GET  /deploy/frontend/status                # 构建状态
GET  /deploy/frontend/git/branches          # 列出分支
GET  /deploy/frontend/git/commits           # 列出提交
POST /deploy/frontend/build                 # 触发构建
GET  /deploy/frontend/build/logs            # 构建日志（SSE）
\`\`\`

### 响应格式

所有 JSON 响应遵循以下结构：

\`\`\`json
{
  "code": 0,
  "message": "success",
  "data": {...}
}
\`\`\`

- \`code\`: 0 = 成功，非零 = 错误
- \`message\`: 人类可读的消息
- \`data\`: 响应数据

## ⚡ 零停机机制

### 问题所在

传统的 \`rm && cp\` 方式存在致命缺陷：

\`\`\`bash
rm -rf dist/* && cp -r build/* dist/
         ↑
    目录为空
    → 用户访问得到 404 错误
\`\`\`

### 我们的解决方案：原子交换

我们使用 \`mv\` 命令实现原子级目录交换：

\`\`\`bash
# 1. 复制到临时目录
cp -r frontend-repo/dist worker/dist-new

# 2. 原子交换（~1μs，不可分割的操作）
mv worker/dist worker/dist-old
mv worker/dist-new worker/dist

# 3. 清理旧版本（异步，非阻塞）
rm -rf worker/dist-old
\`\`\`

### 性能对比

| 指标 | 传统方式 | 原子交换 |
|------|---------|---------|
| 停机时间 | ~100ms | ~1μs |
| 404 风险 | ⚠️ 高 | ✅ 零 |
| 用户影响 | 明显 | 无感知 |
| 回滚 | ❌ 手动 | ✅ 自动 |

### 技术细节

- \`mv\` 在同一文件系统上是原子操作（POSIX 保证）
- 交换在单个系统调用中完成（约 1 微秒）
- 不存在目录缺失或为空的时间窗口
- 如果新版本启动失败，可自动回滚

## 📚 使用示例

### 示例 1：最小化配置

\`\`\`go
package main

import (
    "net/http"
    deployer "github.com/vito-go/go-deployer"
)

func main() {
    cfg, _ := deployer.NewConfig(deployer.ConfigParams{
        GithubRepo: "git@github.com:yourorg/app.git",
        Env:        "production",
        BuildEntry: "./cmd/app",
        Port:       8080,
        BasePath:   "/deploy",
    })

    dep := deployer.NewDeployer(cfg)
    mux := http.NewServeMux()
    dep.Mount(mux)

    http.ListenAndServe(":8080", mux)
}
\`\`\`

### 示例 2：多个 PreHandler

\`\`\`go
// 链式多个认证/授权处理器
func loggingHandler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
    log.Printf("[%s] %s", r.Method, r.URL.Path)
    return r, true
}

func authHandler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
    if r.Header.Get("X-API-Key") != "secret" {
        http.Error(w, "Forbidden", http.StatusForbidden)
        return r, false
    }
    return r, true
}

dep := deployer.NewDeployer(cfg)
mux := http.NewServeMux()
dep.Mount(mux, loggingHandler, authHandler)
\`\`\`

### 示例 3：前后端同时部署

\`\`\`go
cfg, _ := deployer.NewConfig(deployer.ConfigParams{
    GithubRepo:     "git@github.com:yourorg/backend.git",
    Env:            "production",
    BuildEntry:     "./cmd/api",
    AppArgs:        "--port=8001",
    Port:           8001,
    BasePath:       "/admin/deploy",
    FrontendGitURL: "git@github.com:yourorg/frontend.git",
})

dep := deployer.NewDeployer(cfg)
defer dep.Cleanup() // 重要：关闭时清理资源

mux := http.NewServeMux()
dep.Mount(mux)

http.ListenAndServe(":8001", mux)
\`\`\`

## 🔒 PreHandler 中间件

\`PreHandler\` 类型允许你在请求处理前注入自定义逻辑：

\`\`\`go
type PreHandler func(w http.ResponseWriter, r *http.Request) (newR *http.Request, next bool)
\`\`\`

### 参数

- \`w\`: 响应写入器（用于发送错误）
- \`r\`: 原始请求

### 返回值

- \`newR\`: 修改后的请求（可以与 \`r\` 相同）
- \`next\`: \`true\` 表示继续处理，\`false\` 表示停止

### 使用场景

- **认证**：验证 JWT token、API 密钥
- **授权**：检查用户权限
- **日志记录**：记录访问日志
- **限流**：实现请求节流
- **请求修改**：添加请求头、上下文值

### 示例：JWT 认证

\`\`\`go
func jwtAuthHandler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
    cookie, err := r.Cookie("auth_token")
    if err != nil {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return r, false
    }

    // 验证 JWT token
    claims, err := validateJWT(cookie.Value)
    if err != nil {
        http.Error(w, "Invalid token", http.StatusUnauthorized)
        return r, false
    }

    // 将用户信息添加到上下文
    ctx := context.WithValue(r.Context(), "user_id", claims.UserID)
    newR := r.WithContext(ctx)

    return newR, true
}
\`\`\`

## 🎨 Web UI 功能

### 导航首页

- 清爽的卡片式界面
- 深色模式（localStorage 持久化）
- 响应式设计（移动端友好）
- 快速访问后端和前端管理

### 后端控制台

- 📋 实例列表（PID、版本、运行时长）
- 🎯 Git 分支和提交选择器
- 🚀 一键部署
- 📊 实时部署日志（SSE）
- 🛑 优雅关闭实例
- ⚠️ 安全检查（防止关闭最后一个实例）

### 前端控制台

- 🏗️ 当前构建信息
- 🎯 Git 分支和提交选择器
- 🔨 一键构建和部署
- 📊 实时构建日志
- 💡 构建流程说明

## 🛡️ 最佳实践

### 生产环境部署

1. **使用 HTTPS** - 保护认证 cookie
2. **强认证** - 实现健壮的 PreHandler
3. **防火墙保护** - 限制部署控制台访问
4. **定期备份** - 备份元数据目录
5. **监控** - 监控 \`bin/\` 和 \`dist/\` 的磁盘使用

### 安全考虑

- 始终在 PreHandler 中验证用户权限
- 使用安全的随机字符串作为 JWT 密钥
- 生产环境启用 HTTPS（secure cookie 必需）
- 定期轮换认证凭据
- 为部署端点实现限流

### 性能建议

- 定期清理旧二进制文件（保留最近 3-5 个版本）
- 为前端构建使用 \`.dockerignore\` 或 \`.gitignore\`
- 为大型仓库配置 Git 浅克隆
- 监控基础目录的磁盘使用

## 🐛 故障排查

### 构建失败

**检查 Node.js 版本**：
\`\`\`bash
node -v  # 应该 >= v16
npm -v   # 应该 >= v8
\`\`\`

### 权限拒绝

**修复目录权限**：
\`\`\`bash
chmod -R 755 ~/.{项目名}/
\`\`\`

### 实例未显示

**检查进程是否运行**：
\`\`\`bash
lsof -i :{端口}
\`\`\`

**检查元数据是否存在**：
\`\`\`bash
ls -la ~/.{项目名}/{环境名}/metadata/
\`\`\`

## 🤝 贡献指南

欢迎贡献！请随时提交 Pull Request。

1. Fork 本仓库
2. 创建你的特性分支（\`git checkout -b feature/amazing-feature\`）
3. 提交你的更改（\`git commit -m 'Add amazing feature'\`）
4. 推送到分支（\`git push origin feature/amazing-feature\`）
5. 开启一个 Pull Request

## 📄 开源协议

本项目采用 MIT 协议 - 详见 [LICENSE](LICENSE) 文件。

## 🙏 致谢

- 基于 Go 强大的标准库构建
- 灵感来自现代 DevOps 实践
- UI 由 React 和 Tailwind CSS 驱动

---

<div align="center">

**用 ❤️ 打造，来自 go-deployer 社区**

[⭐ 在 GitHub 上给我们点赞](https://github.com/vito-go/go-deployer)

</div>
