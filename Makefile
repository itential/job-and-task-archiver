BINARY      := job-and-task-archiver
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS     := -s -w -X main.version=$(VERSION)
OUTDIR      := dist
INSTALL_DIR ?= /usr/local/bin
GOOS        ?= $(shell go env GOOS)
GOARCH      ?= $(shell go env GOARCH)

.PHONY: all mac linux windows clean test coverage install

all: mac linux windows

## test — run unit tests
test:
	go test ./...

## coverage — run tests with coverage report
coverage:
	go test ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out

## mac — darwin/amd64 and darwin/arm64 (Apple Silicon)
mac: | $(OUTDIR)
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(BINARY)-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(BINARY)-darwin-arm64 .

## linux — amd64 and arm64 (RHEL/Rocky 8/9 compatible)
linux: | $(OUTDIR)
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(BINARY)-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(BINARY)-linux-arm64 .

## windows — amd64 only
windows: | $(OUTDIR)
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(BINARY)-windows-amd64.exe .

$(OUTDIR):
	mkdir -p $(OUTDIR)

## install — build for the current platform and install to INSTALL_DIR (default: /usr/local/bin)
install: | $(OUTDIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(BINARY) .
	install -m 0755 $(OUTDIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "Installed $(INSTALL_DIR)/$(BINARY)"

clean:
	rm -rf $(OUTDIR)
