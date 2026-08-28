package web

import (
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"fmt"
	"io/fs"
	"sort"
	"sync"
)

// resourceListRendererSources deliberately embeds the templ sources, generated
// renderers, and Go helpers as validator metadata. The roughly 760 KiB payload is
// a bounded binary-size tradeoff: a template-only change must invalidate an ETag
// automatically, including in local "dev" builds that have no unique version.
//
//go:embed templates/*.templ templates/*.go
var resourceListRendererSources embed.FS

var cachedResourceListRendererFingerprint = sync.OnceValue(func() [sha256.Size]byte {
	digest, _, err := fingerprintResourceListRendererSources(resourceListRendererSources)
	if err != nil {
		panic("fingerprint embedded resource-list renderers: " + err.Error())
	}
	return digest
})

func resourceListRendererFingerprint() [sha256.Size]byte {
	return cachedResourceListRendererFingerprint()
}

// fingerprintResourceListRendererSources hashes the sorted path and bytes of
// every top-level templates/*.templ and templates/*.go source. Length-prefixing
// both parts keeps concatenation unambiguous. Returning the paths gives focused
// tests an exact file-set seam without exposing it in production behavior.
func fingerprintResourceListRendererSources(sourceFS fs.FS) ([sha256.Size]byte, []string, error) {
	var paths []string
	for _, pattern := range [...]string{"templates/*.templ", "templates/*.go"} {
		matches, err := fs.Glob(sourceFS, pattern)
		if err != nil {
			return [sha256.Size]byte{}, nil, fmt.Errorf("glob %s: %w", pattern, err)
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return [sha256.Size]byte{}, nil, fmt.Errorf("no resource-list renderer sources found")
	}

	hash := sha256.New()
	var size [8]byte
	for _, path := range paths {
		data, err := fs.ReadFile(sourceFS, path)
		if err != nil {
			return [sha256.Size]byte{}, nil, fmt.Errorf("read %s: %w", path, err)
		}
		binary.BigEndian.PutUint64(size[:], uint64(len(path)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(path))
		binary.BigEndian.PutUint64(size[:], uint64(len(data)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(data)
	}
	return [sha256.Size]byte(hash.Sum(nil)), paths, nil
}
