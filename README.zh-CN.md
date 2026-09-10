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
3. 将经审计、固定版本的 **Linux** OfficeCLI 二进制放到 `bin/officecli`，赋予可执行权限，并保持 `OFFICECLI_SKIP_UPDATE=1`。Compose 会只读挂载它，不会在构建时拉取 `latest`。`D:\OfficeCli\officecli.exe` 仅适合 Windows M0 测试，不能用于 Linux 容器。
4. 在本目录启动服务：

```sh
docker compose -f docker-compose.yml up -d --build
```

5. 在 DEEIX 中添加 Streamable HTTP MCP 服务，URL 为 `http://chat2work-mcp:8090/`。把配置的 `mcp_token` 填入后台的鉴权密钥字段（DEEIX 会生成 `Authorization: Bearer ...`），请求头只需添加：

```text
X-Deeix-User-Context: ${DEEIX_SIGNED_USER_CONTEXT}
```

6. 同步工具，并按需启用 `fs_*`、`doc_*` 和 `workspace_list`。

服务仅加入 `deeix-chat-network`，并只发布 `127.0.0.1:8090` 给反向代理。把 `/chat2work/`（或子域名）转发到它，并相应设置 `download_base_url`。

## 配置项

每个 YAML 配置都可用大写环境变量覆盖：`LISTEN_ADDR`、`MCP_TOKEN`、`MCP_USER_CONTEXT_SECRET`、`WORKSPACE_ROOT`、`MAX_FILE_BYTES`、`MAX_WORKSPACE_BYTES`、`MAX_WORKSPACE_FILES`、`MAX_LIST_RESULTS`、`MAX_SEARCH_RESULTS`、`OFFICECLI_PATH`、`OFFICE_TIMEOUT`、`DELETE_ENABLED`、`AUDIT_LOG_PATH`、`DOWNLOAD_BASE_URL`、`DOWNLOAD_TTL`、`DOWNLOAD_SECRET`、`STDIO_TENANT_ID`。

`fs_delete` 默认禁用，且从不删除目录。`fs_write` 使用临时文件加 rename 原子写入。`fs_edit` 要求 `old_text` 精确匹配且唯一，否则报错。

## OfficeCLI M0 验证

Windows M0 使用 OfficeCLI `1.0.148`，确认了以下契约：

- `create <file>` 由 `.docx`、`.xlsx`、`.pptx` 后缀推断类型，没有 `--kind`、`--content` 参数。
- `batch <file> --commands <JSON 数组>` 是受支持的批量编辑调用；MCP 的 `ops` 就作为该 JSON 数组转发。
- 成功的 `create` 与 `batch` 可能启动 resident 进程。服务端在每次调用后显式 `close`，强制落盘并避免跨请求保留 resident 状态。
- DOCX 创建、段落批处理、close、`query --json` 均成功；空 XLSX 与 PPTX 创建/关闭成功。
- 未安装 OfficeCLI 导出插件时无法转换 PDF。

生产启用 Office 工具前，请针对固定版本的 Linux 二进制重复并记录：`create`/`batch --commands`/`close`/`query --json`、`OFFICECLI_SKIP_UPDATE=1`、DOCX/XLSX/PPTX 冒烟测试、大 XLSX 资源占用，以及 PDF 导出插件情况。

## 开发

```sh
go test ./...
go vet ./...
go run ./cmd/chat2work-mcp -config config.yaml
```

本地 stdio 调试：设置 `stdio_tenant_id`，运行 `go run ./cmd/chat2work-mcp -config config.yaml --stdio`。这与 DEEIX 的签名 HTTP 流程刻意分离。

## 许可证

Apache-2.0，见 `LICENSE`。
