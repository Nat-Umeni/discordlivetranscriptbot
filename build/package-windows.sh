#!/bin/sh
# Builds the Windows download, dist/hopamine-captions-windows-x64.zip: the
# program, the DLLs it needs and the docs. Running bots update from it too.
#
#   build/package-windows.sh [version]
#
# Runs in MSYS2's MINGW64 shell, with Go, gcc, pkg-config, opus, libdave
# (build/install-libdave.sh) and zip.
set -eu
cd "$(dirname "$0")/.."
. build/common.sh

prepare "${1:-}"
build_program "$stage/$name.exe"

# Windows looks for DLLs next to the program first, so they go there.
sherpa=$(unix_path "$(go list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-windows)")/lib/x86_64-pc-windows-gnu
cp "$sherpa/sherpa-onnx-c-api.dll" "$sherpa/onnxruntime.dll" "$stage/"
cp "$(unix_path "$(pkg-config --variable=prefix dave)")/bin/libdave.dll" "$stage/"

# Then the ones from MSYS2: opus, and gcc's runtime. ldd lists every DLL the
# program loads (finding the ones above next to it), each with its licence.
ldd "$stage/$name.exe" | awk '{ print $3 }' | grep -i "^$MINGW_PREFIX/bin/" | sort -u |
	while read -r dll; do
		cp "$dll" "$stage/"
		package=$(pacman -Qqo "$dll")
		short=${package#"$MINGW_PACKAGE_PREFIX-"}
		if [ -d "$MINGW_PREFIX/share/licenses/$short" ]; then
			cp -r "$MINGW_PREFIX/share/licenses/$short" "$stage/licenses/$short"
		fi
	done
[ -f "$stage/libopus-0.dll" ] || fail "libopus-0.dll wasn't found next to the program"

add_docs

make_zip hopamine-captions-windows-x64.zip
