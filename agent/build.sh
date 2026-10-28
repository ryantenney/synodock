#!/bin/sh
# Builds the C synodock-agent, static with musl, in an Alpine container for each arch:
# build/synodock-agent-c-<arch>, with zlib (gzip output); ZLIB=0 leaves it out
# (build/synodock-agent-c-nozlib-<arch>: smaller, never compresses).
#
# Author: Ryan Tenney
set -eu
cd "$(dirname "$0")/.."
mkdir -p build
name=synodock-agent-c
lib=-lz
pkgs="build-base zlib-dev zlib-static"
if [ "${ZLIB:-1}" = 0 ]; then
	name=synodock-agent-c-nozlib
	lib=-DNO_ZLIB
	pkgs=build-base
fi
for arch in ${ARCHES:-amd64 arm64}; do
	# 4 KiB pages: arm64's default 64 KiB alignment pads the binary by about 40 KB.
	docker run --rm --platform "linux/$arch" -v "$PWD/agent:/src:ro" -v "$PWD/build:/out" alpine:3.22 sh -c "
		apk add -q $pkgs &&
		gcc -std=c11 -D_GNU_SOURCE -Os -Wall -Wextra -Werror -static -s \
			-ffunction-sections -fdata-sections -Wl,--gc-sections -Wl,-z,max-page-size=4096 \
			-o /out/$name-$arch /src/agent.c $lib"
	ls -l "build/$name-$arch"
done
