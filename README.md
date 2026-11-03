# lib

The shared library of [Raccoon CI](https://github.com/Raccoon-CI), a self-hosted CI/CD platform that moves GitHub Actions workflows onto compute you choose. Module: `github.com/Raccoon-CI/lib`. It is data and protocols and runs nothing itself; there is no binary.

Chinese version: [README.zh.md](README.zh.md).

## What is in it

| package | what it is |
|---|---|
| `pkg/value`, `pkg/expr`, `pkg/workflow`, `pkg/trigger`, `pkg/cron` | the dynamic data model, the `${{ }}` expression evaluator, the workflow YAML parser, event matching, the cron parser |
| `pkg/catalog`, `pkg/runnerimages`, `pkg/platform` | tool knowledge as data (`catalog.yaml`, layered: built-in, signed feed, local file), what GitHub's runner images preinstall, which platforms are available |
| `pkg/wire`, `pkg/dispatch`, `pkg/runevent`, `pkg/jobgraph` | the protocol between a controller and a machine, the events a runner emits, the job graph (`needs`, matrices, `if:`) |
| `pkg/instabox` | the InstaBox API client and provider |
| `pkg/blob`, `pkg/artifact`, `pkg/cache`, `pkg/archive` | the artifact and cache protocol services and their storage |
| `pkg/affected` | which jobs the changed files concern |
| `pkg/ghrunner`, `pkg/sched` | the GitHub-triggered path behind `raccoonci serve` (runs a job on a machine started for it), and which short job instances share a machine |
| `internal/azblob`, `internal/twirp` | private helpers of `artifact` and `cache` |
| `deploy/catalog-feed` | the **template of the [`Raccoon-CI/catalog`](https://github.com/Raccoon-CI/catalog) repository**, which publishes the signed catalog feed (`sign.sh`, the publish workflow, a starter `catalog.yaml`, READMEs). It is not Go code; changes are copied to that repository by hand. |

No API is promised stable yet (0.x). See [TODO.md](TODO.md) for the surface still to be narrowed.

## Build and test

```sh
make check    # gofmt check + go vet + go test -race ./...
make test     # go test ./...
make build    # go build ./...
```

`pkg/deps_test.go` keeps `pkg/*` resting only on the standard library, third-party modules, other `pkg/*`, `internal/azblob` and `internal/twirp`; `wire`, `runevent` and `jobgraph` have stricter guards of their own.

## Relation to the other repositories

lib <- runner <- cli, and lib <- backend. This repository depends on none of them. `runner` (machine side and local engine), `cli` and `backend` (the server) build on it. They require tag `v0.1.0` of this module.

## Licence

Apache-2.0, see [LICENSE](LICENSE). Third-party notices: [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).
