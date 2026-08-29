package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"runtime/debug"
	"sync"
)

const resourceListRendererBuildDomain = "readout.resource-list.renderer-build\x00"

var cachedResourceListRendererFingerprint = sync.OnceValue(func() [sha256.Size]byte {
	info, _ := debug.ReadBuildInfo()
	var nonce [sha256.Size]byte
	if !cleanVCSRevision(info) {
		if _, err := rand.Read(nonce[:]); err != nil {
			panic("mint resource-list renderer nonce: " + err.Error())
		}
	}
	return resourceListRendererBuildFingerprint(info, nonce)
})

func resourceListRendererFingerprint() [sha256.Size]byte {
	return cachedResourceListRendererFingerprint()
}

// resourceListRendererBuildFingerprint uses the linker-recorded VCS revision
// for reproducible clean builds. Development and dirty builds intentionally get
// a process nonce: their source tree has no stable build identity, while ETags
// and Live revisions only need to agree inside this process.
func resourceListRendererBuildFingerprint(info *debug.BuildInfo, nonce [sha256.Size]byte) [sha256.Size]byte {
	digest := sha256.New()
	_, _ = digest.Write([]byte(resourceListRendererBuildDomain))
	if revision, clean := vcsRevision(info); clean {
		writeFingerprintPart(digest, []byte("revision"))
		writeFingerprintPart(digest, []byte(revision))
	} else {
		writeFingerprintPart(digest, []byte("process"))
		writeFingerprintPart(digest, nonce[:])
	}
	return [sha256.Size]byte(digest.Sum(nil))
}

func cleanVCSRevision(info *debug.BuildInfo) bool {
	_, clean := vcsRevision(info)
	return clean
}

func vcsRevision(info *debug.BuildInfo) (string, bool) {
	if info == nil {
		return "", false
	}
	var revision string
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, revision != "" && !modified
}

func writeFingerprintPart(digest hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(value)
}
