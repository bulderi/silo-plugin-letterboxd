.PHONY: build build-all clean lint test

BINARY=plugin
PLATFORMS=linux/amd64 linux/arm64 darwin/arm64
# The version comes from manifest.json, so a local build reports the same
# version a release of this commit would.
VERSION ?= $(shell sed -n 's/^  "version": "\(.*\)",$$/\1/p' manifest.json)
LDFLAGS=-s -w -X main.version=$(VERSION)

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test -race ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BINARY) dist

build-all:
	@mkdir -p dist
	@set -e; for platform in $(PLATFORMS); do \
		GOOS=$${platform%%/*} GOARCH=$${platform##*/} CGO_ENABLED=0 \
		go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$(BINARY)-$${platform%%/*}-$${platform##*/} .; \
	done
