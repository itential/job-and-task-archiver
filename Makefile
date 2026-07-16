BINARY        := itential-job-archiver
ORPHAN_BINARY := itential-orphan-archiver
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS     := -s -w -X main.version=$(VERSION)
OUTDIR      := dist
.PHONY: all mac linux windows clean test coverage hooks
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
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(ORPHAN_BINARY)-darwin-amd64 ./orphan-archiver
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(ORPHAN_BINARY)-darwin-arm64 ./orphan-archiver

## linux — amd64 and arm64 (RHEL/Rocky 8/9 compatible)
linux: | $(OUTDIR)
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(BINARY)-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(BINARY)-linux-arm64 .
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(ORPHAN_BINARY)-linux-amd64 ./orphan-archiver
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(ORPHAN_BINARY)-linux-arm64 ./orphan-archiver

## windows — amd64 only
windows: | $(OUTDIR)
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(BINARY)-windows-amd64.exe .
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUTDIR)/$(ORPHAN_BINARY)-windows-amd64.exe ./orphan-archiver

$(OUTDIR):
	mkdir -p $(OUTDIR)


## hooks — install git hooks from githooks/ into .git/hooks
hooks:
	@for hook in githooks/*; do \
		name=$$(basename $$hook); \
		cp $$hook .git/hooks/$$name; \
		chmod +x .git/hooks/$$name; \
		echo "Installed .git/hooks/$$name"; \
	done

clean:
	rm -rf $(OUTDIR)
