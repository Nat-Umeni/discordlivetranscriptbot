#!/bin/sh
# Builds the Linux download, dist/discord-live-captions-linux-x64.zip: the
# program, the libraries it needs and the docs. Running bots update from it
# too.
#
#   build/package-linux.sh [version]
#
# Needs Go, gcc, pkg-config, opus, libdave (build/install-libdave.sh) and
# zip; patchelf too for a release.
set -eu
cd "$(dirname "$0")/.."
. build/common.sh

prepare "${1:-}"
# The libraries go next to the program, so it looks for them in its own
# folder ($ORIGIN) too.
build_program "$stage/$name" '-extldflags=-Wl,-rpath,$ORIGIN'

# The libraries it needs that a Linux PC might not have go next to it. ldd
# says where this machine has each one.
for lib in libdave.so libopus.so.0 libsherpa-onnx-c-api.so libonnxruntime.so; do
	path=$(ldd "$stage/$name" | awk -v lib="$lib" '$1 == lib { print $3 }')
	[ -f "$path" ] || fail "can't find $lib, which the program needs"
	cp -L "$path" "$stage/"
	chmod 644 "$stage/$lib"
done

# The build also left this machine's library folders in the program's search
# path. They're harmless elsewhere (they don't exist there, so it moves on to
# its own folder), but patchelf makes its own folder the only place it looks.
if command -v patchelf >/dev/null 2>&1; then
	patchelf --set-rpath '$ORIGIN' "$stage/$name"
else
	echo "warning: patchelf isn't installed, so this machine's library folders stay in the program's search path" >&2
fi

add_docs
for licence in /usr/share/licenses/opus/COPYING /usr/share/doc/libopus0/copyright; do
	if [ -f "$licence" ]; then
		cp "$licence" "$stage/licenses/opus.txt"
	fi
done
[ -f "$stage/licenses/opus.txt" ] || fail "opus's licence wasn't found"

make_zip discord-live-captions-linux-x64.zip
