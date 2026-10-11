// Package catalogindex builds the plugin repository index (index.json) that a
// Silo server reads when an administrator adds this repository under
// Plugins > Catalog. Silo then downloads the binary for its platform, checks
// its SHA-256, and offers new versions as updates.
package catalogindex

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

// The index mirrors Silo's RepositoryIndex, CatalogPackage, and PlatformBinary
// (internal/plugins/catalog_service.go). Silo decodes it with encoding/json
// straight into the protobuf manifest type, so the manifest is written the
// same way: snake_case field names and numeric enum values. A manifest copied
// from manifest.json, with enum names as strings, would not decode.

// Index is a plugin repository index.
type Index struct {
	Plugins []Package `json:"plugins"`
}

// Package is one plugin release in the index.
type Package struct {
	Manifest     *pluginv1.PluginManifest `json:"manifest"`
	RepoURL      string                   `json:"repo_url,omitempty"`
	ChecksumsURL string                   `json:"checksums_url,omitempty"`
	Binaries     map[string]Binary        `json:"binaries,omitempty"`
}

// Binary is one platform's release asset.
type Binary struct {
	URL      string `json:"url"`
	Checksum string `json:"checksum"`
}

var (
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Options describes one release.
type Options struct {
	// ManifestJSON is the repository's manifest.json.
	ManifestJSON []byte
	// Repository is the GitHub repository in owner/name form.
	Repository string
	// Tag is the release tag, which must be "v" plus the manifest version.
	Tag string
	// DistDir holds plugin-<os>-<arch> binaries and checksums.txt.
	DistDir string
}

// Build creates the index for one release. It refuses a release whose tag
// does not match the manifest, or whose binaries do not match checksums.txt,
// so an index can never point Silo at a binary it would reject.
func Build(opts Options) (*Index, error) {
	if !repoPattern.MatchString(opts.Repository) || strings.Contains(opts.Repository, "..") {
		return nil, fmt.Errorf("repository %q must be in owner/name form", opts.Repository)
	}
	manifest, err := publicmanifest.Load(opts.ManifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	if want := "v" + manifest.GetVersion(); opts.Tag != want {
		return nil, fmt.Errorf("tag %q does not match manifest version %q", opts.Tag, manifest.GetVersion())
	}
	checksumsFile, err := os.Open(filepath.Join(opts.DistDir, "checksums.txt"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = checksumsFile.Close() }()
	checksums, err := ParseChecksums(checksumsFile)
	if err != nil {
		return nil, err
	}

	base := "https://github.com/" + opts.Repository + "/releases/download/" + url.PathEscape(opts.Tag) + "/"
	binaries := map[string]Binary{}
	for _, platform := range manifest.GetSupportedPlatforms() {
		key := platform.GetOs() + "/" + platform.GetArch()
		if _, dup := binaries[key]; dup {
			return nil, fmt.Errorf("manifest declares platform %s twice", key)
		}
		name := "plugin-" + platform.GetOs() + "-" + platform.GetArch()
		want, ok := checksums[name]
		if !ok {
			return nil, fmt.Errorf("checksums.txt has no entry for %s", name)
		}
		got, err := fileSHA256(filepath.Join(opts.DistDir, name))
		if err != nil {
			return nil, err
		}
		if got != want {
			return nil, fmt.Errorf("%s does not match its checksum in checksums.txt", name)
		}
		binaries[key] = Binary{URL: base + name, Checksum: want}
	}
	if len(binaries) == 0 {
		return nil, fmt.Errorf("manifest declares no supported platforms")
	}

	// The checksum belongs to each platform binary; the manifest's own field
	// is a build-time placeholder.
	manifest.Checksum = ""
	return &Index{Plugins: []Package{{
		Manifest:     manifest,
		RepoURL:      "https://github.com/" + opts.Repository,
		ChecksumsURL: base + "checksums.txt",
		Binaries:     binaries,
	}}}, nil
}

// Marshal writes the index as indented JSON with a trailing newline.
func (i *Index) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ParseChecksums reads "<sha256>  <file>" lines, as sha256sum writes them.
func ParseChecksums(r io.Reader) (map[string]string, error) {
	checksums := map[string]string{}
	scanner := bufio.NewScanner(r)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 || !sha256Pattern.MatchString(strings.ToLower(fields[0])) {
			return nil, fmt.Errorf("checksums.txt line %d: want \"<sha256>  <file>\"", line)
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" || strings.ContainsAny(name, `/\`) {
			return nil, fmt.Errorf("checksums.txt line %d: invalid file name %q", line, name)
		}
		if _, dup := checksums[name]; dup {
			return nil, fmt.Errorf("checksums.txt line %d: %s listed twice", line, name)
		}
		checksums[name] = strings.ToLower(fields[0])
	}
	return checksums, scanner.Err()
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
