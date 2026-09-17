#!/bin/sh
# Build patched mautrix bridge binaries.
#
# bbctl normally downloads a stock binary per bridge at container start. This
# script builds the same bridges from source with patches/apply.py applied, so
# `bbctl run` can be pointed at them with BEEPER_BRIDGE_CUSTOM_STARTUP_COMMAND
# (which also disables bbctl's update check, so the patched binary survives).
#
# Upstream ships main builds — the stock binaries report pseudo-versions of
# mautrix/<bridge> main — so main is what we build too. The mautrix-go version
# is NOT chosen here: it is read from each bridge's own go.mod and cloned at
# exactly that version, then patched. Several bridges share a version, so the
# clones are cached per version.
#
# Usage: build-bridges.sh <output-dir>
set -eu

OUT=${1:?usage: build-bridges.sh <output-dir>}
PATCHES=${PATCHES:-/patches}
WORK=${WORK:-/build}
MXGO_CACHE="$WORK/mautrix-go"

# Bridge repos to build. Binaries are named mautrix-<name>, which is what bbctl
# expects. `instagram` is a second binary out of the meta repo, not a repo.
BRIDGES=${PATCH_BRIDGES:-telegram whatsapp gmessages meta twitter bluesky linkedin}

mkdir -p "$OUT" "$WORK" "$MXGO_CACHE"

# Clone mautrix-go at the version a bridge's go.mod asks for, patch it once,
# and echo the path. Tagged versions clone by tag; pseudo-versions
# (vX.Y.Z-0.<timestamp>-<commit>) are fetched by commit.
prepare_mautrix_go() {
	ver=$1
	dest="$MXGO_CACHE/$ver"
	if [ -d "$dest" ]; then
		echo "$dest"
		return
	fi
	case "$ver" in
	*-*-*)
		commit=${ver##*-}
		mkdir -p "$dest"
		git -C "$dest" init -q
		git -C "$dest" fetch -q --depth 1 https://github.com/mautrix/go "$commit"
		git -C "$dest" checkout -q FETCH_HEAD
		;;
	*)
		git clone -q --depth 1 -b "$ver" https://github.com/mautrix/go "$dest"
		;;
	esac
	python3 "$PATCHES/apply.py" bridgev2 "$dest" >&2
	echo "$dest"
}

build_bridgev2() {
	name=$1
	src="$WORK/src/$name"
	echo "=== $name ==="
	rm -rf "$src"
	git clone -q --depth 1 "https://github.com/mautrix/$name" "$src"

	ver=$(awk '/maunium\.net\/go\/mautrix v/ {print $2; exit}' "$src/go.mod")
	[ -n "$ver" ] || { echo "FATAL: no mautrix-go requirement in $name/go.mod" >&2; exit 1; }
	echo "  mautrix-go $ver"
	mxgo=$(prepare_mautrix_go "$ver")

	cd "$src"
	go mod edit -replace "maunium.net/go/mautrix=$mxgo"
	go mod tidy
	run_repo_build "$name"
	collect
	cd - >/dev/null
}

# Use each repo's own build script rather than a hand-rolled `go build`.
# They call `go tool maubuild`, which sets the tags and ldflags that produce
# the static binary bbctl would otherwise download; plain `go build` with
# CGO_ENABLED=0 fails because mxmain imports mattn/go-sqlite3 unconditionally.
run_repo_build() {
	case "$1" in
	meta)
		# One repo, two binaries: Messenger and Instagram are separate bbctl
		# bridges and bbctl looks for mautrix-meta and mautrix-instagram.
		scripts="./build-fb.sh ./build-ig.sh"
		;;
	*)
		scripts="./build.sh"
		;;
	esac
	for sc in $scripts; do
		[ -f "$sc" ] || { echo "FATAL: $sc missing in $1 — upstream changed its build" >&2; exit 1; }
		echo "  running $sc"
		sh "$sc"
	done
}

# Build scripts drop their output in the repo root, named after the binary.
collect() {
	found=0
	for f in mautrix-*; do
		[ -f "$f" ] && [ -x "$f" ] || continue
		mv "$f" "$OUT/$f"
		echo "  -> $OUT/$f"
		found=$((found + 1))
	done
	[ "$found" -gt 0 ] || { echo "FATAL: build produced no mautrix-* binary in $PWD" >&2; exit 1; }
}

# Discord is still bridgev1: the deletion path lives in its own repo, so it
# gets its own patch and needs no mautrix-go replacement at all.
build_discord() {
	src="$WORK/src/discord"
	echo "=== discord (bridgev1) ==="
	rm -rf "$src"
	git clone -q --depth 1 https://github.com/mautrix/discord "$src"
	python3 "$PATCHES/apply.py" discord "$src"
	cd "$src"
	go mod tidy
	run_repo_build discord
	collect
	cd - >/dev/null
}

for b in $BRIDGES; do
	case "$b" in
	discord) build_discord ;;
	signal) echo "skip: signal is built in its own stage (needs Rust)" ;;
	*) build_bridgev2 "$b" ;;
	esac
done

echo "=== built ==="
ls -la "$OUT"
