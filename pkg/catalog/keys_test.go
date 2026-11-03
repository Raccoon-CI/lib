package catalog

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSourceSeveralKeys(t *testing.T) {
	oldPub, oldPriv := newKey(t)
	newPub, newPriv := newKey(t)
	strangerPub, _ := newKey(t)
	ctx := context.Background()

	// Any one key that verifies is enough.
	for name, signer := range map[string]ed25519.PrivateKey{"old": oldPriv, "new": newPriv} {
		f := newFeed(t, zigCatalog)
		f.sig = Sign(signer, []byte(zigCatalog))
		src, _ := f.source(t)
		src.PublicKeys = []ed25519.PublicKey{strangerPub, oldPub, newPub}
		if c, st := src.Load(ctx, Default(), false); st.Origin != FromNetwork || !hasZig(c) {
			t.Errorf("signed by the %s key: %+v", name, st)
		}
	}

	// None of them: refused.
	f := newFeed(t, zigCatalog)
	f.sig = Sign(newPriv, []byte(zigCatalog))
	src, _ := f.source(t)
	src.PublicKeys = []ed25519.PublicKey{strangerPub, oldPub}
	if c, st := src.Load(ctx, Default(), false); hasZig(c) || st.Err == nil || st.Origin != BuiltIn {
		t.Errorf("signed by no listed key: %+v", st)
	}
}

func TestSourceKeyRotation(t *testing.T) {
	oldPub, oldPriv := newKey(t)
	newPub, _ := newKey(t)
	ctx := context.Background()

	f := newFeed(t, zigCatalog)
	f.sig = Sign(oldPriv, []byte(zigCatalog))
	src, _ := f.source(t)
	src.PublicKeys = []ed25519.PublicKey{oldPub}
	if _, st := src.Load(ctx, Default(), false); st.Origin != FromNetwork {
		t.Fatalf("seed the cache: %+v", st)
	}

	// A release that trusts both keys still verifies the old cache, offline.
	f.down.Store(true)
	rotated := *src
	rotated.PublicKeys = []ed25519.PublicKey{newPub, oldPub}
	if c, st := rotated.Load(ctx, Default(), true); st.Origin != FromCache || !hasZig(c) {
		t.Errorf("old cache under old+new keys: %+v", st)
	}
	// Once the old key is retired the old cache is no longer trusted.
	retired := *src
	retired.PublicKeys = []ed25519.PublicKey{newPub}
	if c, st := retired.Load(ctx, Default(), true); hasZig(c) || st.Origin != BuiltIn {
		t.Errorf("old cache under the new key only: %+v", st)
	}
}

func TestStatusBody(t *testing.T) {
	ctx := context.Background()
	f := newFeed(t, zigCatalog)
	src, now := f.source(t)

	check := func(label string, want Origin) {
		t.Helper()
		c, st := src.Load(ctx, Default(), label == "network" || label == "not modified")
		if st.Origin != want {
			t.Fatalf("%s: origin %v, want %v (%v)", label, st.Origin, want, st.Err)
		}
		if string(st.Body) != zigCatalog {
			t.Fatalf("%s: body %q", label, st.Body)
		}
		again, err := Default().Overlay(st.Body)
		if err != nil {
			t.Fatalf("%s: overlay of Body: %v", label, err)
		}
		if !reflect.DeepEqual(again, c) {
			t.Errorf("%s: Overlay(Body) is not the catalog Load returned", label)
		}
	}
	check("network", FromNetwork)
	*now = now.Add(time.Hour)
	check("cache", FromCache)
	*now = now.Add(48 * time.Hour)
	// A conditional request that answers 304 reuses the cached text.
	src.MaxAge = time.Hour
	check("not modified", NotModified)

	// The built-in catalog has no body.
	f.down.Store(true)
	fresh, _ := f.source(t)
	if _, st := fresh.Load(ctx, Default(), false); st.Origin != BuiltIn || st.Body != nil {
		t.Errorf("built-in: %+v", st)
	}
}

func TestParsePublicKeys(t *testing.T) {
	a, _ := newKey(t)
	b, _ := newKey(t)
	ea, eb := base64.StdEncoding.EncodeToString(a), base64.StdEncoding.EncodeToString(b)

	for _, in := range []string{ea + "," + eb, ea + ", " + eb + "\n", " " + ea + "\n" + eb} {
		got, err := ParsePublicKeys(in)
		if err != nil || len(got) != 2 || !got[0].Equal(a) || !got[1].Equal(b) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if got, err := ParsePublicKeys(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
	if _, err := ParsePublicKeys(ea + ",short"); err == nil {
		t.Error("a bad key in the list must be an error")
	}
}

func TestGenerateAndSign(t *testing.T) {
	pubS, privS, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivateKey(privS)
	if err != nil {
		t.Fatal(err)
	}
	if PublicKeyOf(priv) != pubS {
		t.Error("PublicKeyOf doesn't match the generated public key")
	}
	// The 64-byte form is accepted and is the same key.
	long, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(priv))
	if err != nil || PublicKeyOf(long) != pubS {
		t.Errorf("64-byte form: %v", err)
	}
	if _, err := ParsePrivateKey("not base64!"); err == nil {
		t.Error("garbage private key accepted")
	}
	if _, err := ParsePrivateKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Error("short private key accepted")
	}

	// A signature made here is accepted by a Source that trusts the public key.
	f := newFeed(t, zigCatalog)
	f.sig = Sign(priv, []byte(zigCatalog)) + "\n"
	src, _ := f.source(t)
	pubs, err := ParsePublicKeys(pubS)
	if err != nil {
		t.Fatal(err)
	}
	src.PublicKeys = pubs
	if c, st := src.Load(context.Background(), Default(), false); st.Origin != FromNetwork || !hasZig(c) {
		t.Errorf("round trip: %+v", st)
	}
	// ... and not for different text.
	f2 := newFeed(t, zigCatalog)
	f2.sig = Sign(priv, []byte("other"))
	src2, _ := f2.source(t)
	src2.PublicKeys = pubs
	if _, st := src2.Load(context.Background(), Default(), false); st.Err == nil || st.Origin != BuiltIn {
		t.Errorf("signature of other text accepted: %+v", st)
	}
}

func TestResolveFeed(t *testing.T) {
	oldKeys := TrustedKeys // DefaultFeedURL is a const: only the keys can be swapped
	defer func() { TrustedKeys = oldKeys }()

	// With no defaults, nothing in is nothing out.
	TrustedKeys = nil
	url, keys, err := ResolveFeed("", "")
	if err != nil || url != DefaultFeedURL || len(keys) != 0 {
		t.Errorf("no defaults: %q %v %v", url, keys, err)
	}
	// What is named wins.
	a, _ := newKey(t)
	url, keys, err = ResolveFeed("https://x.test/c.yaml", base64.StdEncoding.EncodeToString(a))
	if err != nil || url != "https://x.test/c.yaml" || len(keys) != 1 || !keys[0].Equal(a) {
		t.Errorf("named: %q %v %v", url, keys, err)
	}
	// Built-in keys apply when none is named, and a named key replaces them.
	b, _ := newKey(t)
	TrustedKeys = []string{base64.StdEncoding.EncodeToString(b)}
	if _, keys, err = ResolveFeed("", ""); err != nil || len(keys) != 1 || !keys[0].Equal(b) {
		t.Errorf("default keys: %v %v", keys, err)
	}
	if _, keys, _ = ResolveFeed("", base64.StdEncoding.EncodeToString(a)); len(keys) != 1 || !keys[0].Equal(a) {
		t.Errorf("named key must replace the default: %v", keys)
	}
	if _, _, err = ResolveFeed("", "junk"); err == nil {
		t.Error("a bad key must be an error")
	}
}

// Every built-in key must be a usable public key: a typo here would make every
// feed fail to verify in a release.
func TestBuiltInTrustedKeysAreValid(t *testing.T) {
	keys, err := ParsePublicKeys(strings.Join(TrustedKeys, ","))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != len(TrustedKeys) {
		t.Fatalf("%d built-in keys parsed to %d", len(TrustedKeys), len(keys))
	}
}
