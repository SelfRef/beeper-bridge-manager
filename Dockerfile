# Self-hosted Beeper bridges: the official bridge-manager image, plus a
# supervisor so that ONE container can run several bridges.
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
# The bridge list is a runtime input ($BRIDGES), not baked in, so this image is
# generic. The base image already carries ffmpeg, lottieconverter and the
# Python dependencies the Python-based bridges need; nothing else is added.
ARG BASE_IMAGE=ghcr.io/beeper/bridge-manager:latest
FROM ${BASE_IMAGE}

# Alpine base (bridge-manager builds on alpine:3.23).
RUN apk add --no-cache supervisor

COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

VOLUME /data
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
