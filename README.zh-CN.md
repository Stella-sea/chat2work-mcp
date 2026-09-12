# chat2work-mcp

`chat2work-mcp` 通过 MCP 为多用户 AI 平台提供安全的、按用户隔离的工作区。它是一个独立的 Go 服务：二进制与容器不依赖 DEEIX 运行时，但联合部署需要与 DEEIX 网络可达并共享签名密钥。DEEIX 是首个身份适配器，而非硬性前提。

## 功能边界

- 工作区文件工具：`fs_list`、`fs_read`、`fs_write`、`fs_edit`、`fs_move`、`fs_delete`、`fs_search`、`fs_link`。
- OfficeCLI 文档工具：`doc_create`、`doc_edit`、`doc_query`。
- `workspace_list`：查看管理员分配给当前用户的工作区。
- DEEIX 走 Streamable HTTP，stdio 仅供显式本地调试。
- 不做 shell 执行、沙箱、RAG、web search、skill 托管、插件市场。

`doc_convert` 暂不提供。OfficeCLI 1.0.148 有 `view <file> pdf --out <file>`，但实测该安装返回 `exporter_not_found`：未安装 PDF 导出插件。PDF 引擎列入 v2 决策，不做隐式兜底。

## 工作区模型

- 主线是：身份 → 默认私有工作区 → 文件/文档工具。
- 私有工作区目录名为 `user-<签名 user_id>`，恒归该用户所有。
- 共享工作区是可选增值能力。管理员在 `config.yaml` 声明，成员角色为 `owner`、`editor`、`viewer`。角色已冻结：不增加角色类型、不做 per-workspace 自定义 ACL、不做子目录授权。
- 任意工具不传 `workspace` 即操作私有工作区；传入已分配的工作区 id 才操作共享空间。服务端始终从签名身份解析授权，绝不信任模型传来的工作区 id 本身。

## 安全模型

HTTP 请求需要静态 Bearer：

1. `Authorization: Bearer <mcp_token>` 必须等于 `MCP_TOKEN`。这也覆盖 `tools/list`，因为 DEEIX 同步工具时会剥掉 `${DEEIX_SIGNED_USER_CONTEXT}` 占位符。

工具调用还需要 DEEIX 用户上下文：

2. `X-Deeix-User-Context` 必须是当前有效的 DEEIX HMAC token。实现会校验 `v1.<payload>.<signature>` 格式、对 base64url payload 的 HMAC-SHA256、非零 `user_id`、以及未过期的 `exp`。

所有文件与 Office 路径都经过同一个解析入口，拒绝绝对路径、路径穿越和 symlink。同一工作区的读写由工作区级锁串行化。stdio 没有 HTTP 头，因此仅在显式配置 `stdio_tenant_id` 时运行；生产 HTTP 部署不要设置它。

## 产物交付（`fs_link`）

`fs_link` 返回工作区文件的限时下载链接。浏览器无法携带静态 Bearer，因此链接自带对 `workspace + path + exp` 的 HMAC 签名。配置项：

```yaml
download_base_url: "https://your-domain.example/chat2work"
download_ttl: "15m"
# download_secret: "..."   # 省略时默认使用 mcp_user_context_secret
```

把 `download_base_url` 设为你的外层反向代理对外可达的前缀。链接仅授予该单个文件在过期前的读权限。

这是当前交付路径。要让产物直接进入 DEEIX 的用户配额存储区，需要 DEEIX 侧提供一个文件导入接口（上游改动），目前有意不纳入范围。

## 与 DEEIX 联合部署

1. 先启动 DEEIX，以创建外部网络 `deeix-chat-network`。
2. 复制 `config.example.yaml` 为 `config.yaml`。设置一个长随机 `mcp_token`，并把 `mcp_user_context_secret` 设为与 DEEIX 的 `security.mcp_user_context_secret` / `MCP_USER_CONTEXT_SECRET` 完全相同的值。
3. 构建并启动服务。OfficeCLI 以固定版本内置于镜像，构建时用 SHA256 校验，无需手动放置二进制。

```sh
docker compose -f docker-compose.yml up -d --build
```

`docker compose up` 会自动按宿主机架构构建。ARM64 或多架构镜像请用 `buildx`（见下文多架构构建）。

4. 在 DEEIX 中添加 Streamable HTTP MCP 服务，URL 为 `http://chat2work-mcp:8090/`。把配置的 `mcp_token` 填入后台的鉴权密钥字段（DEEIX 会生成 `Authorization: Bearer ...`），请求头只需添加：

```text
X-Deeix-User-Context: ${DEEIX_SIGNED_USER_CONTEXT}
```

5. 同步工具，并按需启用 `fs_*`、`doc_*` 和 `workspace_list`。

服务仅加入 `deeix-chat-network`，并只发布 `127.0.0.1:8090` 给反向代理。把 `/chat2work/`（或子域名）转发到它，并相应设置 `download_base_url`。

## 配置项

每个 YAML 配置都可用大写环境变量覆盖：`LISTEN_ADDR`、`MCP_TOKEN`、`MCP_USER_CONTEXT_SECRET`、`WORKSPACE_ROOT`、`MAX_FILE_BYTES`、`MAX_WORKSPACE_BYTES`、`MAX_WORKSPACE_FILES`、`MAX_LIST_RESULTS`、`MAX_SEARCH_RESULTS`、`OFFICECLI_PATH`、`OFFICE_TIMEOUT`、`MAX_OFFICE_CONCURRENCY`、`MAX_UNCOMPRESSED_BYTES`、`DELETE_ENABLED`、`AUDIT_LOG_PATH`、`AUDIT_MAX_BYTES`、`AUDIT_MAX_BACKUPS`、`DOWNLOAD_BASE_URL`、`DOWNLOAD_TTL`、`DOWNLOAD_SECRET`、`STDIO_TENANT_ID`。

`GET /healthz` 是无需鉴权的存活探针；`GET /readyz` 仅在 OfficeCLI 可解析且工作区根目录可写时返回 200，否则 503。两者都可用于容器编排的健康检查。

`fs_delete` 默认禁用，且从不删除目录。`fs_write` 使用临时文件加 rename 原子写入。`fs_edit` 要求 `old_text` 精确匹配且唯一，否则报错。

资源控制按工作区生效：`max_workspace_bytes` 与 `max_workspace_files` 约束写入，`max_list_results` 与 `max_search_results` 约束 `fs_list` 和 `fs_search`（结果含 `truncated` 标记）。设置 `audit_log_path` 后，每次工具调用追加一条 JSON Lines 记录（用户、工作区、工具、路径、结果、耗时、结果大小），不含文件内容或密钥；日志在 `audit_max_bytes` 时轮转，保留 `audit_max_backups` 个文件。

Office 文档在交给 OfficeCLI 前会做大小检查（`max_file_bytes`）与解压膨胀检查（`max_uncompressed_bytes`）。会修改文档的 Office 操作先作用于同目录临时文件，仅在成功后重命名覆盖目标，因此失败或超时的编辑不会破坏原文档。

## OfficeCLI：内置、固定版本、多架构

OfficeCLI 内置在运行镜像中，不再挂载。Dockerfile 会按构建目标下载对应 release 资产、用固定 SHA256 校验、赋予可执行权限，并在每次调用时设置 `OFFICECLI_SKIP_UPDATE=1` 与 `OFFICECLI_NO_AUTO_RESIDENT=1`。

`create` 默认会启动后台 resident 进程，等于每个文档泄漏一个进程。`OFFICECLI_NO_AUTO_RESIDENT=1` 关闭该行为，使每次调用都是自包含的 open/save/exit，无跨请求状态。`max_office_concurrency` 信号量（默认 4）限制同时运行的 OfficeCLI 进程数。

实测二进制带来的两个打包要点：

- 它是自包含的 .NET 单文件程序，但**并非静态链接**。运行镜像必须包含 `libstdc++`（提供 `libgcc_s`）与 `icu-libs`。缺少时会分别报 `libstdc++.so.6 not found` 与 `Could not find a valid ICU package`。
- 它是真正的二进制，不是安装器，下载后无需再执行安装步骤。

`v1.0.149` 固定资产（Alpine / musl）：

| 架构 | 资产 | SHA256 |
|---|---|---|
| amd64 | `officecli-linux-alpine-x64` | `b0129f315d744f1ddd64029b5f5d3244627ac3a9322007b01f6bf273924767d9` |
| arm64 | `officecli-linux-alpine-arm64` | `ecd319f19e0beb524a3a0961d85b552b332dc1836592b4f1bcbaca61b86f05fd` |

Dockerfile 依据 BuildKit 的 `TARGETARCH` 选择资产，因此在 ARM64 宿主机上普通 `docker build` 即可产出 ARM64 镜像。发布多架构镜像：

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/chat2work-mcp:<tag> --push .
```

### 版本策略：固定，不自动更新

固定 OfficeCLI 版本。在常驻服务里自动更新文档引擎是供应链与可复现性风险：同一镜像可能随时间产出不同 OOXML，坏版本会静默影响所有租户。更新应是显式动作：改 Dockerfile 里的 `OFFICECLI_VERSION` 与两个校验值，重新构建，重跑冒烟测试。可用定时 CI 在出现新版本时自动开 PR，但已部署镜像保持固定直到评审通过。

## OfficeCLI 验证记录

在 `alpine:3.22` + `libstdc++` + `icu-libs` 中，针对 `officecli-linux-alpine-x64` v1.0.149 验证：

- `create <file>` 由 `.docx`、`.xlsx`、`.pptx` 后缀推断类型，没有 `--kind`、`--content` 参数。
- `batch <file> --commands <JSON 数组>` 是受支持的批量编辑调用；MCP 的 `ops` 就作为该 JSON 数组转发。
- 设置 `OFFICECLI_NO_AUTO_RESIDENT=1` 后，`create`、`batch`、`query` 均不残留后台进程；每次调用后进程数归零，文件在退出时落盘。
- `.docx`、`.xlsx`、`.pptx` 的 `create`/`close` 均成功；`--locale zh-CN` 创建成功；DOCX `query --json` 成功。
- 通过构建出的镜像端到端验证：签名 `fs_write`、`fs_list`、`doc_create`、`fs_link` 与签名下载均成功。
- 未安装 OfficeCLI 导出插件时无法转换 PDF。

生产启用 Office 工具前，请针对固定版本的二进制重复并记录：`create`/`batch --commands`/`close`/`query --json`、`OFFICECLI_SKIP_UPDATE=1`、DOCX/XLSX/PPTX 冒烟测试、大 XLSX 资源占用，以及 PDF 导出插件情况。

## CI 与发布

`.github/workflows/ci.yml` 在每次 push 与 PR 时执行 `go vet`、`go test -race`、`go build` 与 Docker 镜像构建。`.github/workflows/release.yml` 在推送 `v*` tag 时构建并推送多架构（`linux/amd64`、`linux/arm64`）镜像到 GHCR。OfficeCLI 更新按设计保持手动（见上文版本策略）；后续可加一个定时工作流在出现新版本时自动开升级 PR。

## 开发

```sh
go test ./...
go vet ./...
go run ./cmd/chat2work-mcp -config config.yaml
```

本地 stdio 调试：设置 `stdio_tenant_id`，运行 `go run ./cmd/chat2work-mcp -config config.yaml --stdio`。这与 DEEIX 的签名 HTTP 流程刻意分离。

## 许可证

Apache-2.0，见 `LICENSE`。
