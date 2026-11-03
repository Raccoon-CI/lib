# The Raccoon CI catalog feed

> This directory is the **template of the `Raccoon-CI/catalog` repository** (kept in lib; changes are copied to that repository by hand). Its files go to the root of that repository, `sign.sh` under `scripts/`, `publish.yml` under `.github/workflows/`.

Raccoon CI pulls `catalog.yaml` from this repository, and requires the signature
in `catalog.yaml.sig` to verify under the public key built into the program
(`TrustedKeys` in `pkg/catalog/keys.go`). The feed is data: a catalog only names
tools, toolchains, packages and images; it cannot contain commands or URLs.

## Files

- `catalog.yaml`: the feed. You edit this one.
- `catalog.yaml.sig`: its ed25519 signature (base64). Made by the workflow; do not edit it.
- `scripts/sign.sh`, `.github/workflows/publish.yml`: copies of `deploy/catalog-feed/` in the lib repository.

## Setting up (once)

1. Make a key pair (on your own machine; the private key never goes to a chat or a repository):
   ```
   openssl genpkey -algorithm ed25519 -out catalog-key.pem
   openssl pkey -in catalog-key.pem -outform DER | tail -c 32 | base64 -w0 > catalog-signing.key
   openssl pkey -in catalog-key.pem -pubout -outform DER | tail -c 32 | base64 -w0   # the public key
   rm catalog-key.pem
   ```
2. Put the content of `catalog-signing.key` into the repository's Actions secret
   `RACCOONCI_CATALOG_SIGNING_KEY` (Settings, Secrets and variables, Actions), then delete the file.
3. Put the public key in `TrustedKeys` (Raccoon CI) and in `RACCOONCI_CATALOG_PUBLIC_KEY` of the workflow.

## Publishing a change

Edit `catalog.yaml` and push to `main`. The workflow signs it and commits
`catalog.yaml.sig`. Until that commit is there, the raw files can briefly show
the new text with the old signature; a client that sees this rejects the feed
and keeps using its cache or the built-in catalog, then tries again later.
Branch protection that forbids the workflow's push has to allow it.

## Replacing the key

`TrustedKeys` can hold several keys. Add the new public key and release; when
the clients have it, sign with the new private key (change the secret and
`RACCOONCI_CATALOG_PUBLIC_KEY`); when the old key is no longer needed, remove
it from `TrustedKeys`.
