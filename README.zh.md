# lib

[Raccoon CI](https://github.com/Raccoon-CI) 的共用库。Raccoon CI 是自托管的 CI/CD 平台,把 GitHub Actions 的 workflow 搬到你自己选的算力上。Module:`github.com/Raccoon-CI/lib`。库里只有数据和协议,自己不执行任何东西,没有可执行文件。

English version: [README.md](README.md).

## 里面有什么

| 包 | 内容 |
|---|---|
| `pkg/value`、`pkg/expr`、`pkg/workflow`、`pkg/trigger`、`pkg/cron` | 动态数据模型、`${{ }}` 表达式求值、workflow YAML 解析、事件匹配、cron 解析 |
| `pkg/catalog`、`pkg/runnerimages`、`pkg/platform` | 以数据形式存放的工具知识(`catalog.yaml`,分层:内置、签名 feed、本地文件)、GitHub runner 镜像预装了什么、哪些平台可用 |
| `pkg/wire`、`pkg/dispatch`、`pkg/runevent`、`pkg/jobgraph` | 主控与机器之间的协议、runner 发出的事件、作业图(`needs`、矩阵、`if:`) |
| `pkg/instabox` | InstaBox 的 API 客户端和 provider |
| `pkg/blob`、`pkg/artifact`、`pkg/cache`、`pkg/archive` | artifact 与 cache 的协议服务及其存储 |
| `pkg/affected` | 改动的文件涉及哪些 job |
| `pkg/ghrunner`、`pkg/sched` | `raccoonci serve` 背后由 GitHub 触发的路径(为 job 现起机器来跑),以及哪些短 job 实例共用一台机器 |
| `internal/azblob`、`internal/twirp` | `artifact` 和 `cache` 的私有帮手 |
| `deploy/catalog-feed` | **[`Raccoon-CI/catalog`](https://github.com/Raccoon-CI/catalog) 仓库的模板**,那个仓库发布签名的 catalog feed(`sign.sh`、发布工作流、起步的 `catalog.yaml`、中英文 README)。它不是 Go 代码,改动要手工同步到那个仓库。 |

目前不承诺任何 API 稳定(0.x),还要收窄的公开表面见 [TODO.md](TODO.md)。

## 构建与测试

```sh
make check    # gofmt 检查 + go vet + go test -race ./...
make test     # go test ./...
make build    # go build ./...
```

`pkg/deps_test.go` 保证 `pkg/*` 只依赖标准库、第三方模块、其他 `pkg/*`、`internal/azblob` 和 `internal/twirp`;`wire`、`runevent`、`jobgraph` 另有更严格的守护测试。

## 和其他仓库的关系

lib ← runner ← cli,lib ← backend。本仓库不依赖其中任何一个。`runner`(机器端和本机引擎)、`cli`、`backend`(服务端)都建立在它之上,它们依赖本模块的 `v0.1.0` 标签。

## 许可证

Apache-2.0,见 [LICENSE](LICENSE)。第三方声明:[THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES)。
