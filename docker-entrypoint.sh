#!/bin/sh
set -eu

mkdir -p /data/workspace /data/audit
chown -R chat2work:chat2work /data
exec su-exec chat2work /usr/local/bin/chat2work-mcp "$@"
