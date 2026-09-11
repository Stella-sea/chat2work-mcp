# chat2work-mcp

`chat2work-mcp` gives a multi-user AI platform a safe, per-user workspace through MCP. It is an independent Go service: the binary and container have no DEEIX runtime dependency, but joint deployment needs network reachability to DEEIX and a shared signing secret. DEEIX is the first identity adapter, not a hard requirement.

## Scope

- Workspace file tools: `fs_list`, `fs_read`, `fs_write`, `fs_edit`, `fs_move`, `fs_delete`, `fs_search`, and `fs_link`.
- OfficeCLI-backed tools: `doc_create`, `doc_edit`, and `doc_query`.
- `workspace_list` to discover the workspaces assigned to the caller.
- Streamable HTTP for DEEIX and stdio for explicit local debugging.
- No shell execution, sandbox, RAG, web search, skill hosting, or plugin marketplace.

`doc_convert` is deliberately not exposed. OfficeCLI 1.0.148 provides `view <file> pdf --out <file>`, but the verified installation returned `exporter_not_found`: no PDF exporter plugin is installed. A PDF engine remains a v2 decision, rather than a hidden fallback.

## Workspace Model

- Identity → default private workspace → file/document tools is the mainline.
- The private workspace directory is named `user-<signed-user-id>` and is always owned by that user.
- Shared workspaces are an optional value-add. Administrators declare them in `config.yaml` with `owner`, `editor`, or `viewer` members. Roles are frozen: no extra role types, no per-workspace custom ACL, no sub-directory authorization.
- Omit `workspace` on any tool to use the private workspace. Pass an assigned workspace id to operate on a shared one. The server always resolves authorization from the signed identity; it never trusts a workspace id from the model beyond that check.

## Security Model

HTTP requests require a static bearer token:

1. `Authorization: Bearer <mcp_token>` must match `MCP_TOKEN`. This also covers `tools/list`, because DEEIX strips the `${DEEIX_SIGNED_USER_CONTEXT}` placeholder when it syncs tools.

Tool calls additionally require a DEEIX user context:

2. `X-Deeix-User-Context` must be a current DEEIX HMAC token. The implementation verifies the documented `v1.<payload>.<signature>` format, HMAC-SHA256 over the base64url payload, nonzero `user_id`, and an unexpired `exp` value.

Every file and Office path passes through one resolver that rejects absolute paths, traversal, and symlinks. Reads and writes in a workspace are serialized with a per-workspace lock. Stdio has no HTTP headers, so it runs only when `stdio_tenant_id` is explicitly configured; do not set it in a production HTTP deployment.

## Artifact Delivery (`fs_link`)

`fs_link` returns a time-limited download URL for a workspace file. The browser cannot send the static bearer token, so the URL carries its own HMAC signature over `workspace + path + exp`. Configure:

```yaml
download_base_url: "https://your-domain.example/chat2work"
download_ttl: "15m"
# download_secret: "..."   # defaults to mcp_user_context_secret
```

Set `download_base_url` to the externally reachable prefix served by your existing reverse proxy. The link only grants read access to that single file until it expires.

This is the current delivery path. Landing artifacts directly in DEEIX's user quota storage requires a DEEIX-side file import endpoint (upstream work), which is intentionally out of scope for now.

## Run With DEEIX

1. Start DEEIX first so the `deeix-chat-network` external network exists.
2. Copy `config.example.yaml` to `config.yaml`. Set a long, random `mcp_token` and set `mcp_user_context_secret` to exactly the same value as DEEIX's `security.mcp_user_context_secret` / `MCP_USER_CONTEXT_SECRET`.
3. Place an audited, version-pinned **Linux** OfficeCLI binary at `bin/officecli`, mark it executable, and keep `OFFICECLI_SKIP_UPDATE=1` enabled. The supplied Compose file mounts it read-only; it does not fetch `latest` during a build. `D:\OfficeCli\officecli.exe` is suitable for Windows M0 testing, not for the Linux container. This repository is validated against `officecli-linux-alpine-x64` v1.0.149 (sha256 `b0129f315d744f1ddd64029b5f5d3244627ac3a9322007b01f6bf273924767d9`); `bin/` is gitignored, so download it during deployment:

```sh
mkdir -p bin
curl -sL -o bin/officecli https://github.com/iOfficeAI/OfficeCLI/releases/download/v1.0.149/officecli-linux-alpine-x64
chmod +x bin/officecli
```
4. From this directory, start the service:

```sh
docker compose -f docker-compose.yml up -d --build
```

5. In DEEIX, add a Streamable HTTP MCP server using URL `http://chat2work-mcp:8090/`. Put the configured `mcp_token` in the admin authentication-key field (DEEIX generates `Authorization: Bearer ...`), and add only this request header:

```text
X-Deeix-User-Context: ${DEEIX_SIGNED_USER_CONTEXT}
```

6. Sync tools and enable the `fs_*`, `doc_*`, and `workspace_list` tools you want.

The service stays on `deeix-chat-network` and publishes only `127.0.0.1:8090` for your reverse proxy. Route `/chat2work/` (or a subdomain) to it and set `download_base_url` accordingly.

## Configuration

Every YAML setting may be overridden by its uppercase environment name: `LISTEN_ADDR`, `MCP_TOKEN`, `MCP_USER_CONTEXT_SECRET`, `WORKSPACE_ROOT`, `MAX_FILE_BYTES`, `MAX_WORKSPACE_BYTES`, `MAX_WORKSPACE_FILES`, `MAX_LIST_RESULTS`, `MAX_SEARCH_RESULTS`, `OFFICECLI_PATH`, `OFFICE_TIMEOUT`, `DELETE_ENABLED`, `AUDIT_LOG_PATH`, `DOWNLOAD_BASE_URL`, `DOWNLOAD_TTL`, `DOWNLOAD_SECRET`, and `STDIO_TENANT_ID`.

`fs_delete` defaults to disabled and never removes directories. `fs_write` uses a temporary file plus rename. `fs_edit` fails unless `old_text` appears exactly once.

Resource controls are enforced per workspace: `max_workspace_bytes` and `max_workspace_files` gate writes, and `max_list_results` / `max_search_results` bound `fs_list` and `fs_search` (the results include a `truncated` flag). When `audit_log_path` is set, every tool call appends one JSON Lines record (user, workspace, tool, path, outcome, duration, result size) with no file content or secrets.

## OfficeCLI M0 Checklist

The Windows M0 validation used OfficeCLI `1.0.148` and established these contracts:

- `create <file>` infers document type from `.docx`, `.xlsx`, or `.pptx`; it has no `--kind` or `--content` parameter.
- `batch <file> --commands <JSON-array>` is the supported bulk-edit invocation. `ops` supplied to the MCP is forwarded as that JSON array.
- Successful `create` and `batch` calls may start a resident process. The server explicitly calls `close` after each, forcing a flush and preventing cross-request resident state.
- DOCX create, paragraph batch edit, close, and `query --json` succeeded. Empty XLSX and PPTX create/close succeeded.
- PDF conversion is unavailable without an OfficeCLI exporter plugin.

Before enabling the Office tools in production, repeat and record these checks against the pinned Linux binary: `create`/`batch --commands`/`close`/`query --json`, `OFFICECLI_SKIP_UPDATE=1`, DOCX/XLSX/PPTX smoke tests, large-XLSX resource measurements, and any PDF exporter plugins.

## Development

```sh
go test ./...
go vet ./...
go run ./cmd/chat2work-mcp -config config.yaml
```

For local stdio debugging, set `stdio_tenant_id` and run `go run ./cmd/chat2work-mcp -config config.yaml --stdio`. This is intentionally separate from DEEIX's signed HTTP flow.

## License

Apache-2.0. See `LICENSE`.
