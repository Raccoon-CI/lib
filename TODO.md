# lib 进度与待办

更新于 2026-11-03。拆仓库后的第一版：只留属于共用库的条目；A（GitHub 触发路径）在 cli，B（自有引擎）在 runner，C.1（服务端）在 backend。`make check` 全绿。

图例：✅ 完成　🟡 部分完成　⬜ 未开始

## 库里有什么

`pkg/{value,expr,workflow,trigger,cron,catalog,runnerimages,platform,wire,dispatch,runevent,jobgraph,instabox,blob,artifact,cache,archive,affected}`，私有帮手 `internal/{azblob,twirp}`，以及 `deploy/catalog-feed`（`Raccoon-CI/catalog` 仓库的模板，见下）。依赖方向：lib ← runner ← cli，lib ← backend；库不依赖任何兄弟仓库。

守护测试（保持绿，改白名单要有理由）：`pkg/deps_test.go`（`pkg/*` 的传递依赖只能是标准库、第三方、其他 `pkg/*`、`internal/azblob`、`internal/twirp`）；`wire`、`runevent`、`jobgraph` 各有更严的"只依赖数据包"白名单。拆分时发现这三个守护测试里的旧 module 前缀没有跟着改，成了空转（永远通过），已改回有效并用临时越界 import 验证过会报错。

## 1. catalog 与 feed（✅ 已发布）

- ✅ `Raccoon-CI/catalog` 仓库已发布：feed 是它的 `catalog.yaml`（`https://raw.githubusercontent.com/Raccoon-CI/catalog/main/catalog.yaml`），旁边是 ed25519 签名 `catalog.yaml.sig`，由该仓库的 Actions 签（私钥只在它的 Actions secret `RACCOONCI_CATALOG_SIGNING_KEY` 里，用户自己生成）。
- ✅ 公钥已内置：`pkg/catalog/keys.go` 的 `TrustedKeys` 有 1 把，`TestBuiltInTrustedKeysAreValid` 守着它能解析。`DefaultFeedURL` 已生效：以后不带 `-catalog-url` 的命令也会拉它（缓存 24 小时，拉不到退回缓存再退回内置）。不带 `-catalog-key` 却显式给 `-catalog-url` 时，feed 必须由内置钥签过，用别人的 feed 要自己给 `-catalog-key`。
- ✅ 库里的机制：`Source.PublicKeys`（多把公钥，任一验过即可，轮换时新旧同列）、`Status.Body`（验过的原文）、`ParsePublicKeys`、`GenerateKey`/`ParsePrivateKey`/`PublicKeyOf`/`Sign`、`ResolveFeed`；拉取只允许 https、有大小上限、ETag 条件请求、验过才写缓存、读缓存时重新验证。
- ✅ `deploy/catalog-feed/` 保留在库里，它是 `Raccoon-CI/catalog` 仓库的**模板**（`sign.sh` 只用 openssl 签名并在写 `.sig` 前用客户端信任的公钥验过、`publish.yml`、起步的 `catalog.yaml`、中英文 README）。改模板后要手工同步到 catalog 仓库。
- ⬜ 还剩：feed 目前只是起步的 `schema: 1`，里面还没有任何覆盖项；catalog 仓库没开分支保护时测过，保护开启后要放行 `github-actions[bot]`；`sign.sh` 没有自动测试。

## 2. 算力提供方里属于库的部分（原 C.2）

- 🟡 `pkg/dispatch`（机器上的 agent 与主控协议）、`pkg/instabox`（API 客户端 + `Provider`、备用机、起机名额 `StartLimit`、用量记录）。机器系统与 InstaBox 实测记录在 runner / backend 的 TODO 里，库这边只记接口。
- ⬜ 起机名额：InstaBox Pro 每分钟最多起 10 台，`instabox.StartLimit` 只是在名额内排队，不能提高名额；用户在查能否提高。
- ⬜ Boat.dev、DigitalOcean / Linode：第二个 provider 出现时再决定是否把 `instabox` 拆成 `provider/instabox`（拆分计划 D.5 第 4 条，现在只有一个 provider，过早拆是白添一个仓库）。

## 3. 公开 API 表面积（原 D.7.2；🟡 只做了核查，没改名）

库的公开 API 合计约 190 个导出名。包外（别的仓库）真正用到的名字数（按拆分前的实测，不含方法，不含测试）：

| 包 | 导出 / 包外用 | 说明 |
|---|---|---|
| `value` | 8 / 8 | |
| `expr` | 12 / 11 | |
| `workflow` | 9 / 7 | 没人用：`MaxMatrixJobs`、`Strategy`（`Strategy` 在别的签名里出现，不能取消导出） |
| `trigger` | 6 / 5 | |
| `catalog` | **37 / 16** | 21 个包外没用，多是 `Catalog` 字段里的 `*Def`/`*Rule` 数据载荷类型 |
| `runnerimages` | 7 / 5 | |
| `runevent` | 19 / 19 | |
| `wire` | 7 / 6 | |
| `dispatch` | 11 / 6 | `ProtocolVersion` 等只在包内 |
| `instabox` | **22 / 13** | 9–10 个包外没用，`CreateRequest`/`MachineInfo`/`Operation` 是 API 载荷类型 |
| `blob` | 16 / 13 | |
| `artifact` | 10 / 9 | |
| `cache` | 5 / 5 | |
| `cron`、`platform`、`archive` | 2 / 2 | |

- ⬜ 表面积不大，但 `catalog` 和 `instabox` 最值得在承诺稳定前瘦身。**按真实使用者出现再收窄**：拆分前严格按"包外零引用、不在任何公开签名/字段/序列化里、行为不变"的标准核查过，两个包都没有可以安全改小写的顶层名字（catalog 37→37，instabox 22→22），所以没有改名。包外零引用的名字都落在这些类：`Catalog` 导出字段的数据载荷类型（`ActionDef`、`ToolDef`…）、`Status.Origin` 的取值、`VersionFile.Format` 的取值、哨兵错误 `ErrNewerSchema`、`Parse`（"从字节解析完整 catalog"的唯一入口）；instabox 的 `Machine`/`Operation`/`Spec`/`Summary`/`Prices`（方法参数或返回值）、`DefaultURL`、`IsRateLimit`、`ReadUsage`（与 `WriteUsage` 成对）。
- ⬜ 导出**方法**这一层还没动：catalog 的 `Catalog.{Command,CompatMessage,ImageForLabel,Installs,IsOpaque,NodeMajor,OSAvailable,OSOfLabel,OSUnavailable,Requires,ToolchainNames,VersionFile,Wrapper}`、`ActionDef.IsVersionRef`、`Source.Load`；instabox 的 `Client.{AllowedSizes,BuildRunnerSnapshot,CreateMachine,CreateMachineSpec,DeleteSnapshot,Exec,GetMachine,ListMachines,PutFile,Resume,SetMetadata,Suspend,Wait}`、`Pool.{Close,Machines}`、`Prices.{Cost,PerSecond,Summarize}`、`Provider.{Acquire,AcquireSize,Release,StartupTime,Usage,Wait}`、`StartLimit.{Refused,Wait}`。包外没人直接调，但多半是库本身的查询/客户端 API，也可能被接口满足，**全部保留**；先看哪些仓库真依赖哪个包，再逐个改。
- ⬜ 稳定性承诺（拆分决定 6）：只承诺 `value`、`workflow`、`expr`、`trigger`、`catalog` 的数据结构和 `wire`/`dispatch` 协议，其余写明不稳定（0.x）。协议有自己的版本号（`dispatch`、`wire`/`InstanceSpec`）；"旧机器忽略新字段"的做法要保持。
- ⬜ 统计 AST 工具没有入库（临时脚本，约 150 行，数 `pkg.Name` 的包外引用）；想入库的话建议放 `tools/apiscan`，用来检查"有没有越界依赖"和重做上表。

## 4. 选项：`pkg/` 前缀以后可去掉

库现在是一个独立仓库，包都在 `pkg/` 下（拆分前为了和 `internal/` 区分而挪的）。独立之后这个前缀只是多一层目录，import 路径是 `github.com/Raccoon-CI/lib/pkg/<包>`。**以后可以去掉**，改成 `github.com/Raccoon-CI/lib/<包>`：机械替换 import、`git mv`、改 `pkg/deps_test.go` 的前缀判断和各守护测试里的路径；代价是 runner / cli / backend 要同步改 import，所以最好在 `v0.1.0` 之前、还没有外部使用者时决定，发布之后再改就是破坏性变更。⬜ 未决定，不影响现在的任何功能。

## 5. 发布

- ⬜ 库先发 `v0.1.0`（带 `CHANGELOG`）；runner、backend、cli 的 CI 要等它的 tag 才能过。协议各有自己的版本号，每个仓库独立 semver。
- ⬜ 拆分前的历史里有一段坏历史（提交 `0c3ca27` 到 `45906d6` 之间，`internal/engine` 相关），以拆分计划 D.4 第 2 条为准；本仓库的历史是 `git filter-repo` 之后的，需要时先查它再用 `git bisect`。
- 许可证：Apache-2.0（用户 2026-11-02 定）。
