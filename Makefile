# tether build tasks. Go 1.27.1 is selected via go.mod (GOTOOLCHAIN=auto).
export GOTOOLCHAIN ?= go1.27.1

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all web build test check fmt dev-server dev-web clean install-service

all: build

web:
	cd web && npm ci --no-audit --no-fund && npm run build

build: web
	go build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/tether .

test:
	go vet ./...
	go test -race ./...
	cd web && npm run typecheck && npm test

# CI（.github/workflows/ci.yml）と同じ確認をまとめて行う
check:
	@test -z "$$(gofmt -l $$(git ls-files '*.go'))" || { gofmt -l $$(git ls-files '*.go'); echo "gofmt needed"; exit 1; }
	go vet ./...
	go test -race -count=1 ./...
	cd web && npm run format:check && npm run typecheck && npm test

fmt:
	gofmt -w $$(git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './web/node_modules/*')
	cd web && npm run format

dev-server:
	go run .

dev-web:
	cd web && npm run dev

clean:
	rm -rf bin web/dist/assets web/dist/*.html web/dist/*.png web/dist/*.svg web/dist/*.js web/dist/*.webmanifest

# Register tether as a systemd service (asks for sudo). See deploy/install-systemd.sh --help.
install-service: build
	deploy/install-systemd.sh install
