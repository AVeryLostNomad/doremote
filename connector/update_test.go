package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestVerifyManifest(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	body := []byte(`{"version":"1.2.3","assets":{}}`)
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body)) + "\n")

	if err := verifyManifest(pub, body, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	tampered := []byte(`{"version":"9.9.9","assets":{}}`)
	if verifyManifest(pub, tampered, sig) == nil {
		t.Fatal("signature accepted for a different manifest")
	}
	if verifyManifest(otherPub, body, sig) == nil {
		t.Fatal("signature accepted under a different key")
	}
	if verifyManifest(pub, body, []byte("not base64!")) == nil {
		t.Fatal("garbage signature accepted")
	}
	if verifyManifest(pub, body, nil) == nil {
		t.Fatal("missing signature accepted")
	}
}

func TestUpdatePublicKeyDecodes(t *testing.T) {
	k, err := base64.StdEncoding.DecodeString(updatePublicKey)
	if err != nil || len(k) != ed25519.PublicKeySize {
		t.Fatalf("updatePublicKey is not a base64 Ed25519 key: %v (%d bytes)", err, len(k))
	}
}

func TestVersionNewer(t *testing.T) {
	for _, c := range []struct {
		candidate, current string
		want               bool
	}{
		{"0.1.2", "0.1.1", true},
		{"0.2.0", "0.1.9", true},
		{"1.0.0", "0.99.99", true},
		{"0.1.10", "0.1.9", true}, // numeric, not lexical
		{"0.1.1", "0.1.1", false},
		{"0.1.0", "0.1.1", false}, // replayed older manifest
		{"0.9.0", "1.0.0", false},
	} {
		got, err := versionNewer(c.candidate, c.current)
		if err != nil || got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v, %v; want %v", c.candidate, c.current, got, err, c.want)
		}
	}
	for _, bad := range []string{"dev", "0.0.0-dev", "1.2", "1.2.3.4", "1.-2.3", ""} {
		if _, err := versionNewer(bad, "0.1.0"); err == nil {
			t.Errorf("versionNewer(%q) accepted a malformed version", bad)
		}
	}
}
