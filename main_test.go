package main

import (
	"encoding/json"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"

	"github.com/bulderi/silo-plugin-letterboxd/provider"
)

func TestManifest(t *testing.T) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("manifest does not pass SDK validation: %v", err)
	}
	if len(manifest.GetCapabilities()) != 1 {
		t.Fatalf("capabilities = %d, want 1", len(manifest.GetCapabilities()))
	}
	capability := manifest.GetCapabilities()[0]
	if capability.GetType() != "watch_sync_provider.v1" || capability.GetId() != "letterboxd" {
		t.Fatalf("capability = %s/%s", capability.GetType(), capability.GetId())
	}
	ws := capability.GetWatchSyncProvider()
	if !ws.GetImportWatchlist() || !ws.GetExportWatchlist() || !ws.GetRemoveWatchlist() || !ws.GetProvidesWatchlistOrder() {
		t.Error("watchlist import, export, removal and order must all be advertised")
	}
	if ws.GetImportWatched() || ws.GetExportWatched() || ws.GetImportFavorites() || ws.GetImportRatings() ||
		ws.GetExportRatings() || ws.GetScrobblePlayback() || ws.GetImportProgress() {
		t.Error("only the watchlist is synced")
	}
	media := ws.GetSupportedMediaTypes()
	if len(media) != 1 || media[0] != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
		t.Errorf("media types = %v, want movies only so Silo keeps series out of the sync", media)
	}
	auth := ws.GetAuthMethods()
	if len(auth) != 1 || auth[0] != pluginv1.WatchSyncAuthMethod_WATCH_SYNC_AUTH_METHOD_API_KEY {
		t.Errorf("auth methods = %v; the host supports exactly one of API key or device code", auth)
	}
	// Each ApplyEvents batch may need a lookup and a write per event, within
	// the host's two-minute RPC deadline.
	if ws.GetMaxBatchSize() < 1 || ws.GetMaxBatchSize() > 20 {
		t.Errorf("max batch size = %d", ws.GetMaxBatchSize())
	}
}

// The host flattens connection config to "<key>.<field>"; the provider reads
// the username from exactly that name.
func TestManifestConnectionConfigMatchesProvider(t *testing.T) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	schemas := manifest.GetCapabilities()[0].GetConfigSchema()
	if len(schemas) != 1 {
		t.Fatalf("config schemas = %d, want 1", len(schemas))
	}
	schema := schemas[0]
	fields := schema.GetAdminForm().GetFields()
	if len(fields) != 1 {
		t.Fatalf("fields = %d, want 1", len(fields))
	}
	if got := schema.GetKey() + "." + fields[0].GetKey(); got != provider.ConnectionUsernameKey {
		t.Fatalf("connection field = %q, provider reads %q", got, provider.ConnectionUsernameKey)
	}
	if fields[0].GetControl() == pluginv1.AdminFormControl_ADMIN_FORM_CONTROL_PASSWORD {
		t.Error("the username must be a public field; the password goes in the API-key field")
	}
	var document struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema.GetJsonSchema()), &document); err != nil {
		t.Fatalf("json_schema: %v", err)
	}
	if document.Type != "object" || len(document.Required) != 1 || document.Required[0] != fields[0].GetKey() {
		t.Errorf("json_schema = %+v", document)
	}
	if _, ok := document.Properties[fields[0].GetKey()]; !ok {
		t.Error("json_schema does not declare the form field")
	}
}

// The manifest carries the complete presentation block catalogs require, and
// its source link points at this repository.
func TestManifestPresentation(t *testing.T) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := publicmanifest.ValidateCatalogPresentation(manifest, "https://github.com/bulderi/silo-plugin-letterboxd"); err != nil {
		t.Fatalf("presentation: %v", err)
	}
	if manifest.GetCategory() != "Watch Providers" {
		t.Errorf("category = %q, want the category first-party watch providers use", manifest.GetCategory())
	}
	if got := manifest.GetPresentation().GetLicenseSpdx(); got != "AGPL-3.0-only" {
		t.Errorf("license_spdx = %q, want the license in LICENSE", got)
	}
}

// TestManifestContract runs the bootstrap ServeManifest runs: it validates the
// manifest and stamps the version and the running binary's checksum.
func TestManifestContract(t *testing.T) {
	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, version)
	if err != nil {
		t.Fatalf("LoadWithChecksum() returned error: %v", err)
	}
	if got, want := manifest.GetPluginId(), "bulderi.letterboxd"; got != want {
		t.Fatalf("plugin_id = %q, want %q (changing it would orphan existing connections)", got, want)
	}
	if got := manifest.GetChecksum(); got == "" || got == "__CHECKSUM__" {
		t.Fatalf("checksum = %q, want the running binary's sha256", got)
	}
	if version == "" && manifest.GetVersion() == "" {
		t.Fatal("manifest has no version")
	}
}
