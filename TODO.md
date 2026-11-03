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

- ✅ 库已发 `v0.1.0`（2026-11-03），runner、backend、cli 依赖它，三者的 CI 都已通过。⬜ 还缺 `CHANGELOG`。协议各有自己的版本号，每个仓库独立 semver。
- 坏历史（用户 2026-11-02 定：接受、不改写）：原单体仓库里有一段提交的 `internal/engine/engine.go` 不能编译（后来修好了）。那个文件现在在 runner 仓库，库里没有，所以库的历史不受影响；runner 仓库的 `CLAUDE.md` 写了那段的哈希。
- 许可证：Apache-2.0（用户 2026-11-02 定）。

---

（2026-11-03 从 cli 仓库的 TODO 搬来：`pkg/ghrunner` 和 `pkg/sched` 现在在 lib。下文的 `serve` 命令本身仍在 cli 仓库的 `cmd/raccoonci`。标题里的“A.x”仍是原单体 TODO 的编号。）

# A. 作为 GitHub Runner（`raccoonci serve`，`pkg/ghrunner`）

背景：2026-10-31 用户研究 Blacksmith 后认为"只用自己的引擎、不做 GitHub runner 编排"的判断可能有误，要求研究并做出这条路径。代码在 `pkg/ghrunner`，命令 `raccoonci serve`，设计和数字见 `docs/research/idle-runners/README.md`，架构说明见 `CLAUDE.md`。InstaBox 的 `ci` 模板（Ubuntu 24.04 = GitHub 官方 runner 镜像，runner 2.338.2 未注册）已上线，所以操作系统和镜像不再是问题。

## A.1 已完成与实测

- ✅ 主流程：按仓库轮询排队的 job（ETag 条件请求，304 不占额度）→ 标签前缀 `raccoon-` 的 job 生成 JIT runner + 起机器（并行）→ JIT 配置写进机器文件（0600，不进命令行和日志，测试断言）→ 跟到 GitHub 把 runner 删掉 → 删机器。真实跑通：job 成功，机密没有出现在日志里
- ✅ 实测启动时间（GitHub 报告的 queued→started）：现建机器 10 s；暂停的备用机（`-warm N`）7–9 s；已在线的空闲 runner 1–2 s。对照：GitHub 托管 3–4 s，InstaBox 官方 `instabox-medium` 12–20 s
- ✅ 冷启动时间花在哪（2026-10-31，读机器上 runner 自己的日志）：排队到开始 6–9 s，GitHub 托管 2 s。机器创建 1.6 s + runner 进程启动 1–2 s + runner 向 GitHub 认证并建立会话约 2 s + GitHub 把 job 派给它约 1 s + 我们发现不到 0.5 s。暂停的备用机已经没有价值：恢复要 1.6–3.2 s，不比新建快，runner 自己的启动在热机器上也没快；`-warm` 默认就是关的，保持关闭。剩下的大头是 GitHub 的 runner 协议，只能用等待中的 runner 遮住（1–2 s），去不掉
- ✅ 自适应保活（`keepalive.go`）：模型和在真实到达轨迹上的比较见研究笔记。每隔一段到达的间隔，按最近 100 次间隔选使"等待成本 + 延迟价值"最小的 TTL，不到 20 次用 ski-rental 保本点 `V·Δ/C`；池大小跟随到达的突发规模（最多 4）；多余的等待 runner 先删注册再退役（有 job 的 runner GitHub 不让删，所以安全）。`-second-value`（默认 0.01 美元/秒，0 关闭）、`-idle N`（下限）
- ✅ 已知到达提前启动（`prestart.go`、`cron.go`）：`schedule:` 的 cron 到点前 8 秒起 runner（保留到之后 10 分钟，GitHub 的定时 run 常晚几分钟）；`needs` 某个正在跑的 job 的下游 job，按上游最近几次时长的中位数在它结束前起 runner（没有历史就不猜）。真机：链式 a→b→c 第二次运行，b、c 排队到开始 2 s（第一次冷启动 a 16 s、因保活 b、c 也是 2 s）
- ✅ 被别的等待 runner 抢走 job 的 runner 约 10 秒后释放（以前要白等 3 分钟的 `IdleTimeout`）。实测中发现并修复；真机上这个修复本身还没重跑过，只有单测
- ✅ 组织级共享池（`-org`）：runner 登记在组织的 `Default` 组（私有仓库可用），一个仓库的突发留下的等待 runner 其他仓库也能用。真机：`bench-lint-docs` 的 job 之后，`bench-go-integration` 的 job 排队后 2 s 开始，用的是保留下来的 runner。只写 `-org` 不写 `-repo` 时监控组织里没归档的全部仓库
- ✅ 按信任分池（`-org` 下，`Service.Isolated`）：公开仓库自动隔离（`serve` 启动时逐个读可见性，读不到就报错退出，不会默认放进共享池），互不信任的私有仓库用 `-isolate owner/name`（可重复）指定。被隔离的仓库不进组织池，runner 登记在仓库本身，只接该仓库的 job，保活、提前起 runner 也按仓库单独算。只有单测（假 GitHub），没有真机验证。**没做**：组织的 `Default` 组本身是否对公开仓库开放没有检查（建议在 GitHub 上关掉 `allows_public_repositories`）；运行中新建的仓库不会重新分类；同一组织池内的私有仓库仍默认互信
- ✅ 按标签选机器规格（2026-10-31，`size.go`）：`raccoon-<N>vcpu` 起 N 核的机器，内存取 InstaBox 给的那一档（可用规格从 `GET /v1/orgs` 读，这个账号是 2/4、4/8、8/16 GiB）；没写规格的用默认。账号建不出的规格（`raccoon-64vcpu`）不会让 job 一直排队，而是在日志里写明能建什么并取消该 run。等待中的 runner 只有 `self-hosted` 和 `raccoon-4vcpu` 两个标签，所以只接这个标签的 job；其他规格的 job 一律新建机器（冷启动），保活和共享池暂时只管默认规格。真机：`raccoon-8vcpu` 的 job 里 `nproc`=8、内存 16 GB；`raccoon-64vcpu` 被拒并取消，没建机器
- 🟡 **webhook 取代轮询**（2026-10-31，`webhook.go`）：`serve -webhook-listen ADDR -webhook-url https://…`，给每个监控的仓库建/更新一个 `workflow_job` hook（`admin:repo_hook`，退出时删除），HMAC-SHA256 验签（错误签名一律 401，不读内容），队列中的 job 直接进和扫描同一个判断函数（重复投递、扫描同时发现都不会起两台），其余事件唤醒扫描；有 webhook 时每 30 秒（`-safety-poll`）才列一次仓库。顺带修了：启动失败的 job 以前不会重试。单测覆盖（签名、不轮询也能起机器、重投、非监控仓库、hook 注册与删除）。**真机已验证（2026-10-31）**：用户用 `instabox-port` 发布 18091 端口（我自己发布被权限拦下，没有绕开），GitHub 的 ping 回 204，未签名的 POST 经公网地址回 401，`bench-lint-docs` 的 a→b→c 推了两次：job 的 runner 在投递到达后 1.4 s 起好，冷启动的第一个 job 排队到开始 7 s（轮询时 10–16 s），后续 job 1–2 s；`serve` 结束后 hook、runner、机器都已清掉。没测：GitHub 投递失败时靠 30 秒兜底列表找回 job 的路径（只有单测）。组织级 hook 需要 `admin:org_hook`，现在是每个仓库一个。端口 18091 还发布着，由用户决定何时撤下
- ✅ 在 `push`/`pull_request` 事件到达时提前起 runner（2026-11-01，`event.go`，已提交）：webhook 现在订阅 `workflow_job`、`push`、`pull_request`；事件一到就读该提交上的各个 workflow，用 `trigger.Match`（分支、tag、`paths`、活动类型过滤，变更文件走 compare 接口，取不到就当未知）判断哪些会被触发，数出它们第一层、带我们标签、不受其他规格限制的 job（矩阵按个数算），立刻提前起那么多 runner，保留 1 分钟。基准用的 `workflow_dispatch` 没有这个事件，所以基准看不出来。**真机实测（`bench-lint-docs`，6 次 push）**：GitHub 从事件到把第一个 job 排上队稳定是 **2–3 s**（以前凭 1–3 s 猜）；提前起了 runner 的第一个 job 排队→开始是 5–7 s（冷启动是 6–9 s），**实际只快了约 1–2 s**，样本只有 6 次、在噪声范围内；6 台里有 1 台没被用上。原因是 runner 从起机到能接任务要约 7 s，而 GitHub 只提前了 2–3 s 给我们。想要更多就只能让 runner 更早在线（等待中的 runner，要花钱）。`pull_request` 没有真机测：刚开的 PR 的 `refs/pull/N/merge` 读不到（404，GitHub 异步生成合并提交；`refs/pull/N/head` 可以），所以 PR 事件读 workflow 用的是 PR 头提交。没做：静态求值 job 的 `if:`（为假的 job 也会起 runner）

## A.2 基准（serve 与 GitHub 托管、InstaBox 官方 runner 的对比）

基准在 https://github.com/Raccoon-CI/raccoonbench。**基准的原则（用户 2026-10-31）：保持客观，不能为了我们的产品改形状；结果不好就改产品。**自有引擎的基准（原单体 TODO 的 C.3）已移到 raccoonbench 仓库，不在这里。

- ✅ 完整对比（2026-10-31）：7 个 bench 仓库都有了 `bench-raccoon` 分支（`runs-on: raccoon-4vcpu`，和 `bench-github` 只差这一行），3 个有 `bench-raccoon-2c`（`raccoon-2vcpu`）；raccoonbench 新增 `raccoon-serve` 系统（`serve` 随会话启停，花费按 serve 的用量文件算）；价格文件改成 GitHub 当前价（2 核 $0.006/分钟，4 核 $0.012），`instabox-medium` 按 4 vCPU / 8 GiB 的模板估（推断）。第一轮：7 负载 × 4 系统 × 1 次；第二轮：3 负载 × 3 系统 × 3 次；第三轮：2 核对 2 核，3 负载 × 3 次。Blacksmith 和 Jog 因为要企业资质/预充 100 美元，已决定不比
- ✅ 第二轮基准（2026-10-31，3 个负载 × 3 个系统 × 每格 3 次取中位数，GitHub 托管是 2 核、其余 4 核）：和 InstaBox 官方 runner 比，serve（webhook）在 warm 和 change 场景稳定快 2–12 s（lint-docs warm 13 对 18 s，go-integration warm 26 对 32 s，typescript-monorepo warm 52 对 64 s），原因是排队只有 1–2 s 而官方是 6–11 s；cold 场景两者相当（18/20、49/47、75/74 s）。和 GitHub 托管比，重负载明显更快（typescript-monorepo warm 52 对 111 s，go-integration cold 49 对 72 s，主要来自 4 核对 2 核），短 job 的 cold 慢约 4 s（lint-docs 18 对 14 s）。花费上 serve 比官方 runner 的估算高 3–6 倍（它是 job 秒数 × medium 规格的估算，serve 是整台机器的寿命，含启动、等待 runner 和 50 GiB 根盘），但仍只有 GitHub 托管的 5%–20%；两边口径不同，不是严格可比
- ✅ 第三轮基准，2 核对 2 核（2026-10-31，GitHub 托管 `ubuntu-latest` 对 `raccoon-2vcpu`，3 个负载 × 每格 3 次取中位数，`serve -runner-label raccoon-2vcpu` 让等待的 runner 也是 2 核）：lint-docs cold 20 对 16 s（慢 4 s）、warm 14 对 15、change 14 对 15；go-integration cold 62 对 72、warm 27 对 42、change 34 对 46；typescript-monorepo cold 107 对 108、warm 67 对 103、change 75 对 106。花费约为 GitHub 的 2%–15%。**换成同样核数后，warm 和 change 仍然明显更快**，逐步骤对比（typescript-monorepo warm）：`pnpm install` 4 s 对 13 s，`pnpm build` 24–25 对 28–43 s，queue 都是 1–2 s。**原因（已用硬件测试证实，见下一条）：这个优势来自 InstaBox 的硬件，不是我们的调度**。我们自己的贡献在排队：有等待 runner 时 1–2 s，对 InstaBox 官方 runner 的 6–11 s（第二轮）。cold 的短 job 仍然慢 4 s，是 GitHub 协议的固定开销
- ✅ 硬件对比（2026-10-31，同一个脚本在两边各跑 3 次，GitHub `ubuntu-latest` 对 `raccoon-2vcpu`，都是 2 vCPU）：GitHub 托管是 AMD EPYC 7763、8 GB、根盘 `/dev/sda1` ext4；InstaBox 是 AMD EPYC、4 GB、`/dev/vda` ext4。3 万个 4 KiB 小文件：读 0.34 对 0.13–0.15 s（快约 2.4 倍），整树复制（缓存还原解包做的事）3.0–3.15 对 1.2–1.4 s（约 2.3 倍），全部硬链接（`pnpm install` 在 store 已热时做的事）0.75 对 0.17–0.19 s（约 4 倍），删除 0.78 对 0.30–0.35 s（约 2.4 倍），创建 1.4–1.5 对 0.69–1.99 s（波动大，一次比 GitHub 慢）；单线程 sha256 300 MB 0.20 对 0.14–0.17 s（约快 20%–30%）；顺序写 189–209 对 252–524 MB/s。结论：小文件元数据操作快 2–4 倍，单线程 CPU 快两成左右，这正好对上 warm 的 `pnpm install` 从 13 s 到 4 s。**限制**：每边只有 3 次；"清缓存后顺序读"在 InstaBox 上是 1.6–2.6 GB/s，太高，可能没有真的绕过宿主机缓存，不能当磁盘速度用；InstaBox 那边的 CPU 型号被截断，没确认是不是同一代。这个优势 InstaBox 官方 runner 也有，所以它不能算我们相对官方 runner 的差异
- ⬜ 剩下 4 个负载（docker-build、frontend-e2e、security-sbom、rust-large）的 2 核对比：用户判断重负载上已有答案（第一轮 4 核数据覆盖了全部 7 个负载），**不再跑**。报告里只有 3 个负载的 2 核数据，要如实写明，不能说成全部
- ✅ 清理（2026-11-01，用户同意）：组织里三个空的 `Blacksmith runners …` runner 组（0 个 runner、0 个仓库）已删，只剩 `Default`；`bench-frontend-e2e` 的分支 `blacksmith-migration-2b5b7fd` 已删，它上面 Blacksmith 机器人开着的 PR #1 先关闭。**还在组织里的**：Blacksmith、InstaBox、Jog 三个 GitHub App（没人要求动，未处理）；`bench-lint-docs` 里我为验证 `refs/pull/N/merge` 开了又关的 PR #1（GitHub 不能删 PR）

## A.3 未完成与未验证

- ⬜ **提高每分钟起机数量**（用户 2026-11-01 在查；和 lib 仓库里 `pkg/instabox` 的起机限额（原单体 TODO 的 C.2）是同一件事）：InstaBox Pro 每分钟最多起 10 台，是矩阵 fan-out 的瓶颈（见 A.4 的矩阵个数一段）。要确认能否提高（更高的套餐、或向 InstaBox 申请）。查到之前的备选：同一个 run 内复用机器（给刚跑完的机器换一份新的 JIT 配置，不占起机名额，代价是同一个 run 的 job 之间不再隔离，默认应关）；`-starts-per-minute` 已能跟着改
- ⬜ InstaBox 30 分钟自动挂起会不会把"有 runner 在监听的机器"当成空闲，没测
- ⬜ 真实 `schedule:` 触发没测（cron 计算和提前启动只有单测）
- ⬜ `actions/cache` 和 artifact 在这条路径上走 GitHub 自己的服务；能否重定向到 `pkg/cache` 没试
- ⬜ `pull_request` 事件没在真 PR 上测；run 的 `path` 的实际内容也没在真 PR 上验证（见 A.4 末尾）
- ⬜ 组织级 webhook（`admin:org_hook`）；保活和共享池按机器规格分开（现在只管默认规格）
- ⬜ 静态求值 job 的 `if:`，为假的 job 不起机器；同一个 run 里让下游 job 接着在上一个 job 的机器上跑；边车（从机器上读上游 job 的真实输出）

## A.4 两种 trigger 下的加速边界，和 Node 家族的专项优化（2026-10-31 讨论，**都还没做**）

**走 Actions trigger 时做不了的加速**（GitHub 决定"跑什么、什么时候跑"，我们只提供机器）：①跳过没受影响的 job 和按内容哈希重放整个 job（runner 只执行 GitHub 发来的步骤，不能改、不能省；取消会让 run 变 cancelled）；②去掉 GitHub 自己的派发延迟（冷启动约 4–5 s：建 run 排队 1–3 s、runner 认证建会话约 2 s、派发约 1 s；有等待 runner 时下限 1–2 s）；③提前精确知道整个任务图（动态矩阵、依赖上游输出的 `if:` 要等运行时；静态部分可以读 workflow 文件得到，下游 job 的时间只能预测）；④控制排队顺序和优先级（`concurrency`、队列是 GitHub 的）；⑤同一个 run 的 job 之间原生共享工作区和服务容器；⑥替换 GitHub 的缓存、artifact、OIDC、job summary 服务（令牌和地址来自 GitHub 的 job 消息，可能能用 runner 的 `.env` 或 job-started 钩子重定向，没试过）；⑦替换 `actions/checkout`。**能做但没做**：同一台机器上多个一次性 runner 打包短 job、环境快照（sticky disk 式）、同一个 run 内复用机器、按规格保活、push/PR 事件到达时提前启动。**只有自己的 trigger 才能做**：跳过没受影响的 job、没有 GitHub 的派发下限、精确的任务图、排队控制。**自己的 trigger 做不了或代价更高**：GitHub 的全部语义（可复用 workflow、`concurrency`、环境和保护规则、OIDC、必需检查、PR 界面）、macOS/Windows/ARM（GitHub runner 装在用户自己的机器上就行）。

**"跑了但实际没跑"（Actions 下跳过没受影响的 job）的两种做法**：①改造普通 runner：把 `bash`/`sh`/`python` 和 runner 自带的 `node` 换成直接退出 0 的小程序，界面上每步显示成功，要靠日志和注解说明；②自己实现 runner 协议的假 runner，把每步报成 Skipped、job 报成功，可以放在控制器上不起虚拟机（约 3 s），但协议没有官方文档（`github-act-runner` 是开源实现，没验证）。判断"该不该跳"用现有的 `pkg/affected`（在 lib 仓库，原单体 TODO 的 B.6），输入是变更文件列表和仓库的包依赖图。**最大风险是假绿**，所以要：默认关闭、按仓库开启；默认分支 push、tag、`schedule`、`workflow_dispatch` 一律全跑，只在 PR 上跳；workflow、本地 action、配置文件变了就全跑；任何不确定一律全跑；有 `outputs`、上传 artifact、`environment`、`container`/`services` 的 job 不跳；提供手动覆盖（PR 标签或 `[ci full]`）；"审计模式"：被判可跳的 job 偶尔真的跑，统计漏判率。**先做的不是实现，而是用真实仓库的历史做离线回放**：最近若干提交里按规则会跳过多少 job，对照这些 job 当时实际是否成功，看漏判率；漏判率不能接近零就不做。只对 monorepo 里常常不受影响的长 job 有用，单 job 仓库（基准负载）没用。

**Node 家族专项（用户 2026-10-31 大致同意；A、B 两条路径都能用）**：Vercel 的按需构建是 **Turborepo**（任务级内容哈希缓存，输入没变就重放输出和日志，`--affected` 只跑受影响的包）；**Turbopack** 是 Next.js 里的增量打包器（进程内函数级缓存），两者思路相同、粒度不同。原则：**让用户自己的工具按内容哈希来跳过，我们负责把缓存放在近处**，正确性由工具保证，不会有我们猜依赖导致的假绿。按把握从大到小：
- ⬜ **注入 Turborepo / Nx 的远程缓存**：据我所知两者都有公开的 HTTP 接口、可自托管（**没核对细节**）；用我们的对象存储（`pkg/blob`）写一个小服务，用 runner 目录里的 `.env` 注入 `TURBO_API`/`TURBO_TOKEN` 等变量，用户不用改 workflow。Actions 和自己的 trigger 两种路径都能用。验证办法：`create-turbo` 生成真实项目，写最小的缓存服务，跑两次 `turbo run build`，看是否命中、重放、省多少。风险最大的一项，因为依赖对接口的记忆
- ⬜ **把 Node 和包管理器预先放进工具缓存**：数据里 `typescript-monorepo` warm 中 `pnpm/action-setup` 6 s + `setup-node` 7–8 s ≈ 14 s，占 52–67 s 的两成多；`setup-node` 在工具缓存里找到版本就不下载。排队时本来就会读 workflow，用现有的 `scan` 和 `provision` 在 runner 接 job 之前备好版本（窗口约 3–4 s，要预先把包放在机器上）
- ⬜ **热盘保留框架的构建缓存**：`.next/cache`、`.turbo`、`node_modules/.cache`、tsc 增量信息、eslint 和 jest 缓存；Turbopack 的磁盘缓存位置待确认。属于环境快照（sticky disk）那一类
- ⬜ **预装重型二进制**：Playwright 浏览器、Cypress、`postinstall` 下载的原生模块；`frontend-e2e` 里 `npx playwright install` 每次都做，占多少还没量
- ✅ 前后端分离的并行：本来就是不同 job、各自一台机器
- 先做的两个小验证：①用现有步骤耗时数据算出 Node 类 job 里"装环境"类步骤占多少（第 2、4 项的天花板，几乎不花钱）；②Turborepo 远程缓存的最小实验

**任务图**（读 workflow 源文件，见 A.1 的提前启动）：目前只用在"下游 job 的提前启动"。读到的静态信息还能：一次得到所有 job、`needs`、矩阵个数、`runs-on` 标签；用 `plan` 求值静态的 `if:`，为假的 job 不起机器；在 push/PR 事件到达时一次性提前起所有第一层 job（需要订阅 `push` 事件）；同一个 run 里让下游 job 接着在上一个 job 的机器上跑（热的包管理器 store 和工具缓存，限同一个 run）；轮询上游 job 的步骤进度（`GET jobs/{id}` 带 `steps`）来修正下游启动时间（**已做**，`progress.go`，2026-11-01 真机验证：一个带 5–24 s 随机步骤的 build 加一个 deploy，8 次预测，按步骤的平均绝对误差 5.3 s、只按中位数 5.0 s，但按步骤从未偏晚（误差 −9 到 −2 s），只按中位数有 4/8 偏晚（最晚 +9 s，runner 约 7 s 才能启动，所以是冷启动）；有历史后下游 job 都是 1 s 开始。样本只有 8 次，而且是故意设计出大方差的情形，真实 workflow 差距会小）。**矩阵个数**（**已做**，`matrix.go`）：静态矩阵直接从 workflow 数（受 `max-parallel` 和 `MaxJobs` 限制），含 `${{ }}` 的动态矩阵用同一个 job 在最近 5 次里展开个数的**最大值**（不是中位数：多起一台等待 runner 约花 15 s 机器费，少起一台让一个 job 多等约 7 s 冷启动，代价差几百倍）。真机（2026-11-01）：中位数版本 8 次（个数 2 4 3 2 2 5 5 4），25 个有历史的 job 里 8 个走了冷启动、浪费 2 台；最大值版本每 100 s 一个 run、8 次，有历史的 7 个 run 共 28 个 job 里只有 1 个冷启动（历史最大 4、这次 5）、浪费 5 台。**但名额紧张时没有用**：每 62 s 一个 run（每个 run 5–6 次起机，对每分钟 10 台）时，限额器挡下了一部分提前起的 runner（以前这条路径不打日志，现在有 "N of those M runner(s) not started"），最大值版本和中位数版本一样约三成 job 冷启动。所以这是起机名额的问题，不是算法的问题：一个仓库每分钟一个大 fan-out 就会顶到 Pro 套餐的上限。可能的出路：同一个 run 内复用机器（给空闲的机器换一份新的 JIT 配置，不占起机名额；代价是同一个 run 的 job 之间不再隔离，要慎重）、更高的套餐、或者把名额预留比例调整。**起机限额**（**已做**，`starts.go`）：InstaBox Pro 每分钟最多起 10 台，`-starts-per-minute`；之前撞到 429 三次就永久放弃 job。**runner 可互换**（**已做**）：runner 会去跑最老的排队 job 而不是为它启动的那个，这个 job 之后没有 runner 就会被遗忘，现在 runner 走后若 job 还在排队就再要一个。三个都是真机实验发现的缺陷，各有一个去掉修复就会失败的测试。**读错 workflow 版本的缺口**（**已修**，2026-11-01，已提交，`workflowOfRun`）：`pull_request` 的 run 用合并提交上的 workflow 文件，`pull_request_target` 和 `workflow_run` 用基础分支或默认分支的，都不是 run 的 `head_sha`；现在这几类事件按 run 的 `path` 里 `@` 后面的 ref 读（读不到再退回提交号），其余事件仍按提交号。顺带修了 `workflowFile` 对分支和 tag 永久缓存的问题（`refreshSchedules` 读的是默认分支，所以 `main` 上改了 cron 永远发现不了），现在不是完整提交号的 ref 只缓存 5 分钟。四类事件的测试在去掉修复时会失败。**没有在真 PR 上验证 run 的 `path` 的实际内容**（凭对 GitHub 行为的了解，测试里用的是假的）。**任务图里还没做的**：静态求值 job 的 `if:`，为假的 job 不起机器；同一个 run 里让下游 job 接着在上一个 job 的机器上跑；边车（从机器上读上游 job 的真实输出）。

---

---
