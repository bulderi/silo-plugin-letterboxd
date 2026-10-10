package main

import (
	"context"
	"net"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimedefault"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
	"github.com/bulderi/silo-plugin-letterboxd/provider"
)

// newGRPCClient serves what main serves through the SDK's plugin set, the way
// the host starts the plugin, and returns a client for it. Every call crosses
// protobuf serialization, as it does between Silo and the plugin.
func newGRPCClient(t *testing.T, site *fakesite.Site) pluginv1.WatchSyncProviderClient {
	t.Helper()
	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, version)
	if err != nil {
		t.Fatal(err)
	}
	servers := capabilityServers(provider.Options{BaseURL: site.URL})
	// ServeManifest adds a Runtime server answering GetManifest with the
	// loaded manifest; the test adds the same.
	servers.Runtime = &manifestRuntime{manifest: manifest}
	plugins := sdkruntime.DefaultPluginSet(servers)
	grpcPlugin, ok := plugins[sdkruntime.PluginSetName].(plugin.GRPCPlugin)
	if !ok {
		t.Fatalf("plugin set entry = %T, want plugin.GRPCPlugin", plugins[sdkruntime.PluginSetName])
	}
	server := grpc.NewServer()
	if err := grpcPlugin.GRPCServer(nil, server); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///letterboxd",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	runtimeClient := pluginv1.NewRuntimeClient(conn)
	got, err := runtimeClient.GetManifest(context.Background(), &pluginv1.GetManifestRequest{})
	if err != nil || got.GetManifest().GetPluginId() != "bulderi.letterboxd" {
		t.Fatalf("GetManifest: %v %v", err, got)
	}
	return pluginv1.NewWatchSyncProviderClient(conn)
}

type manifestRuntime struct {
	runtimedefault.Server
	manifest *pluginv1.PluginManifest
}

func (r *manifestRuntime) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: r.manifest}, nil
}

// TestWatchSyncOverGRPC connects, reads the watchlist, and writes to it
// through the gRPC services the host calls.
func TestWatchSyncOverGRPC(t *testing.T) {
	site := fakesite.New(t)
	site.AddFilms(
		fakesite.Film{LID: "aA1", Slug: "the-quiet-harbor", Title: "The Quiet Harbor", Year: 2019, TMDBID: "5550001", TMDBType: "movie", IMDbID: "tt5550001"},
		fakesite.Film{LID: "bB2", Slug: "northern-lights-2021", Title: "Northern Lights", Year: 2021, TMDBID: "5550002", TMDBType: "movie"},
		fakesite.Film{LID: "eE5", Slug: "the-mini-series", Title: "The Mini Series", Year: 2016, TMDBID: "5550005", TMDBType: "tv"},
	)
	site.SetWatchlist("eE5", "aA1")
	client := newGRPCClient(t, site)
	ctx := context.Background()

	connected, err := client.ExchangeAPIKey(ctx, &pluginv1.WatchSyncExchangeAPIKeyRequest{
		CapabilityId:   "letterboxd",
		ProviderConfig: &pluginv1.WatchSyncProviderConfig{Values: map[string]string{provider.ConnectionUsernameKey: site.Email()}},
		ApiKey:         site.Password(),
	})
	if err != nil || connected.GetFault() != nil {
		t.Fatalf("ExchangeAPIKey: %v %v", err, connected.GetFault())
	}
	if connected.GetAccount().GetUsername() != site.Username() {
		t.Fatalf("account = %v", connected.GetAccount())
	}
	auth := &pluginv1.WatchSyncAuthenticatedContext{CapabilityId: "letterboxd", Credentials: connected.GetCredentials()}

	listed, err := client.ListRemoteState(ctx, &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    auth,
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST},
	})
	if err != nil || listed.GetFault() != nil || listed.GetNextPageToken() != "" {
		t.Fatalf("ListRemoteState: %v %v", err, listed)
	}
	if !listed.GetCompleteSnapshot() || len(listed.GetItems()) != 1 || listed.GetItems()[0].GetProviderItemKey() != "letterboxd:aA1" {
		t.Fatalf("items = %v", listed.GetItems())
	}
	if media := listed.GetItems()[0].GetMedia(); media.GetExternalIds()["tmdb"] != "5550001" || media.GetTitle() != "The Quiet Harbor" {
		t.Fatalf("media = %v", media)
	}
	if got := strings.Join(listed.GetWarnings(), "\n"); !strings.Contains(got, "The Mini Series (2016)") {
		t.Fatalf("warnings = %q, want the skipped TV entry named", got)
	}

	applied, err := client.ApplyEvents(ctx, &pluginv1.WatchSyncApplyEventsRequest{
		Context: auth,
		Events: []*pluginv1.WatchSyncEvent{{
			EventId:   "add-northern-lights",
			Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_WATCHLIST,
			Media: &pluginv1.WatchSyncMedia{
				MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
				ExternalIds: map[string]string{"tmdb": "5550002"},
			},
		}},
	})
	if err != nil || applied.GetFault() != nil || len(applied.GetResults()) != 1 ||
		applied.GetResults()[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("ApplyEvents: %v %v", err, applied)
	}
	if got := strings.Join(site.Watchlist(), ","); got != "bB2,eE5,aA1" {
		t.Fatalf("watchlist = %s", got)
	}

	account, err := client.GetAccount(ctx, &pluginv1.WatchSyncGetAccountRequest{Context: auth})
	if err != nil || account.GetAccount().GetExternalSubject() != site.Username() {
		t.Fatalf("GetAccount: %v %v", err, account)
	}
}
