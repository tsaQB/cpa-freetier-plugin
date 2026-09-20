.PHONY: all build-amd64 build-arm64 clean

all: build-amd64 build-arm64

build-amd64:
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC=gcc go build -buildmode=c-shared -ldflags="-s -w" -o dist/cpa-opencode-plugin_linux_amd64.so main.go

build-arm64:
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc go build -buildmode=c-shared -ldflags="-s -w" -o dist/cpa-opencode-plugin_linux_arm64.so main.go

clean:
	rm -rf dist/
