FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/chat2work-mcp ./cmd/chat2work-mcp

FROM alpine:3.22
# OfficeCLI is a self-contained .NET single-file app. It is not statically
# linked, so the runtime needs libstdc++ (libgcc_s + libstdc++) and ICU.
ARG TARGETARCH
ARG OFFICECLI_VERSION=1.0.149
RUN apk add --no-cache su-exec libstdc++ icu-libs \
 && addgroup -S chat2work && adduser -S -G chat2work chat2work \
 && case "${TARGETARCH}" in \
      amd64) asset=officecli-linux-alpine-x64;  sha=b0129f315d744f1ddd64029b5f5d3244627ac3a9322007b01f6bf273924767d9 ;; \
      arm64) asset=officecli-linux-alpine-arm64; sha=ecd319f19e0beb524a3a0961d85b552b332dc1836592b4f1bcbaca61b86f05fd ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && wget -qO /usr/local/bin/officecli "https://github.com/iOfficeAI/OfficeCLI/releases/download/v${OFFICECLI_VERSION}/${asset}" \
 && echo "${sha}  /usr/local/bin/officecli" | sha256sum -c - \
 && chmod +x /usr/local/bin/officecli
COPY scripts/officecli-xlsx-smoke.sh /tmp/officecli-xlsx-smoke.sh
RUN apk add --no-cache --virtual .officecli-smoke-deps jq \
 && chmod +x /tmp/officecli-xlsx-smoke.sh \
 && /tmp/officecli-xlsx-smoke.sh \
 && rm /tmp/officecli-xlsx-smoke.sh \
 && apk del .officecli-smoke-deps
COPY --from=build /out/chat2work-mcp /usr/local/bin/chat2work-mcp
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["-config", "/etc/chat2work/config.yaml"]
