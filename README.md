# chat2work-mcp

`chat2work-mcp` gives a multi-user AI platform a safe, per-user workspace through MCP. It is an independent Go service: the binary and container have no DEEIX runtime dependency, but joint deployment needs network reachability to DEEIX and a shared signing secret. DEEIX is the first identity adapter, not a hard requirement.

## Scope

- Workspace file tools: `fs_list`, `fs_read`, `fs_write`, `fs_edit`, `fs_move`, `fs_delete`, `fs_search`, and `fs_link`.
- OfficeCLI-backed tools: `doc_create`, `doc_edit`, `doc_query`, and `sheet_set_cells`.
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
3. Build and start the service. OfficeCLI is bundled into the image at a pinned version and verified by SHA256 during the build; you do not place a binary by hand.

```sh
docker compose -f docker-compose.yml up -d --build
```

`docker compose up` builds for the host architecture automatically. For ARM64 or a multi-arch registry image, use `buildx` (see Multi-arch builds below).

4. In DEEIX, add a Streamable HTTP MCP server using URL `http://chat2work-mcp:8090/`. Put the configured `mcp_token` in the admin authentication-key field (DEEIX generates `Authorization: Bearer ...`), and add only this request header:

```text
X-Deeix-User-Context: ${DEEIX_SIGNED_USER_CONTEXT}
```

5. Sync tools and enable the `fs_*`, `doc_*`, and `workspace_list` tools you want.

The service stays on `deeix-chat-network` and publishes only `127.0.0.1:8090` for your reverse proxy. Route `/chat2work/` (or a subdomain) to it and set `download_base_url` accordingly.

## Configuration

Every YAML setting may be overridden by its uppercase environment name: `LISTEN_ADDR`, `LOG_LEVEL`, `MCP_TOKEN`, `MCP_USER_CONTEXT_SECRET`, `WORKSPACE_ROOT`, `MAX_FILE_BYTES`, `MAX_WORKSPACE_BYTES`, `MAX_WORKSPACE_FILES`, `MAX_LIST_RESULTS`, `MAX_SEARCH_RESULTS`, `OFFICECLI_PATH`, `OFFICE_TIMEOUT`, `MAX_OFFICE_CONCURRENCY`, `MAX_UNCOMPRESSED_BYTES`, `DELETE_ENABLED`, `AUDIT_LOG_PATH`, `AUDIT_MAX_BYTES`, `AUDIT_MAX_BACKUPS`, `DOWNLOAD_BASE_URL`, `DOWNLOAD_TTL`, `DOWNLOAD_SECRET`, `REQUEST_ID_CACHE_TTL`, `REQUEST_ID_CACHE_ENTRIES`, and `STDIO_TENANT_ID`.

`GET /healthz` is an unauthenticated liveness probe. `GET /readyz` returns 200 only when OfficeCLI is resolvable and the workspace root is writable, otherwise 503. Both are safe for container orchestration health checks.

`fs_delete` defaults to disabled and never removes directories. `fs_write` uses a temporary file plus rename. `fs_edit` fails unless `old_text` appears exactly once.

Resource controls are enforced per workspace: `max_workspace_bytes` and `max_workspace_files` gate writes, and `max_list_results` / `max_search_results` bound `fs_list` and `fs_search` (the results include a `truncated` flag). When `audit_log_path` is set, every tool call appends one JSON Lines record (user, workspace, tool, path, outcome, duration, result size) with no file content or secrets; the file rotates at `audit_max_bytes` and keeps `audit_max_backups` files.

Logs are structured JSON on stderr (stdout stays reserved for the stdio transport), controlled by `log_level`. Each HTTP request and tool call is logged with a `request_id`, taken from the `X-Request-Id` header or the DEEIX signed context when present, and generated otherwise. Logs never contain file contents, tokens, or signed contexts.

DEEIX retries can safely repeat a mutating call when its signed context includes a `request_id`. For `fs_write`, `fs_edit`, `fs_move`, `fs_delete`, `doc_create`, `doc_edit`, and `sheet_set_cells`, the service deduplicates calls by authenticated user, request id, tool, and normalized parameters. It caches completed MCP results for `request_id_cache_ttl` (default `10m`) and bounds the cache to `request_id_cache_entries` (default `1000`). Reads are never cached, calls without a signed request id run normally, and audit/log records mark replays with `deduplicated: true`.

Office documents are size-checked against `max_file_bytes` and decompression-checked against `max_uncompressed_bytes` before they are handed to OfficeCLI. Mutating Office operations run against a temporary sibling file and are renamed over the target only on success, so a failed or timed-out edit leaves the original document untouched.

## OfficeCLI: bundled, pinned, and multi-arch

OfficeCLI is baked into the runtime image, not mounted. The Dockerfile downloads the matching release asset for the build target, verifies it against a pinned SHA256, and marks it executable. `OFFICECLI_SKIP_UPDATE=1` and `OFFICECLI_NO_AUTO_RESIDENT=1` are set on every invocation.

`create` auto-starts a background resident process by default, which would leak a process per document. `OFFICECLI_NO_AUTO_RESIDENT=1` disables that, so every call is a self-contained open/save/exit with no cross-request state. A `max_office_concurrency` semaphore (default 4) bounds how many OfficeCLI processes run at once.

`sheet_set_cells` converts a same-worksheet batch of up to 1000 A1 cells into OfficeCLI `set` commands. A `value` beginning with `=` is an Excel formula; optional `props` apply limited OfficeCLI cell formatting. It never spans worksheets or workbooks.

Two facts learned from the real binary matter for packaging:

- It is a self-contained .NET single-file executable, but **not statically linked**. The runtime image must ship `libstdc++` (which provides `libgcc_s`) and `icu-libs`. Without them it aborts with `libstdc++.so.6 not found` / `Could not find a valid ICU package`.
- It is a real binary, not an installer. No post-download install step is needed.

Pinned assets for `v1.0.149` (Alpine, musl):

| Arch | Asset | SHA256 |
|---|---|---|
| amd64 | `officecli-linux-alpine-x64` | `b0129f315d744f1ddd64029b5f5d3244627ac3a9322007b01f6bf273924767d9` |
| arm64 | `officecli-linux-alpine-arm64` | `ecd319f19e0beb524a3a0961d85b552b332dc1836592b4f1bcbaca61b86f05fd` |

The Dockerfile selects the asset from the BuildKit `TARGETARCH`, so a normal `docker build` on an ARM64 host produces an ARM64 image. To publish a multi-arch registry image:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/chat2work-mcp:<tag> --push .
```

### Version policy: pin, do not auto-update

Pin the OfficeCLI version. Auto-updating a document engine inside a long-running service is a supply-chain and reproducibility risk: the same image could start producing different OOXML over time, and a bad release would silently affect every tenant. Updating is a deliberate act: bump `OFFICECLI_VERSION` and the two checksums in the Dockerfile, rebuild, and re-run the smoke tests. This can be automated by a scheduled CI job that opens a pull request when a new release appears, while the deployed image stays pinned until reviewed.

## OfficeCLI validation record

Validated against `officecli-linux-alpine-x64` v1.0.149 inside `alpine:3.22` with `libstdc++` and `icu-libs`:

- `create <file>` infers document type from `.docx`, `.xlsx`, or `.pptx`; it has no `--kind` or `--content` parameter.
- `batch <file> --commands <JSON-array>` is the supported bulk-edit invocation. `ops` supplied to the MCP is forwarded as that JSON array.
- With `OFFICECLI_NO_AUTO_RESIDENT=1`, `create`, `batch`, and `query` leave no background process; process count returns to zero after each call and the file is flushed on exit.
- `create`/`close` succeeded for `.docx`, `.xlsx`, and `.pptx`; `--locale zh-CN` creation succeeded; DOCX `query --json` succeeded.
- End-to-end through the built image: signed `fs_write`, `fs_list`, `doc_create`, `fs_link`, and a signed download all succeeded.
- PDF conversion is unavailable without an OfficeCLI exporter plugin.

Before enabling the Office tools in production, repeat and record these checks against the pinned binary: `create`/`batch --commands`/`close`/`query --json`, `OFFICECLI_SKIP_UPDATE=1`, DOCX/XLSX/PPTX smoke tests, large-XLSX resource measurements, and any PDF exporter plugins.

## CI and releases

`.github/workflows/ci.yml` runs `go vet`, `go test -race`, `go build`, and a Docker image build on every push and pull request. `.github/workflows/release.yml` builds and pushes a multi-arch (`linux/amd64`, `linux/arm64`) image to GHCR when a `v*` tag is pushed. OfficeCLI updates stay manual by design (see the version policy above); a scheduled workflow that opens a bump PR is a reasonable future addition.

## Development

```sh
go test ./...
go vet ./...
go run ./cmd/chat2work-mcp -config config.yaml
```

For local stdio debugging, set `stdio_tenant_id` and run `go run ./cmd/chat2work-mcp -config config.yaml --stdio`. This is intentionally separate from DEEIX's signed HTTP flow.

## License

Apache-2.0. See `LICENSE`.
