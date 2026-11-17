# Shared by the build scripts, which source it from the project's folder.

name=discord-live-captions

fail() {
	echo "error: $*" >&2
	exit 1
}

# unix_path turns a Windows path from a Windows program (go, pkg-config) into
# one MSYS2's shell can use. Elsewhere it changes nothing.
unix_path() {
	if command -v cygpath >/dev/null 2>&1; then
		cygpath -u "$1"
	else
		echo "$1"
	fi
}

# prepare sets the version (the first argument, else from git) and empties
# the folder the download is put together in.
prepare() {
	version=${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
	stage=dist/stage/$name
	rm -rf dist/stage
	mkdir -p "$stage/licenses"
	echo "version: $version"
}

# build_program builds the bot into $1 with its version built in, which is
# how it knows whether there's a newer release. Any more arguments are added
# to its -ldflags. nolibopusfile: the opus package's file-reading half
# (opusfile, ogg) isn't used, so it isn't linked and its libraries needn't be
# shipped.
build_program() {
	output=$1
	shift
	CGO_ENABLED=1 go build -trimpath -tags nolibopusfile \
		-ldflags "-s -w -X main.version=$version $*" -o "$output" .
}

# add_docs copies in how to run it, and the licences of everything it's built
# from: Go, every Go module linked into the program, and libdave (with what
# libdave is built from).
add_docs() {
	cp build/HOW-TO-RUN.txt build/THIRD_PARTY_NOTICES.txt "$stage/"
	cp "$(unix_path "$(go env GOROOT)")/LICENSE" "$stage/licenses/go.txt"

	go list -deps -tags nolibopusfile -f '{{with .Module}}{{.Path}} {{.Dir}}{{end}}' . | sort -u |
		while read -r path dir; do
			[ "$path" = livetranscribebot ] && continue
			dir=$(unix_path "$dir")
			for licence in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING*; do
				if [ -f "$licence" ]; then
					cp "$licence" "$stage/licenses/$(echo "$path" | tr / _).txt"
				fi
			done
		done

	dave=$(unix_path "$(pkg-config --variable=prefix dave)")/licenses
	[ -d "$dave" ] || fail "libdave's licences aren't in $dave: install libdave with build/install-libdave.sh"
	mkdir -p "$stage/licenses/libdave"
	cp "$dave"/* "$stage/licenses/libdave/"
}

# make_zip zips everything into dist/$1, inside a discord-live-captions folder
# (updater.zipFolder). It's both what people download and what a running bot
# downloads to update itself. The speech model isn't in it: the bot downloads
# that itself the first time it runs (speechmodel).
make_zip() {
	rm -f "dist/$1"
	(cd dist/stage && zip -qr "../$1" "$name")
	ls -lh "dist/$1"
}
