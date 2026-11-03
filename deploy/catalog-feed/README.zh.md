# Raccoon CI 的 catalog feed

> 这个目录是 **`Raccoon-CI/catalog` 仓库的模板**(放在 lib 里,改动要手工同步到那个仓库)。文件放到那个仓库的根目录,`sign.sh` 放 `scripts/`,`publish.yml` 放 `.github/workflows/`。

Raccoon CI 从这个仓库拉 `catalog.yaml`,并要求 `catalog.yaml.sig` 里的签名能被程序内置的公钥
(`pkg/catalog/keys.go` 里的 `TrustedKeys`)验过。feed 只是数据:catalog 只"命名"工具、工具链、
包和镜像,不能放命令或下载地址。

## 文件

- `catalog.yaml`:feed 本体,你改这个。
- `catalog.yaml.sig`:它的 ed25519 签名(base64),由工作流生成,不要手改。
- `scripts/sign.sh`、`.github/workflows/publish.yml`:Raccoon CI 仓库 `deploy/catalog-feed/` 里的副本。

## 一次性设置

1. 在你自己的电脑上生成密钥对(私钥不进聊天、不进仓库):
   ```
   openssl genpkey -algorithm ed25519 -out catalog-key.pem
   openssl pkey -in catalog-key.pem -outform DER | tail -c 32 | base64 -w0 > catalog-signing.key
   openssl pkey -in catalog-key.pem -pubout -outform DER | tail -c 32 | base64 -w0   # 公钥
   rm catalog-key.pem
   ```
2. 把 `catalog-signing.key` 的内容放进本仓库的 Actions secret `RACCOONCI_CATALOG_SIGNING_KEY`
   (Settings → Secrets and variables → Actions),然后删掉这个文件。
3. 公钥填进 Raccoon CI 的 `TrustedKeys`,以及工作流里的 `RACCOONCI_CATALOG_PUBLIC_KEY`。

## 发布一次改动

改 `catalog.yaml` 并推到 `main`。工作流会签名并提交 `catalog.yaml.sig`。在这次提交出现之前,
raw 地址上可能短暂地出现"新正文配旧签名";客户端遇到这种情况会拒绝这份 feed,继续用缓存或内置的,
稍后再试。如果分支保护禁止工作流推送,需要给它放行。

## 换密钥

`TrustedKeys` 可以放多把。先把新公钥加进去并发版;客户端更新后,换用新私钥签名(改 secret 和
`RACCOONCI_CATALOG_PUBLIC_KEY`);旧钥不再需要时,从 `TrustedKeys` 里删掉。
