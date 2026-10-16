VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
BIN     := bin/subsvc

.PHONY: build test vet clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/subsvc

vet:
	go vet ./...

# Set XRAY_BIN=/usr/local/bin/xray to also run the tests that use a real Xray.
test:
	go test -race -count=1 ./...

clean:
	rm -rf bin
