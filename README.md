# chat2work-mcp

`chat2work-mcp` gives a multi-user AI platform a safe, per-user workspace through MCP. It is an independent Go service: DEEIX is the first identity adapter, not a runtime dependency.

## Scope

- Per-tenant workspace file tools: `fs_list`, `fs_read`, `fs_write`, `fs_edit`, `fs_move`, `fs_delete`, and `fs_search`.
- OfficeCLI-backed tools: `doc_create`, `doc_edit`, and `doc_query`.
- Streamable HTTP for DEEIX and stdio for explicit local debugging.
- No shell execution, sandbox, RAG, web search, skill hosting, or plugin marketplace.

`doc_convert` is deliberately not exposed. OfficeCLI 1.0.148 provides `view <file> pdf --out <file>`, but the verified installation returned `exporter_not_found`: no PDF exporter plugin is installed. A PDF engine remains a v2 decision, rather than a hidden fallback.

## Security Model

HTTP calls require both controls:

1. `Authorization: Bearer <mcp_token>` must match `MCP_TOKEN`.
2. `X-Deeix-User-Context` must be a current DEEIX HMAC token. The implementation verifies the documented `v1.<payload>.<signature>` format, HMAC-SHA256 over the base64url payload, nonzero `user_id`, and an unexpired `exp` value.

The authenticated user ID is the tenant directory name. Every file and Office path passes through one resolver that rejects absolute paths, traversal, and symlinks. Stdio has no HTTP headers, so it runs only when `stdio_tenant_id` is explicitly configured.

## Run With DEEIX

1. Copy `config.example.yaml` to `config.yaml`. Set a long, random `mcp_token` and set `mcp_user_context_secret` to exactly the same value as DEEIX's `security.mcp_user_context_secret` / `MCP_USER_CONTEXT_SECRET`.
2. Place an audited, version-pinned **Linux** OfficeCLI binary at `bin/officecli`, mark it executable, and keep `OFFICECLI_SKIP_UPDATE=1` enabled. The supplied Compose file mounts it read-only; it intentionally does not fetch `latest` during a build. `D:\OfficeCli\officecli.exe` is suitable for Windows M0 testing, not for the Linux container.
3. Start alongside the DEEIX Compose stack:

```sh
docker compose -f docker-compose.yml up -d --build
```

4. In DEEIX, add a Streamable HTTP MCP server using URL `http://chat2work-mcp:8090/` and configure these request headers:

```text
Authorization: Bearer <the configured mcp_token>
X-Deeix-User-Context: ${DEEIX_SIGNED_USER_CONTEXT}
```

The service publishes no host port and only attaches to `deeix-chat-network`.

## Configuration

Every YAML setting may be overridden by its uppercase environment name: `LISTEN_ADDR`, `MCP_TOKEN`, `MCP_USER_CONTEXT_SECRET`, `WORKSPACE_ROOT`, `MAX_FILE_BYTES`, `OFFICECLI_PATH`, `OFFICE_TIMEOUT`, `DELETE_ENABLED`, and `STDIO_TENANT_ID`.

`fs_delete` defaults to disabled and never removes directories. `fs_write` uses a temporary file plus rename. `fs_edit` fails unless `old_text` appears exactly once.

## OfficeCLI M0 Checklist

The Windows M0 validation used OfficeCLI `1.0.148` and established these contracts:

- `create <file>` infers document type from `.docx`, `.xlsx`, or `.pptx`; it has no `--kind` or `--content` parameter.
- `batch <file> --commands <JSON-array>` is the supported bulk-edit invocation. `ops` supplied to the MCP is forwarded as that JSON array.
- Successful `create` and `batch` calls may start a resident process. The server explicitly calls `close` after each, forcing a flush and preventing cross-request resident state.
- DOCX create, paragraph batch edit, close, and `query --json` succeeded. Empty XLSX and PPTX create/close succeeded.
- PDF conversion is unavailable without an OfficeCLI exporter plugin.

Before enabling the Office tools in production, repeat and record these checks against the pinned Linux binary:

- `create`, `batch --commands`, `close`, and `query --json` syntax and exit-code behavior.
- `OFFICECLI_SKIP_UPDATE=1` behavior inside the production image.
- DOCX, XLSX, and PPTX create/edit/query smoke tests.
- Large XLSX memory/time measurements at the configured 20 MiB file limit.
- Installed PDF exporter plugins and their resource usage, if PDF conversion is later proposed.

## Development

```sh
go test ./...
go vet ./...
go run ./cmd/chat2work-mcp -config config.yaml
```

For local stdio debugging, set `stdio_tenant_id` and run `go run ./cmd/chat2work-mcp -config config.yaml --stdio`. This is intentionally separate from DEEIX's signed HTTP flow.

## License

Apache-2.0. See `LICENSE`.
