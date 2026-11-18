.PHONY: build build-all clean lint test

BINARY=plugin
PLATFORMS=linux/amd64 linux/arm64 darwin/arm64
# The version comes from manifest.json, so a local build reports the same
# version a release of this commit would.
VERSION ?= $(shell sed -n 's/^  "version": "\(.*\)",$$/\1/p' manifest.json)
# Development builds for a server Cloudflare keeps challenging can set
# FLARESOLVERR_URL, e.g. http://flaresolverr:8191; releases leave it empty.
FLARESOLVERR_URL ?=
LDFLAGS=-s -w -X main.version=$(VERSION) -X main.flareSolverrURL=$(FLARESOLVERR_URL)

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
