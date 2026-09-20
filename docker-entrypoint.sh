#!/bin/sh
# Generate a supervisord config from $BRIDGES, then run it.
#
# `bbctl run` handles exactly ONE bridge per process, so running several
# bridges in one container needs a supervisor with one program per bridge.
# The list is a runtime input rather than baked into the image, so the image
# stays generic (and carries no information about whose bridges these are).
#
# BRIDGES is a comma/space separated list of "name" or "name:type":
#   BRIDGES="whatsapp,signal,meta:meta,instagram:instagram"
# Each entry becomes a program running the base image's own run-bridge.sh with
#   BRIDGE_NAME=${BRIDGE_PREFIX}<name>   (default prefix "sh-", required by bbctl)
#   BEEPER_BRIDGE_TYPE=<type>            (only when given)
#   BBCTL_CONFIG=${DATA_DIR}/bbctl/<name>.json
#
# Why <type> is worth setting explicitly: bbctl otherwise guesses it with a
# strings.Contains over an ordered table, so the guess depends on substring
# order. Passing it removes the ambiguity.
#
# Why BBCTL_CONFIG is per bridge: bbctl re-saves that file to backfill the
# username after a whoami, so one shared file would be a write race between
# the running bridges.
#
# Everything else (MATRIX_ACCESS_TOKEN, BEEPER_ENV, DATA_DIR, DB_DIR, and any
# BEEPER_BRIDGE_* flags such as BEEPER_BRIDGE_NO_OVERRIDE_CONFIG) is inherited
# from the container environment by every program, so it is set once on the
# container rather than repeated per bridge.
#
# Patched bridges: when a patch is switched on AND the image carries a patched
# binary for a bridge's type (/opt/patched-bridges/mautrix-<type>), that program
# additionally gets
#   BEEPER_BRIDGE_CUSTOM_STARTUP_COMMAND=<that binary>
# so bbctl runs our build instead of downloading the stock one. That flag also
# turns off bbctl's update check, which is what keeps the patched binary in
# place. Bridges with no patched binary are left completely untouched, and with
# every switch off the image behaves exactly like the unpatched one.
#
# Two independent patches ask for the patched binary:
#   KEEP_DELETED_MESSAGES  a remote deletion marks instead of redacting
#   WATCH_URL              every bridged event is reported to beeper-watch
#
# KEEP_DELETED_MESSAGES chooses WHOSE deleted messages are kept, by the sender
# of the message rather than by who issued the deletion:
#   off (default)  stock upstream: every deletion redacts
#   all            keep everything
#   self           keep only your own messages; other people's still redact
#   other          keep only other people's messages; your own still redact
# The old booleans still work: true/1/yes/on mean all, false/0/no mean off.
#
# Two independent markers on a kept message:
#   KEEP_DELETED_MARKER   reaction on it. UNSET means the bridge default (a
#                         wastebasket); set-but-EMPTY means no reaction at all
#   KEEP_DELETED_NOTICE   bool: one bridge-bot notice replying to it, saying
#                         who deleted it and when. UNSET follows
#                         KEEP_DELETED_MESSAGES — on when deletions are kept
# KEEP_DELETED_MARKER is exported to supervisord rather than written into an
# `environment=` line, because supervisord runs %(...)s expansion over that
# field and this one holds arbitrary operator text — a literal % would break
# the config file.
#
# WATCH_URL turns on the beeper-watch hook: the bridge POSTs every event it
# handles — messages, edits, reactions, deletions, both directions — to that
# URL in plaintext, before encryption, so something outside the bridge can
# react to it. Beeper exposes no events at all, which is why this exists.
#   WATCH_URL           where to POST; empty disables the hook entirely
#   WATCH_TOKEN         bearer token for that endpoint
#   WATCH_OUTGOING      false to report only what arrives from the network
#   WATCH_RAW_CONTENT   true to include each event's full content
#   WATCH_MAX_BODY      truncate message bodies to this many bytes
#   WATCH_QUEUE / WATCH_WORKERS / WATCH_TIMEOUT / WATCH_ATTEMPTS  delivery
# There is deliberately no per-bridge switch: which networks matter is a rule
# in beeper-watch, not ten environment variables here. These are exported
# globally for the same %-expansion reason as the marker above.
set -eu

# /etc/supervisord.conf is supervisorctl's default search path, so writing it
# there keeps `supervisorctl status` working with no arguments — which the
# documented healthcheck and every operator reflex rely on.
CONF=${SUPERVISORD_CONF:-/etc/supervisord.conf}
BRIDGE_PREFIX=${BRIDGE_PREFIX:-sh-}
DATA_DIR=${DATA_DIR:-/data}
START_SECS=${BRIDGE_START_SECS:-30}
STOP_WAIT_SECS=${BRIDGE_STOP_WAIT_SECS:-30}
PATCHED_DIR=${PATCHED_BRIDGES_DIR:-/opt/patched-bridges}
KEEP_DELETED=$(printf '%s' "${KEEP_DELETED_MESSAGES:-}" | tr '[:upper:]' '[:lower:]')

case "$KEEP_DELETED" in
	"" | off | 0 | false | no) KEEP_DELETED= ;;
	all | 1 | true | yes | on) KEEP_DELETED=all ;;
	self | mine) KEEP_DELETED=self ;;
	other | others | theirs) KEEP_DELETED=other ;;
	*)
		# Erring towards keeping content: this switch exists to prevent
		# irreversible redactions, so a typo must not cause any.
		echo "keep-deleted: WARNING - unknown KEEP_DELETED_MESSAGES '${KEEP_DELETED_MESSAGES}', using 'all'" >&2
		KEEP_DELETED=all
		;;
esac

# Marker: distinguish UNSET (use the bridge's default) from SET-BUT-EMPTY (no
# reaction). ${VAR+x} is the only portable way to tell those apart.
MARKER_IS_SET=${KEEP_DELETED_MARKER+set}

if [ -n "$KEEP_DELETED" ] && [ -n "$MARKER_IS_SET" ]; then
	export BRIDGE_KEEP_DELETED_MARKER="${KEEP_DELETED_MARKER}"
fi

# The notice is a bool whose default is derived inside the bridge, so it is
# exported whenever it is set, independently of the mode — which is also where
# it has no effect, since nothing is kept to reply to.
NOTICE=$(printf '%s' "${KEEP_DELETED_NOTICE:-}" | tr '[:upper:]' '[:lower:]')
case "$NOTICE" in
	"") ;;
	1 | true | yes | on) NOTICE=true ;;
	0 | false | no | off) NOTICE=false ;;
	*)
		echo "keep-deleted: WARNING - unknown KEEP_DELETED_NOTICE '${KEEP_DELETED_NOTICE}', following KEEP_DELETED_MESSAGES" >&2
		NOTICE=
		;;
esac
if [ -n "$NOTICE" ]; then
	export BRIDGE_KEEP_DELETED_NOTICE="$NOTICE"
fi

WATCH=${WATCH_URL:-}
if [ -n "$WATCH" ]; then
	export BRIDGE_WATCH_URL="$WATCH"
	for var in TOKEN OUTGOING RAW_CONTENT MAX_BODY QUEUE WORKERS TIMEOUT ATTEMPTS NETWORK; do
		# eval rather than ${!var}: this runs under ash, not bash.
		eval "value=\${WATCH_${var}:-}"
		[ -n "$value" ] && export "BRIDGE_WATCH_${var}=$value"
	done
fi

# Either patch is a reason to run our own binary instead of the stock one.
USE_PATCHED=
[ -n "$KEEP_DELETED" ] && USE_PATCHED=1
[ -n "$WATCH" ] && USE_PATCHED=1

if [ -z "${BRIDGES:-}" ]; then
	echo "BRIDGES is not set — nothing to run." >&2
	echo 'Example: BRIDGES="whatsapp,signal,telegram"' >&2
	exit 1
fi

cat > "$CONF" <<CONFEOF
; GENERATED by docker-entrypoint.sh from \$BRIDGES — edits here are lost on
; restart. Change BRIDGES (or BRIDGE_PREFIX) on the container instead.
[supervisord]
nodaemon=true
logfile=/dev/null
logfile_maxbytes=0
pidfile=/tmp/supervisord.pid

[unix_http_server]
file=/tmp/supervisor.sock

[rpcinterface:supervisor]
supervisor.rpcinterface_factory = supervisor.rpcinterface:make_main_rpcinterface

[supervisorctl]
serverurl=unix:///tmp/supervisor.sock
CONFEOF

count=0
patched=0
for entry in $(echo "$BRIDGES" | tr ',' ' '); do
	name=${entry%%:*}
	type=${entry#"$name"}
	type=${type#:}

	# bbctl requires 1-32 chars of [a-z0-9-]; the prefix counts toward that.
	case "$name" in
		*[!a-z0-9-]* | "")
			echo "Invalid bridge name '$name' (allowed: a-z, 0-9, -)" >&2; exit 1 ;;
	esac

	{
		echo
		echo "[program:${name}]"
		# mkdir here (not at image build) because \$DATA_DIR is a volume.
		echo "command=/bin/sh -c 'mkdir -p ${DATA_DIR}/bbctl && exec /usr/local/bin/run-bridge.sh'"
		printf 'environment=BRIDGE_NAME="%s%s"' "$BRIDGE_PREFIX" "$name"
		if [ -n "$type" ]; then
			printf ',BEEPER_BRIDGE_TYPE="%s"' "$type"
		fi
		printf ',BBCTL_CONFIG="%s/bbctl/%s.json"' "$DATA_DIR" "$name"
		# bbctl names bridge binaries mautrix-<type>; fall back to the bridge
		# name when no type was given (bbctl would then guess the same way).
		binary="${PATCHED_DIR}/mautrix-${type:-$name}"
		if [ -n "$USE_PATCHED" ] && [ -x "$binary" ]; then
			printf ',BEEPER_BRIDGE_CUSTOM_STARTUP_COMMAND="%s"' "$binary"
			# Per program rather than exported, because this one is an enum
			# from a fixed set and can never contain a literal %.
			if [ -n "$KEEP_DELETED" ]; then
				printf ',BRIDGE_KEEP_DELETED_MESSAGES="%s"' "$KEEP_DELETED"
			fi
			patched=$((patched + 1))
		fi
		printf '\n'
		echo "autostart=true"
		echo "autorestart=true"
		echo "startsecs=${START_SECS}"
		echo "startretries=1000"
		# run-bridge.sh does not exec, so the shell stays the parent of bbctl,
		# which in turn spawns the bridge binary. Without stop/killasgroup a
		# stop would signal only the shell and leak the bridge process.
		echo "stopasgroup=true"
		echo "killasgroup=true"
		echo "stopwaitsecs=${STOP_WAIT_SECS}"
		echo "stdout_logfile=/dev/fd/1"
		echo "stdout_logfile_maxbytes=0"
		echo "redirect_stderr=true"
	} >> "$CONF"
	count=$((count + 1))
done

echo "Configured ${count} bridge(s) from \$BRIDGES" >&2
if [ -n "$USE_PATCHED" ]; then
	echo "patched binaries: ${patched}/${count} bridge(s)" >&2
	if [ "$patched" -eq 0 ]; then
		echo "WARNING - no patched binary matched any bridge type in ${PATCHED_DIR}" >&2
	fi
fi
if [ -n "$WATCH" ]; then
	echo "beeper-watch: reporting events to ${WATCH}" >&2
	if [ -z "${WATCH_TOKEN:-}" ]; then
		echo "beeper-watch: WARNING - no WATCH_TOKEN set, events are sent unauthenticated" >&2
	fi
fi
if [ -n "$KEEP_DELETED" ]; then
	echo "keep-deleted: mode ${KEEP_DELETED}" >&2
	if [ -n "$MARKER_IS_SET" ] && [ -z "${KEEP_DELETED_MARKER}" ]; then
		echo "keep-deleted: marker disabled (KEEP_DELETED_MARKER is set but empty)" >&2
	else
		echo "keep-deleted: marker ${BRIDGE_KEEP_DELETED_MARKER:-(bridge default)}" >&2
	fi
	case "$NOTICE" in
		false) echo "keep-deleted: notice off" >&2 ;;
		"") echo "keep-deleted: notice on (following KEEP_DELETED_MESSAGES)" >&2 ;;
		*) echo "keep-deleted: notice on" >&2 ;;
	esac
fi

# Print the generated config and exit — for testing, without starting anything.
if [ -n "${GENERATE_ONLY:-}" ]; then
	cat "$CONF"
	exit 0
fi

exec /usr/bin/supervisord -c "$CONF"
