#!/bin/sh
# Installs Discord's prebuilt libdave (end-to-end encrypted voice) into a
# folder, with its licences, and a dave.pc so the build finds it.
#
#   build/install-libdave.sh <folder>
#   then: PKG_CONFIG_PATH=<folder>/lib/pkgconfig
#
# The version is the one godave pins (its release.txt).
set -eu

version=v1.1.0
prefix=${1:?usage: build/install-libdave.sh <folder>}

case "$(uname -s)" in
MINGW* | MSYS*) platform=Windows ;;
*) platform=Linux ;;
esac

mkdir -p "$prefix/lib/pkgconfig"
zip="$prefix/libdave.zip"
curl -fsSL --retry 3 -o "$zip" \
	"https://github.com/discord/libdave/releases/download/$version/cpp/libdave-$platform-X64-boringssl.zip"
unzip -qo "$zip" -d "$prefix"
rm "$zip"

if [ "$platform" = Windows ]; then
	# gcc and pkg-config on Windows want C:/style paths. MinGW links straight
	# against the DLL, as sherpa-onnx's Go module does with its own.
	pc_prefix=$(cygpath -m "$(cd "$prefix" && pwd)")
	libs='-L${prefix}/bin -ldave'
else
	pc_prefix=$(cd "$prefix" && pwd)
	# So tests can run straight from the build. The download's program is
	# changed to look next to itself instead (package-linux.sh).
	libs='-L${prefix}/lib -ldave -Wl,-rpath,${prefix}/lib'
fi

cat >"$prefix/lib/pkgconfig/dave.pc" <<PC
prefix=$pc_prefix

Name: dave
Description: Discord Audio & Video End-to-End Encryption (DAVE) Protocol
Version: $version
Libs: $libs
Cflags: -I\${prefix}/include/dave
PC
echo "libdave $version installed in $prefix"
