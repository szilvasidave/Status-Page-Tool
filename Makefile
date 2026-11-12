# Status Page - build helpers. Everything is plain "go build"; see README.md.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
ifeq ($(VERSION),)
VERSION := dev
endif

LDFLAGS := -s -w -X main.version=$(VERSION)
export CGO_ENABLED := 0

.PHONY: build build-windows test installer clean

## build: statuspage for this computer -> dist/statuspage
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/statuspage ./cmd/statuspage

## build-windows: dist/statuspage.exe (windows/amd64)
build-windows:
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/statuspage.exe ./cmd/statuspage

## test: go vet + go test (set STATUSPAGE_TEST_PG_DSN to include the PostgreSQL tests)
test:
	go vet ./...
	go test -p 1 ./...

## installer: dist/StatusPageSetup-<version>.exe with Inno Setup (Docker image amake/innosetup, or iscc on PATH)
installer: build-windows
	@if command -v docker >/dev/null 2>&1; then \
		docker run --rm -v "$(CURDIR):/work" amake/innosetup "/DAppVersion=$(VERSION)" installer/statuspage.iss; \
	elif command -v iscc >/dev/null 2>&1; then \
		iscc "/DAppVersion=$(VERSION)" installer/statuspage.iss; \
	else \
		echo "Building the installer needs Docker (image amake/innosetup) or Inno Setup 6.3+ (iscc)."; exit 1; \
	fi

clean:
	rm -rf dist
