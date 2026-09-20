# Self-hosted Beeper bridges: the official bridge-manager image, plus a
# supervisor so that ONE container can run several bridges, plus patched
# bridge binaries that keep remotely-deleted messages.
#
# Why a supervisor at all: `bbctl run` handles exactly ONE bridge per process,
# so N bridges normally means N containers. Each supervisord program still
# execs the base image's own /usr/local/bin/run-bridge.sh, which is the
# important part — see "The persistence contract" in README.md. In short:
# run-bridge.sh generates each bbctl.json with bridge_data_dir=$DATA_DIR, and
# calling `bbctl run` directly instead (with a hand-written bbctl.json) leaves
# every bridge database, session and downloaded binary in the container's
# writable layer, where a recreate destroys them.
#
# Why bridge binaries are in the image now: bbctl downloads stock binaries at
# start, which is fine until you want a behaviour change. The keep-deleted
# patch (see patches/) has to be compiled in, so the patched binaries ship in
# /opt/patched-bridges and the entrypoint points bbctl at them with
# BEEPER_BRIDGE_CUSTOM_STARTUP_COMMAND. That flag also disables bbctl's update
# check, which is what stops a stock binary overwriting the patched one.
# Bridges WITHOUT a patched binary keep the original download-at-start path, so
# this stays a superset of the old behaviour.
#
# The bridge list is a runtime input ($BRIDGES), not baked in, so the image is
# still generic — /opt/patched-bridges is just a menu the entrypoint consults.

ARG BASE_IMAGE=ghcr.io/beeper/bridge-manager:latest
ARG GO_IMAGE=golang:1-alpine
ARG RUST_IMAGE=rust:1-alpine
# Set to signal-none to skip the (slow, Rust) mautrix-signal build.
ARG SIGNAL_STAGE=signal-build


# --- bridgev2 bridges + Discord -----------------------------------------
# All pure Go, built static (CGO_ENABLED=0) so they run on the Alpine base.
FROM ${GO_IMAGE} AS bridges
RUN apk add --no-cache git python3 build-base olm-dev
WORKDIR /build
COPY patches /patches
COPY build-bridges.sh /usr/local/bin/build-bridges.sh
ARG PATCH_BRIDGES="telegram whatsapp gmessages meta twitter bluesky linkedin discord"
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    PATCH_BRIDGES="${PATCH_BRIDGES}" /usr/local/bin/build-bridges.sh /out


# --- mautrix-signal: libsignal (Rust) ------------------------------------
# Mirrors upstream's own Dockerfile. This is by far the most expensive stage:
# libsignal is a large Rust build pulled in as a git submodule.
FROM ${RUST_IMAGE} AS signal-rust
RUN apk add --no-cache git make cmake protoc musl-dev g++ clang-dev protobuf-dev
WORKDIR /build
RUN git clone --depth 1 --recurse-submodules --shallow-submodules \
        https://github.com/mautrix/signal .
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    ./build-rust.sh


# --- mautrix-signal: the Go half -----------------------------------------
# Not CGO_ENABLED=0: it links libsignal_ffi.a, so it stays a dynamic musl
# binary, exactly like the stock one bbctl downloads.
FROM ${GO_IMAGE} AS signal-build
RUN apk add --no-cache git python3 build-base olm-dev zlib-dev
WORKDIR /build
COPY patches /patches
RUN git clone --depth 1 https://github.com/mautrix/signal .
COPY --from=signal-rust /build/pkg/libsignalgo/libsignal/target/release/libsignal_ffi.a ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod <<SH
set -eu
ver=$(awk '/maunium\.net\/go\/mautrix v/ {print $2; exit}' go.mod)
[ -n "$ver" ] || { echo "FATAL: no mautrix-go requirement in signal/go.mod" >&2; exit 1; }
echo "mautrix-go $ver"
# Same tag-or-pseudo-version dance as prepare_mautrix_go in build-bridges.sh:
# a pseudo-version (vX.Y.Z-0.<ts>-<commit>) is not a ref, and GitHub refuses a
# fetch for the 12-character commit prefix it carries, so fall back to a
# blobless clone and let git expand the abbreviation locally.
case "$ver" in
*-*-*)
	mkdir -p /mautrix-go
	git -C /mautrix-go init -q
	git -C /mautrix-go remote add origin https://github.com/mautrix/go
	if ! git -C /mautrix-go fetch -q --depth 1 origin "${ver##*-}" 2>/dev/null; then
		git -C /mautrix-go fetch -q --filter=blob:none origin
	fi
	git -C /mautrix-go checkout -q "${ver##*-}"
	;;
*)
	git clone -q --depth 1 -b "$ver" https://github.com/mautrix/go /mautrix-go
	;;
esac
python3 /patches/apply.py bridgev2 /mautrix-go
go mod edit -replace "maunium.net/go/mautrix=/mautrix-go"
go mod tidy
mkdir -p /out
# Upstream's own Go build (maubuild), same as its Dockerfile runs.
LIBRARY_PATH=. ./build-go.sh
mv mautrix-signal /out/mautrix-signal
SH

# Escape hatch: `--build-arg SIGNAL_STAGE=signal-none` skips the Rust build.
FROM alpine:3.23 AS signal-none
RUN mkdir -p /out

FROM ${SIGNAL_STAGE} AS signal-final


# --- runtime --------------------------------------------------------------
FROM ${BASE_IMAGE}

# Alpine base (bridge-manager builds on alpine:3.23). tzdata is here for the
# keep-deleted notice: it stamps a local time, and without the zone database
# Go's time.Local is UTC no matter what TZ says, so the notice would claim the
# wrong hour for anyone not on UTC.
RUN apk add --no-cache supervisor tzdata

COPY --from=bridges /out/ /opt/patched-bridges/
COPY --from=signal-final /out/ /opt/patched-bridges/
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

VOLUME /data
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
