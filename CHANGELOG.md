# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - 2026-02-12

### 🎉 Initial Release

First public release of go-deployer!

### ✨ Features

- **Zero-downtime deployment** for Go backend services
- **Frontend build and deploy** with npm integration
- **Unified web console** with navigation homepage
- **Multi-instance management** with PID tracking
- **Git integration** for branch and commit selection
- **Real-time logging** via Server-Sent Events (SSE)
- **PreHandler middleware** system for authentication and request preprocessing
- **Atomic directory swapping** for frontend deployments (~1μs downtime)
- **Dark mode support** with localStorage persistence
- **Graceful shutdown** and cleanup mechanisms
- **Security features**: Last instance protection, PID validation, permission checks

### 🏗️ Architecture

- **Backend deployment**: Git fetch → Go build → nohup start → metadata tracking
- **Frontend deployment**: Git fetch → npm install → npm build → atomic swap
- **Metadata system**: JSON files for instance tracking
- **Embedded UI**: Single binary with embedded HTML/CSS/JS

### 📦 Components

- `starter.go` - Core backend deployment logic
- `frontend.go` - Frontend build and deploy pipeline
- `index.html` - Navigation homepage
- `backend.html` - Backend management dashboard
- `frontend.html` - Frontend build console

### 📚 Documentation

- Comprehensive README (English and Chinese)
- Architecture documentation
- Contributing guidelines
- Multiple usage examples (basic, auth, fullstack)
- API reference
- Troubleshooting guide

### 🔒 Security

- Flexible PreHandler system for custom authentication
- Process isolation with `Setsid`
- PID-based permission checks
- Audit logging for sensitive operations
- Safe directory permissions (0755)

### 🚀 Performance

- Optimized Go build flags (`-s -w -trimpath`)
- Buffered channels for log streaming
- Minimal in-memory state (file-based persistence)
- Efficient `lsof` for process detection

### 🎨 UI/UX

- Modern React-based interface
- Tailwind CSS styling
- Responsive design (mobile-friendly)
- Real-time build/deploy logs
- Professional dark mode
- Intuitive navigation

### 📄 License

- MIT License

---

## [Unreleased]

### Planned Features

- [ ] Docker container deployment support
- [ ] Webhook triggers for auto-deployment
- [ ] Metrics and monitoring dashboard
- [ ] Multi-server cluster support
- [ ] Build artifact caching
- [ ] Deployment rollback UI
- [ ] Email notifications
- [ ] Slack integration

### Under Consideration

- [ ] Kubernetes integration
- [ ] Blue-green deployment strategy
- [ ] A/B testing support
- [ ] Deployment approval workflow
- [ ] Health check automation

---

## Release Notes Format

Each release follows this structure:

### Added
- New features and capabilities

### Changed
- Changes in existing functionality

### Deprecated
- Features that will be removed in future releases

### Removed
- Features removed in this release

### Fixed
- Bug fixes

### Security
- Security improvements and vulnerability patches

---

**Note**: Dates are in YYYY-MM-DD format. All versions follow [Semantic Versioning](https://semver.org/).

[1.0.0]: https://github.com/yourusername/go-deployer/releases/tag/v1.0.0
[Unreleased]: https://github.com/yourusername/go-deployer/compare/v1.0.0...HEAD
