# Contributing to go-deployer

First off, thank you for considering contributing to go-deployer! 🎉

## 📋 Table of Contents

- [Code of Conduct](#code-of-conduct)
- [How Can I Contribute?](#how-can-i-contribute)
- [Development Setup](#development-setup)
- [Pull Request Process](#pull-request-process)
- [Coding Guidelines](#coding-guidelines)
- [Testing Guidelines](#testing-guidelines)

## Code of Conduct

This project adheres to a code of conduct. By participating, you are expected to uphold this code. Please report unacceptable behavior to the project maintainers.

## How Can I Contribute?

### Reporting Bugs

Before creating bug reports, please check existing issues to avoid duplicates. When creating a bug report, include:

- **Clear title and description**
- **Steps to reproduce** the behavior
- **Expected behavior** vs **actual behavior**
- **Environment details** (Go version, OS, etc.)
- **Error messages** and logs

### Suggesting Enhancements

Enhancement suggestions are tracked as GitHub issues. When creating an enhancement suggestion:

- **Use a clear and descriptive title**
- **Provide detailed description** of the proposed functionality
- **Explain why this enhancement would be useful**
- **List any alternative solutions** you've considered

### Pull Requests

- Fill in the required template
- Follow the coding guidelines
- Include appropriate test coverage
- Update documentation as needed
- Ensure all tests pass

## Development Setup

### Prerequisites

- Go 1.21 or higher
- Git
- Node.js 16+ (for frontend features)
- Make (optional, for convenience commands)

### Clone and Setup

```bash
# Clone your fork
git clone https://github.com/yourusername/go-deployer.git
cd go-deployer

# Install dependencies
go mod tidy

# Run tests
go test ./...
```

### Project Structure

```
go-deployer/
├── starter.go           # Core deployment logic
├── frontend.go          # Frontend build logic
├── index.html           # Navigation homepage
├── backend.html         # Backend management UI
├── frontend.html        # Frontend build UI
├── examples/            # Usage examples
│   ├── basic/
│   ├── with-auth/
│   └── fullstack/
├── README.md            # English documentation
├── README_ZH.md         # Chinese documentation
├── LICENSE              # MIT License
└── go.mod               # Go module definition
```

## Pull Request Process

1. **Fork** the repository and create your branch from `main`
2. **Make your changes** following the coding guidelines
3. **Add tests** for any new functionality
4. **Update documentation** (README, code comments, etc.)
5. **Ensure tests pass**: `go test ./...`
6. **Run formatting**: `go fmt ./...`
7. **Run linting** (if available): `golangci-lint run`
8. **Commit with clear messages** following conventional commits
9. **Push to your fork** and submit a pull request

### Commit Message Format

Follow the [Conventional Commits](https://www.conventionalcommits.org/) specification:

```
<type>(<scope>): <subject>

<body>

<footer>
```

**Types:**
- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation only changes
- `style`: Code style changes (formatting, etc.)
- `refactor`: Code refactoring
- `test`: Adding or updating tests
- `chore`: Maintenance tasks

**Examples:**
```
feat(frontend): add build retry mechanism
fix(backend): resolve race condition in heartbeat loop
docs(readme): add installation instructions for Windows
```

## Coding Guidelines

### Go Code Style

- Follow [Effective Go](https://golang.org/doc/effective_go.html)
- Use `gofmt` for formatting
- Use meaningful variable and function names
- Keep functions small and focused
- Add comments for exported functions and types
- Use `golangci-lint` for static analysis

### Code Organization

- **Exported types/functions**: Start with capital letter, include doc comment
- **Unexported types/functions**: Start with lowercase letter
- **Error handling**: Always handle errors explicitly, never ignore
- **Logging**: Use `mylog` package for consistent logging

### Documentation

- **Public APIs**: Must have doc comments
- **Examples**: Include usage examples for complex features
- **README**: Keep updated with new features
- **Code comments**: Explain "why", not "what"

## Testing Guidelines

### Unit Tests

- Test file naming: `*_test.go`
- Test function naming: `TestFunctionName`
- Use table-driven tests when appropriate
- Mock external dependencies
- Aim for high test coverage (>70%)

### Integration Tests

- Tag with `// +build integration`
- Test real Git operations
- Test real file system operations
- Clean up test artifacts

### Example Test

```go
func TestNewConfig(t *testing.T) {
    tests := []struct {
        name        string
        repo        string
        env         string
        basePath    string
        wantErr     bool
    }{
        {
            name:     "valid config",
            repo:     "git@github.com:org/repo.git",
            env:      "prod",
            basePath: "/deploy",
            wantErr:  false,
        },
        {
            name:     "empty basePath",
            repo:     "git@github.com:org/repo.git",
            env:      "prod",
            basePath: "",
            wantErr:  true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            cfg, err := NewConfig(tt.repo, tt.env, "./cmd", "", 8080, tt.basePath, "")
            if (err != nil) != tt.wantErr {
                t.Errorf("NewConfig() error = %v, wantErr %v", err, tt.wantErr)
            }
            if !tt.wantErr && cfg == nil {
                t.Error("NewConfig() returned nil config")
            }
        })
    }
}
```

## Release Process

1. Update version in documentation
2. Update CHANGELOG.md
3. Create git tag: `git tag -a v1.0.0 -m "Release v1.0.0"`
4. Push tag: `git push origin v1.0.0`
5. GitHub Actions will create the release automatically

## Questions?

Feel free to open an issue for discussion or reach out to the maintainers.

Thank you for contributing! 🚀
