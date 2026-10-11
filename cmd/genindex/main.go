// Command genindex writes index.json, the plugin repository index Silo reads
// from Plugins > Catalog > Add repository, for one release:
//
//	go run ./cmd/genindex -repo owner/name -tag v0.3.1 -dist dist -out dist/index.json
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bulderi/silo-plugin-letterboxd/internal/catalogindex"
)

func main() {
	repo := flag.String("repo", "", "GitHub repository in owner/name form")
	tag := flag.String("tag", "", "release tag, v plus the manifest version")
	dist := flag.String("dist", "dist", "directory with plugin-<os>-<arch> binaries and checksums.txt")
	manifestPath := flag.String("manifest", "manifest.json", "path to manifest.json")
	out := flag.String("out", "index.json", "where to write the index")
	flag.Parse()

	manifestJSON, err := os.ReadFile(*manifestPath)
	if err != nil {
		fail(err)
	}
	index, err := catalogindex.Build(catalogindex.Options{
		ManifestJSON: manifestJSON,
		Repository:   *repo,
		Tag:          *tag,
		DistDir:      *dist,
	})
	if err != nil {
		fail(err)
	}
	data, err := index.Marshal()
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "genindex:", err)
	os.Exit(1)
}
