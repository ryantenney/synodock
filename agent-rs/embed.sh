#!/bin/sh
# Builds synodock-agent (with gzip) for both arches and puts it in internal/agentbin,
# where synodock embeds it to send with each exec, with the hash of the sources it was
# built from (internal/agentbin's test fails when they've changed since).
#
# Author: Ryan Tenney
set -eu
cd "$(dirname "$0")/.."
ARCHES="amd64 arm64" agent-rs/build.sh
for arch in amd64 arm64; do
	cp "build/synodock-agent-rs-$arch" "internal/agentbin/synodock-agent-$arch"
done
agent-rs/sources.sh > internal/agentbin/sources.sha256
cat internal/agentbin/sources.sha256
