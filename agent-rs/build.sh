#!/bin/sh
# Builds the Rust synodock-agent, static with musl, in a rust:alpine container for each
# arch: build/synodock-agent-rs-<arch>, with gzip output (miniz_oxide); GZIP=0 leaves it
# out (build/synodock-agent-rs-nogzip-<arch>: smaller, never compresses).
#
# Author: Ryan Tenney
set -eu
cd "$(dirname "$0")/.."
mkdir -p build
name=synodock-agent-rs
features=
if [ "${GZIP:-1}" = 0 ]; then
	name=synodock-agent-rs-nogzip
	features=--no-default-features
fi
image=${RUST_IMAGE:-rust:1.92-alpine3.22}
for arch in ${ARCHES:-amd64 arm64}; do
	case $arch in
	amd64) target=x86_64-unknown-linux-musl ;;
	arm64) target=aarch64-unknown-linux-musl ;;
	*) echo "build.sh: unknown arch $arch" >&2; exit 2 ;;
	esac
	# The crate registry is a volume, so builds after the first don't download again.
	docker run --rm --platform "linux/$arch" -v "$PWD/agent-rs:/src:ro" -v "$PWD/build:/out" \
		-v "synodock-cargo-$arch:/usr/local/cargo/registry" -e CARGO_TARGET_DIR=/tmp/target "$image" sh -c "
		cd /src &&
		cargo build -q --locked --release --target $target $features &&
		cp /tmp/target/$target/release/synodock-agent /out/$name-$arch"
	ls -l "build/$name-$arch"
done
