# Contributing to upstash-redis-local

Thank you for your interest in contributing! This document provides guidelines and steps for contributing.

## 🚀 Quick Start

1. **Fork the repository** and clone it locally
2. **Install Go 1.22+** from [golang.org](https://golang.org/dl/)
3. **Install dependencies**: `go mod download`
4. **Build**: `make build`
5. **Run tests**: `go test ./...`

## 🐳 Running with Docker

```bash
# Start Redis and upstash-redis-local together
docker-compose up -d

# Test the connection
curl -H "Authorization: Bearer local-dev-token" http://localhost:8000/PING
```

## 📝 Making Changes

1. Create a new branch: `git checkout -b feature/your-feature`
2. Make your changes
3. Ensure code compiles: `go build`
4. Run tests: `go test ./...`
5. Commit with clear messages
6. Push and create a Pull Request

## 🔧 Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Add comments for exported functions
- Keep functions focused and small
- Handle errors explicitly

## 🐛 Reporting Issues

When reporting bugs, please include:
- Go version (`go version`)
- Operating system
- Steps to reproduce
- Expected vs actual behavior

## 📋 Pull Request Guidelines

- Keep PRs focused on a single change
- Update documentation if needed
- Add tests for new features
- Ensure CI passes

## 📄 License

By contributing, you agree that your contributions will be licensed under the MIT License.
