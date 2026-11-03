#!/bin/sh
# Signs catalog.yaml for the Raccoon CI catalog feed, with openssl only (no
# Raccoon CI binary needed), and checks the signature against the public key
# the clients trust before it is kept. The signature equals the one
# `raccoonci catalog sign` makes: ed25519 is deterministic.
#
#   RACCOONCI_CATALOG_SIGNING_KEY  the private key, base64 of the 32-byte seed
#                                   (from the repository's Actions secrets)
#   RACCOONCI_CATALOG_PUBLIC_KEY   the public key clients trust, base64
#
# Writes catalog.yaml.sig next to catalog.yaml. The key is read from the
# environment only and never printed.
set -eu

file=${1:-catalog.yaml}
: "${RACCOONCI_CATALOG_SIGNING_KEY:?the signing key is not set}"
: "${RACCOONCI_CATALOG_PUBLIC_KEY:?the public key is not set}"
[ -f "$file" ] || { echo "sign.sh: $file not found" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
umask 077

# An ed25519 private key in PKCS#8 is a fixed 16-byte header and the 32-byte seed.
{ printf '\060\056\002\001\000\060\005\006\003\053\145\160\004\042\004\040'
  printf %s "$RACCOONCI_CATALOG_SIGNING_KEY" | base64 -d; } > "$tmp/key.der"
[ "$(wc -c < "$tmp/key.der")" -eq 48 ] || { echo "sign.sh: the signing key is not a 32-byte seed in base64" >&2; exit 1; }
openssl pkey -inform DER -in "$tmp/key.der" -out "$tmp/key.pem"

openssl pkeyutl -sign -rawin -inkey "$tmp/key.pem" -in "$file" -out "$tmp/sig.bin"

# The public key as SPKI: a fixed 12-byte header and the 32-byte key.
{ printf '\060\052\060\005\006\003\053\145\160\003\041\000'
  printf %s "$RACCOONCI_CATALOG_PUBLIC_KEY" | base64 -d; } > "$tmp/pub.der"
openssl pkey -pubin -inform DER -in "$tmp/pub.der" -out "$tmp/pub.pem"
if ! openssl pkeyutl -verify -rawin -pubin -inkey "$tmp/pub.pem" -in "$file" -sigfile "$tmp/sig.bin" >/dev/null 2>&1; then
  echo "sign.sh: the signature does not verify under RACCOONCI_CATALOG_PUBLIC_KEY; the secret is not the key clients trust" >&2
  exit 1
fi

base64 < "$tmp/sig.bin" | tr -d '\n' > "$file.sig"
echo "signed $file -> $file.sig"
