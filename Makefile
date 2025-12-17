.DEFAULT_GOAL := amd64

# Version information
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS := -X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)

# Build targets
amd64: GOARCH=amd64
amd64: GOOS=linux
amd64: binary

arm64: GOARCH=arm64
arm64: GOOS=linux
arm64: binary

mactel: GOARCH=amd64
mactel: GOOS=darwin
mactel: binary

macarm: GOARCH=arm64
macarm: GOOS=darwin
macarm: binary

wintel: GOARCH=amd64
wintel: GOOS=windows
wintel: binary

winarm: GOARCH=arm64
winarm: GOOS=windows
winarm: binary

# Universal binary build
binary:
	@rm -rf bin/$(GOOS)/$(GOARCH)
	@mkdir -p bin/$(GOOS)/$(GOARCH)
	@echo -n "Building 'maria_repl_check' for $(GOOS)/$(GOARCH) in './bin/$(GOOS)/$(GOARCH)'..."
	@GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(GOOS)/$(GOARCH)/maria_repl_check$(if $(findstring windows,$(GOOS)),.exe,) .
	@echo " done"
	@ls -lh bin/$(GOOS)/$(GOARCH)/maria_repl_check$(if $(findstring windows,$(GOOS)),.exe,)

# Build all platforms
all: amd64 arm64 mactel macarm wintel winarm
	@echo "All builds complete"

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf bin/
	@echo "Clean complete"

# Run tests
test:
	@echo "Running tests..."
	@go test -v ./...

# Install dependencies
deps:
	@echo "Installing dependencies..."
	@go mod download
	@go mod tidy

# Local development build (current platform)
dev:
	@echo "Building for local development..."
	@go build -o rssimport .
	@echo "Build complete: ./rssimport"

# Help target
help:
	@echo "Available targets:"
	@echo "  amd64    - Build for Linux AMD64 (default)"
	@echo "  arm64    - Build for Linux ARM64"
	@echo "  mactel   - Build for macOS Intel"
	@echo "  macarm   - Build for macOS ARM (M1/M2/M3)"
	@echo "  wintel   - Build for Windows AMD64"
	@echo "  winarm   - Build for Windows ARM64"
	@echo "  all      - Build for all platforms"
	@echo "  dev      - Build for current platform"
	@echo "  clean    - Remove build artifacts"
	@echo "  test     - Run tests"
	@echo "  deps     - Install/update dependencies"
	@echo "  help     - Show this help message"

.PHONY: amd64 arm64 mactel macarm wintel winarm binary all clean test deps dev help
