package web

import (
	"bytes"
	"crypto/sha256"
	"runtime/debug"
	"testing"
)

func TestResourceListRendererFingerprintUsesCleanBuildRevision(t *testing.T) {
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.modified", Value: "false"},
	}}
	var firstNonce, secondNonce [sha256.Size]byte
	firstNonce[0] = 1
	secondNonce[0] = 2
	first := resourceListRendererBuildFingerprint(info, firstNonce)
	second := resourceListRendererBuildFingerprint(info, secondNonce)
	if first != second {
		t.Fatalf("clean revision depended on process nonce: %x != %x", first, second)
	}
	info.Settings[0].Value = "fedcba9876543210"
	if changed := resourceListRendererBuildFingerprint(info, firstNonce); changed == first {
		t.Fatalf("changed VCS revision kept renderer fingerprint %x", first)
	}
}

func TestResourceListRendererFingerprintUsesNonceForUnstableBuilds(t *testing.T) {
	var firstNonce, secondNonce [sha256.Size]byte
	firstNonce[0] = 1
	secondNonce[0] = 2
	for _, info := range []*debug.BuildInfo{
		nil,
		{},
		{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef"}, {Key: "vcs.modified", Value: "true"}}},
	} {
		first := resourceListRendererBuildFingerprint(info, firstNonce)
		second := resourceListRendererBuildFingerprint(info, secondNonce)
		if first == second {
			t.Fatalf("unstable build did not depend on process nonce: %x", first)
		}
	}
}

func TestResourceListRendererFingerprintIsProcessStable(t *testing.T) {
	first := resourceListRendererFingerprint()
	second := resourceListRendererFingerprint()
	if first != second {
		t.Fatalf("renderer fingerprint changed inside one process: %x != %x", first, second)
	}
	if bytes.Equal(first[:], make([]byte, len(first))) {
		t.Fatal("renderer fingerprint is zero")
	}
}
