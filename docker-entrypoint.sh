#!/bin/sh
set -eu

mkdir -p /data/workspace
chown chat2work:chat2work /data/workspace
exec su-exec chat2work /usr/local/bin/chat2work-mcp "$@"
