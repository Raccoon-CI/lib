package catalog

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// DefaultFeedURL is the feed pulled when none is named. Empty means there is
// no default feed: only a URL given on the command line is pulled.
const DefaultFeedURL = ""

// TrustedKeys are the base64 ed25519 public keys a feed must be signed by when
// no key is named. Several can be listed, so that a key can be replaced
// without a release that trusts none. With none listed and none named, a feed
// is not required to be signed.
var TrustedKeys = []string{}

// ResolveFeed turns what was asked for (a URL and a key list, either may be
// empty) into the feed to pull and the keys it must be signed by, using the
// defaults above for what is empty. An empty URL back means no feed.
func ResolveFeed(url, keys string) (string, []ed25519.PublicKey, error) {
	if url == "" {
		url = DefaultFeedURL
	}
	if keys == "" {
		keys = strings.Join(TrustedKeys, ",")
	}
	pubs, err := ParsePublicKeys(keys)
	return url, pubs, err
}

// GenerateKey makes a signing key pair, both base64: the public key (what
// -catalog-key and TrustedKeys take) and the private key (a 32-byte seed; keep
// it in a secret, never in a repository or on a command line).
func GenerateKey() (public, private string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(priv.Seed()), nil
}

// ParsePrivateKey reads a private key as GenerateKey prints it (a 32-byte seed
// in base64); the 64-byte form some tools print is accepted too.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("a catalog signing key is base64")
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, fmt.Errorf("a catalog signing key is %d bytes of base64 (or %d)", ed25519.SeedSize, ed25519.PrivateKeySize)
}

// PublicKeyOf is the base64 public key that belongs to a private key.
func PublicKeyOf(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// Sign signs a catalog's text; the result is what goes in the feed's ".sig" file.
func Sign(priv ed25519.PrivateKey, body []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body))
}
