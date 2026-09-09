FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/chat2work-mcp ./cmd/chat2work-mcp

FROM alpine:3.22
RUN addgroup -S chat2work && adduser -S -G chat2work chat2work && apk add --no-cache su-exec
COPY --from=build /out/chat2work-mcp /usr/local/bin/chat2work-mcp
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["-config", "/etc/chat2work/config.yaml"]
