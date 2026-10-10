// Command plugin is a Silo watch_sync_provider.v1 plugin that syncs a
// Letterboxd watchlist with the Silo watchlist.
package main

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"time"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/hashicorp/go-hclog"

	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
	"github.com/bulderi/silo-plugin-letterboxd/provider"
)

//go:embed manifest.json
var manifestJSON []byte

// version is set at build time via -ldflags "-X main.version=...".
var version string

// requestInterval spaces out every request to Letterboxd, across all
// connected accounts.
const requestInterval = 1500 * time.Millisecond

func main() {
	logger := hclog.New(&hclog.LoggerOptions{
		Name:       "letterboxd",
		Level:      hclog.Info,
		Output:     os.Stderr,
		JSONFormat: true,
	})
	sdkruntime.ServeManifest(manifestJSON, version, capabilityServers(provider.Options{
		Throttle: letterboxd.NewThrottle(requestInterval),
		Store:    hostStore{},
		Logger:   logger,
	}))
}

// capabilityServers is what main serves; tests build the same set against a
// fake Letterboxd.
func capabilityServers(opts provider.Options) sdkruntime.CapabilityServers {
	return sdkruntime.CapabilityServers{WatchSyncProvider: provider.New(opts)}
}

var errHostUnavailable = errors.New("silo runtime host is not bound yet")

// hostStore is the host's per-installation instance state. The host binds its
// broker after the plugin starts, so the client is looked up on every call.
type hostStore struct{}

func (hostStore) Read(ctx context.Context, key string) ([]byte, bool, error) {
	host := sdkruntime.Host()
	if host == nil {
		return nil, false, errHostUnavailable
	}
	return host.ReadInstanceState(ctx, key)
}

func (hostStore) Write(ctx context.Context, key string, value []byte) error {
	host := sdkruntime.Host()
	if host == nil {
		return errHostUnavailable
	}
	return host.WriteInstanceState(ctx, key, value)
}
