GO ?= go

.PHONY: build test race vet fmt-check check notices

# A library has no binaries; build only proves everything compiles.
build:
	$(GO) build ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }

# Everything that must pass before merging.
check: fmt-check vet race

# Regenerate THIRD_PARTY_NOTICES' inputs (needs go-licenses on PATH).
notices:
	go-licenses report ./... --ignore github.com/Raccoon-CI/lib
