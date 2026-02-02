# AGENTS.md - Project Guidelines

## Project Overview

This is a **microVM sandbox management** project.
- **Language**: Go 1.21+
- **Type**: CLI tool and library
- **Package Manager**: Go modules
- **Purpose**: Manage microVM sandboxes supporting multiple VMM backends (Cloud-Hypervisor, Firecracker, QEMU)

## Build/Lint/Test Commands

### Build Commands
```bash
# Build all packages
go build ./...

# Build CLI binary
go build -o bin/sandboxd ./cmd/sandboxd

# Build for production (stripped)
go build -ldflags="-s -w" -o bin/sandboxd ./cmd/sandboxd
```

### Test Commands
```bash
# Run all tests
go test ./...

# Run a single test file
go test ./pkg/sandbox -run TestCreate

# Run tests matching a pattern
go test ./... -run "TestVMM"

# Run tests in watch mode (requires entr or similar)
find . -name "*.go" | entr -c go test ./...

# Run tests with race detector
go test -race ./...

# Run tests with coverage
go test -cover ./...
go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out
```

### Lint/Format Commands
```bash
# Run go fmt
go fmt ./...

# Run go vet
go vet ./...

# Run linter (requires golangci-lint)
golangci-lint run

# Fix auto-fixable issues
golangci-lint run --fix

# Check imports (requires goimports)
goimports -w .
```

### Type Checking
```bash
# Go is statically typed, no separate type checker needed
# Use go build or go vet for type checking
```

## Code Style Guidelines

### Imports
- Use **absolute** imports for project files (e.g., `github.com/microvm/sandbox/pkg/vmm`)
- Group imports in 3 sections: 1) stdlib, 2) external, 3) internal
- Sort imports alphabetically within groups
- Example:
```go
import (
	"context"
	"fmt"
	"os"

	"github.com/sirupsen/logrus"

	"github.com/microvm/sandbox/pkg/vmm"
	"github.com/microvm/sandbox/pkg/sandbox"
)
```

### Formatting
- Indent: **tabs** (gofmt standard)
- Max line length: **100** characters (soft limit)
- Trailing commas: **always** in multi-line structs/slices
- Semicolons: **not used** (Go inserts them)

### Naming Conventions
- **Files**: `snake_case.go` or `lowercase.go`
- **Variables**: `camelCase`
- **Constants**: `PascalCase` or `camelCase` for unexported
- **Functions**: `PascalCase` (exported) or `camelCase` (unexported)
- **Interfaces**: `PascalCase` with `-er` suffix (e.g., `Reader`, `Driver`)
- **Structs**: `PascalCase`
- **Package names**: lowercase, single word (e.g., `vmm`, `sandbox`)
- **Acronyms**: keep uppercase (e.g., `VMM`, `API`, `HTTP`, `ID`)

### Types
- Prefer **explicit** interfaces for public APIs
- Use **struct embedding** for composition
- Define error types for specific error handling
- Use `context.Context` as first parameter in functions that accept it

### Error Handling
- Use **explicit error returns**, not exceptions
- Wrap errors with context using `fmt.Errorf("...: %w", err)`
- Check errors immediately after function calls
- Never swallow errors silently
- Example:
```go
if err := doSomething(); err != nil {
	return fmt.Errorf("failed to do something: %w", err)
}
```

### Comments
- Use **complete sentences** ending with a period
- Begin exported declarations with the name being declared
- Example: `// VM represents a virtual machine.`
- Use `// TODO:` and `// FIXME:` markers for pending work

## Project Structure

```
.
├── cmd/
│   └── sandboxd/        # Main CLI application
│       └── main.go
├── pkg/
│   ├── vmm/            # VMM abstraction layer
│   │   ├── vmm.go      # Core interfaces
│   │   ├── cloudhypervisor/  # Cloud-Hypervisor driver
│   │   │   └── cloudhypervisor.go
│   │   ├── firecracker/      # Firecracker driver (future)
│   │   └── qemu/             # QEMU driver (future)
│   └── sandbox/        # High-level sandbox management
│       └── manager.go
├── internal/
│   └── config/         # Internal configuration
├── bin/                # Compiled binaries (gitignored)
├── go.mod              # Go module definition
└── AGENTS.md           # This file
```

## Testing Guidelines

- Write tests for all exported functions
- Use descriptive test names: `TestCreate_Success`, `TestCreate_InvalidConfig`
- Mock external dependencies (VMM binaries, network)
- One assertion per test (where practical)
- Run tests before committing: `go test ./...`
- Table-driven tests for multiple test cases

## Git Workflow

- Branch naming: `feature/description`, `fix/description`, `docs/description`
- Commit format: `type(scope): description` (conventional commits)
- Types: feat, fix, docs, style, refactor, test, chore
- Example: `feat(vmm): add Cloud-Hypervisor snapshot support`

## VMM Driver Development

When implementing a new VMM driver:
1. Create package under `pkg/vmm/<driver>/`
2. Implement the `vmm.Driver` interface
3. Implement the `vmm.VM` interface for VM instances
4. Register driver in `cmd/sandboxd/main.go`
5. Add tests for all operations

## Security Notes

- Never log sensitive data (VM credentials, network configs)
- Validate all file paths to prevent path traversal
- Use proper permissions (0755/0644) for created directories/files
- Handle signals gracefully for clean shutdown

## Important Notes

- Always run `go fmt` and `go vet` before committing
- Update this file when conventions change
- Ask before adding new dependencies (check `go.mod`)
- VMM binaries (cloud-hypervisor, firecracker) must be installed separately
