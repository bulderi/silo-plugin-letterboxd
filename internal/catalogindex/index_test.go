package catalogindex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/proto"
)

func readManifest(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeDist creates fake release binaries for every platform in the manifest
// and a matching checksums.txt.
func writeDist(t *testing.T, manifestJSON []byte) (dir string, tag string) {
	t.Helper()
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	var lines []string
	for _, p := range manifest.GetSupportedPlatforms() {
		name := "plugin-" + p.GetOs() + "-" + p.GetArch()
		body := []byte("binary for " + name)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+name)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, "v" + manifest.GetVersion()
}

// siloRepositoryIndex mirrors Silo's RepositoryIndex and CatalogPackage
// (internal/plugins/catalog_service.go), which decode the index with
// encoding/json.
type siloRepositoryIndex struct {
	Plugins []struct {
		Manifest     *pluginv1.PluginManifest `json:"manifest"`
		RepoURL      string                   `json:"repo_url,omitempty"`
		ArchiveURL   string                   `json:"archive_url,omitempty"`
		ChecksumsURL string                   `json:"checksums_url,omitempty"`
		Binaries     map[string]struct {
			URL      string `json:"url"`
			Checksum string `json:"checksum"`
		} `json:"binaries,omitempty"`
	} `json:"plugins"`
}

func TestIndexDecodesTheWaySiloReadsIt(t *testing.T) {
	manifestJSON := readManifest(t)
	dist, tag := writeDist(t, manifestJSON)
	index, err := Build(Options{ManifestJSON: manifestJSON, Repository: "bulderi/silo-plugin-letterboxd", Tag: tag, DistDir: dist})
	if err != nil {
		t.Fatal(err)
	}
	data, err := index.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	var decoded siloRepositoryIndex
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Silo could not decode the index: %v", err)
	}
	if len(decoded.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(decoded.Plugins))
	}
	entry := decoded.Plugins[0]
	want, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	want.Checksum = ""
	if !proto.Equal(entry.Manifest, want) {
		t.Fatalf("decoded manifest differs from manifest.json:\n got %v\nwant %v", entry.Manifest, want)
	}
	if err := publicmanifest.Validate(entry.Manifest); err != nil {
		t.Fatalf("decoded manifest does not validate: %v", err)
	}
	if entry.Manifest.GetChecksum() != "" {
		t.Error("the index must not advertise the manifest's checksum placeholder")
	}
	if entry.RepoURL != "https://github.com/bulderi/silo-plugin-letterboxd" {
		t.Errorf("repo_url = %q", entry.RepoURL)
	}
	base := "https://github.com/bulderi/silo-plugin-letterboxd/releases/download/" + tag + "/"
	if entry.ChecksumsURL != base+"checksums.txt" {
		t.Errorf("checksums_url = %q", entry.ChecksumsURL)
	}
	if len(entry.Binaries) != len(want.GetSupportedPlatforms()) {
		t.Fatalf("binaries = %v", entry.Binaries)
	}
	for _, p := range want.GetSupportedPlatforms() {
		name := "plugin-" + p.GetOs() + "-" + p.GetArch()
		bin, ok := entry.Binaries[p.GetOs()+"/"+p.GetArch()]
		body, _ := os.ReadFile(filepath.Join(dist, name))
		sum := sha256.Sum256(body)
		if !ok || bin.URL != base+name || bin.Checksum != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: %+v", name, bin)
		}
	}
}

// manifest.json names enum values as strings, which the SDK's loader accepts
// but encoding/json does not. This is why the index cannot be a copy of it.
func TestManifestFileItselfIsNotAValidIndexManifest(t *testing.T) {
	raw := `{"plugins":[{"manifest":` + string(readManifest(t)) + `}]}`
	var decoded siloRepositoryIndex
	if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
		t.Fatal("expected encoding/json to reject string enum values; if the SDK changed, revisit the index format")
	}
}

func TestBuildRefusesMismatches(t *testing.T) {
	manifestJSON := readManifest(t)
	cases := map[string]func(dist string, opts *Options){
		"tag does not match the manifest": func(_ string, o *Options) { o.Tag = "v9.9.9" },
		"binary does not match its checksum": func(dist string, _ *Options) {
			if err := os.WriteFile(filepath.Join(dist, "plugin-linux-amd64"), []byte("tampered"), 0o755); err != nil {
				panic(err)
			}
		},
		"binary missing": func(dist string, _ *Options) {
			if err := os.Remove(filepath.Join(dist, "plugin-linux-arm64")); err != nil {
				panic(err)
			}
		},
		"platform missing from checksums.txt": func(dist string, _ *Options) {
			data, _ := os.ReadFile(filepath.Join(dist, "checksums.txt"))
			var kept []string
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.HasSuffix(line, "plugin-darwin-arm64") {
					kept = append(kept, line)
				}
			}
			if err := os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(strings.Join(kept, "\n")), 0o644); err != nil {
				panic(err)
			}
		},
		"repository not owner/name": func(_ string, o *Options) { o.Repository = "bulderi/../other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dist, tag := writeDist(t, manifestJSON)
			opts := Options{ManifestJSON: manifestJSON, Repository: "bulderi/silo-plugin-letterboxd", Tag: tag, DistDir: dist}
			mutate(dist, &opts)
			if _, err := Build(opts); err == nil {
				t.Fatal("Build accepted a release it should refuse")
			}
		})
	}
}

func TestParseChecksums(t *testing.T) {
	sum := strings.Repeat("a", 64)
	got, err := ParseChecksums(strings.NewReader(fmt.Sprintf("%s  plugin-a\n\n%s *plugin-b\n", sum, strings.ToUpper(sum))))
	if err != nil || got["plugin-a"] != sum || got["plugin-b"] != sum {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{
		"nothex  plugin-a",
		sum + "  dir/plugin-a",
		sum + "  plugin-a\n" + sum + "  plugin-a",
		sum,
	} {
		if _, err := ParseChecksums(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// The index.json on main is what the install URL serves. It must decode the
// way Silo reads it and point at this repository's release for its version.
func TestCommittedIndex(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "index.json"))
	if os.IsNotExist(err) {
		t.Skip("no index.json committed yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	var decoded siloRepositoryIndex
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Silo could not decode index.json: %v", err)
	}
	if len(decoded.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(decoded.Plugins))
	}
	entry := decoded.Plugins[0]
	if err := publicmanifest.Validate(entry.Manifest); err != nil {
		t.Fatalf("index manifest does not validate: %v", err)
	}
	current, err := publicmanifest.Load(readManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Manifest.GetPluginId() != current.GetPluginId() {
		t.Fatalf("index plugin_id = %q, manifest.json has %q", entry.Manifest.GetPluginId(), current.GetPluginId())
	}
	base := "https://github.com/bulderi/silo-plugin-letterboxd/releases/download/v" + entry.Manifest.GetVersion() + "/"
	if entry.RepoURL != "https://github.com/bulderi/silo-plugin-letterboxd" || entry.ChecksumsURL != base+"checksums.txt" {
		t.Fatalf("repo_url %q, checksums_url %q", entry.RepoURL, entry.ChecksumsURL)
	}
	for _, p := range entry.Manifest.GetSupportedPlatforms() {
		bin, ok := entry.Binaries[p.GetOs()+"/"+p.GetArch()]
		if !ok || bin.URL != base+"plugin-"+p.GetOs()+"-"+p.GetArch() || !sha256Pattern.MatchString(bin.Checksum) {
			t.Errorf("%s/%s: %+v", p.GetOs(), p.GetArch(), bin)
		}
	}
}
