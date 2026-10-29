#!/bin/sh
# The hash of synodock-agent's sources, as internal/agentbin's test computes it: each
# file's path and a newline, then its contents, in this order.
#
# Author: Ryan Tenney
set -eu
cd "$(dirname "$0")/.."
export LC_ALL=C
for f in agent-rs/Cargo.toml agent-rs/Cargo.lock agent-rs/.cargo/config.toml agent-rs/src/*.rs; do
	printf '%s\n' "$f"
	cat "$f"
done | shasum -a 256 | cut -d' ' -f1
