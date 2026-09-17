# Beeper bridge-manager, several bridges in one container

[bridge-manager](https://github.com/beeper/bridge-manager) (`bbctl`) connects self-hosted [mautrix](https://github.com/mautrix) bridges to your Beeper account, and Beeper publishes [a container image](https://github.com/beeper/bridge-manager/tree/main/docker) for it. That image runs **one bridge per container**, because `bbctl run` handles exactly one bridge per process — ten networks means ten containers, ten copies of the same config, ten sets of downloaded binaries.

This image adds `supervisord` on top of Beeper's, so one container runs as many bridges as you list in `$BRIDGES`. Nothing else is changed: every bridge still runs through the base image's own `run-bridge.sh`.

`linux/amd64`.

## Get the image

```bash
docker pull ghcr.io/selfref/beeper-bridge-manager-docker:latest
```

| Tag | Meaning |
|---|---|
| `latest` | moving: newest build |
| `sha-<7>` | the recipe (this repo) that produced the image |
| `base-<digest12>` | which `bridge-manager` base it was built on |
| `<YYYYMMDD>` | scheduled builds |

Labels `dev.selfref.bridgemanager.base` / `.base_digest` record the exact base image.

Rebuilt weekly ([workflow](.github/workflows/build.yml)) to pick up base-image updates, and on every change to the Dockerfile or entrypoint. The bridge **binaries** are not in the image — `bbctl` downloads them at container start — so bridges update on restart without a rebuild.

## Usage

```yaml
services:
  beeper-bridges:
    image: ghcr.io/selfref/beeper-bridge-manager-docker:latest
    restart: unless-stopped
    volumes:
      - ./data:/data
    environment:
      MATRIX_ACCESS_TOKEN: ${MATRIX_ACCESS_TOKEN}
      BRIDGES: "whatsapp,signal,telegram,meta:meta,instagram:instagram"
      DATA_DIR: /data
      DB_DIR: /data/db
    healthcheck:
      test: ["CMD-SHELL", "test \"$$(supervisorctl status | grep -vc RUNNING)\" = 0"]
      interval: 60s
      timeout: 10s
      retries: 3
      start_period: 300s
```

Get `MATRIX_ACCESS_TOKEN` from `~/.config/bbctl/config.json` after `bbctl login`, or from Beeper Desktop → Settings → Help & About.

Bridges are **logged in over Matrix**, not through this container: DM the bridge bot (`@<prefix><name>bot:beeper.local`) from a Beeper client and send `help`, then whatever login command it names (`login`, `login-qr`, `login google`, … differs per network).

Every bridge is an appservice using a websocket to Beeper's server, so no ports are published, but the container does need outbound internet access.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `BRIDGES` | — (**required**) | Comma/space separated `name` or `name:type` entries |
| `MATRIX_ACCESS_TOKEN` | — (**required** on first run) | Beeper access token; `run-bridge.sh` writes it into each `bbctl.json` |
| `BRIDGE_PREFIX` | `sh-` | Prefix for the bbctl bridge name; bbctl wants self-hosted names to start with `sh-` |
| `DATA_DIR` | `/data` | Bridge data root — **must be a volume**, see below |
| `DB_DIR` | `/data/db` | Bridge databases |
| `BEEPER_ENV` | `prod` | Beeper environment |
| `BRIDGE_START_SECS` | `30` | supervisord `startsecs` |
| `BRIDGE_STOP_WAIT_SECS` | `30` | supervisord `stopwaitsecs` |
| `KEEP_DELETED_MESSAGES` | unset | `true`/`1`/`yes`/`on` — keep remotely deleted messages and mark them instead. See below |
| `KEEP_DELETED_MARKER` | `🗑️` | Reaction used as the deletion marker |
| `PATCHED_BRIDGES_DIR` | `/opt/patched-bridges` | Where the patched binaries live in the image |
| `GENERATE_ONLY` | unset | Print the generated supervisord config and exit, without starting anything |

Any other `BEEPER_BRIDGE_*` variable ([bbctl run flags](https://github.com/beeper/bridge-manager/blob/main/cmd/bbctl/run.go)) is inherited by every bridge — set it once on the container. The useful ones:

| Variable | Effect |
|---|---|
| `BEEPER_BRIDGE_NO_OVERRIDE_CONFIG` | Keep hand-edited `config.yaml` files. **bbctl regenerates them on every start otherwise**, so any tuning is lost. Trade-off: it also stops refreshing the appservice/homeserver block, so rotated tokens then need fixing by hand. |
| `BEEPER_BRIDGE_NO_UPDATE` | Do not update the bridge binary on start |
| `BEEPER_BRIDGE_CUSTOM_STARTUP_COMMAND` | Run your own binary or wrapper instead (for a patched bridge) |

`BRIDGES` entries become one supervisord program each:

```
BRIDGES="whatsapp,meta:meta,instagram:instagram"
```

- `whatsapp` → program `whatsapp`, `BRIDGE_NAME=sh-whatsapp`, type guessed by bbctl
- `meta:meta` → type given explicitly

Give the type explicitly where it matters. bbctl otherwise guesses it with a `strings.Contains` over an ordered table, so the result depends on substring order. It also matters where one upstream bridge has several modes: `meta` is Facebook/Messenger and `instagram` is Instagram, both from `mautrix-meta`, and they are separate bbctl bridges with separate registrations and databases.

Names must match `[a-z0-9-]`, and the prefix counts toward bbctl's 32-character limit.

Check what a given `BRIDGES` produces without starting anything:

```bash
docker run --rm -e GENERATE_ONLY=1 -e BRIDGES="whatsapp,meta:meta" \
  ghcr.io/selfref/beeper-bridge-manager-docker:latest
```

## Keeping deleted messages

By default a mautrix bridge **redacts** the Matrix event when the remote
network reports that a message was deleted. A Matrix redaction strips the
content server-side and irreversibly, and the bridge drops its own database
rows too, so the message is gone from your archive as well as from the chat.

This image ships patched bridge binaries that can instead leave the message
intact and add a single 🗑️ reaction to it:

```yaml
environment:
  KEEP_DELETED_MESSAGES: "true"
```

Off by default. With the switch off the image behaves exactly like the
unpatched one, down to using bbctl's own downloaded binaries.

### How it works

`bbctl run` normally downloads a stock binary at container start. When the
switch is on and the image carries a patched binary for a bridge's type, the
entrypoint adds two variables to that program:

```
BEEPER_BRIDGE_CUSTOM_STARTUP_COMMAND=/opt/patched-bridges/mautrix-<type>
BRIDGE_KEEP_DELETED_MESSAGES=true
```

`BEEPER_BRIDGE_CUSTOM_STARTUP_COMMAND` is a stock bbctl flag; it swaps the
executable and disables the update check, which is what stops a downloaded
binary overwriting the patched one. Everything else — registration, config
generation, the websocket transport, the persistence contract — is unchanged.

Bridges with no patched binary in the image are left completely alone and keep
downloading as before. The entrypoint prints how many matched on start:

```
keep-deleted: 8/10 bridge(s) running a patched binary
```

### What the patch changes

Two code sites, because the bridges do not share one deletion path:

| Target | File | Function |
|---|---|---|
| `bridgev2` (every bridge except Discord) | `bridgev2/portal.go` in mautrix-go | `Portal.handleRemoteMessageRemove` |
| Discord (still bridgev1) | `portal.go` in mautrix-discord | `Portal.redactAllParts` |

In both, a guard is inserted before the redaction: if the switch is on, react
to the first part of the message and return, leaving the Matrix events *and*
the bridge's database rows in place so replies and edits still resolve. Only
the first part is marked — one deletion produces one marker, not one per
attachment.

The patcher (`patches/apply.py`) matches its anchors with regexes that tolerate
upstream signature changes, and **fails the build** if an anchor is missing or
ambiguous. That is deliberate: a silently unpatched binary looks identical
until the first deletion.

### Caveats

- Deletions stop propagating out of the bridge, so a message you delete
  elsewhere stays readable in Beeper. That is the point, but it is also a
  privacy decision about other people's messages — make it knowingly.
- The dim, centred "deleted" placeholder is a client-side rendering of a
  redaction. Once you stop redacting you get a reaction chip instead; there is
  no way to get the dim style in a normal chat room.
- These are your own builds of the bridges, from `mautrix/<bridge>` main. They
  update when this image is rebuilt (weekly), not when bbctl next starts.

## The persistence contract

**`/data` must be a real volume, and the entrypoint must stay in charge.**

`run-bridge.sh` in the base image generates each `bbctl.json` with `bridge_data_dir=$DATA_DIR` and `database_dir=$DB_DIR`. It is tempting to skip it and call `bbctl run` directly with a hand-written `bbctl.json` — but bbctl then uses the `bridge_data_dir` from *that* file, ignoring `DATA_DIR`/`DB_DIR` entirely. Point it anywhere outside the volume (its default is `~/.local/share/bbctl/<env>`) and every bridge database, session and downloaded binary lives in the container's writable layer, where it is destroyed on the next recreate — including by any auto-updater pulling a new image. The symptom is bridges that sit at `UNCONFIGURED` forever, because each login is wiped before it can be used.

This image keeps `run-bridge.sh` as each program's command for exactly that reason. After a first run, `/data` should contain:

```
/data/<prefix><name>/config.yaml   per-bridge config (regenerated on start unless NO_OVERRIDE_CONFIG)
/data/db/                          one SQLite database per bridge
/data/binaries/                    downloaded bridge binaries, shared by all programs
/data/bbctl/<name>.json            per-bridge bbctl config
```

`BBCTL_CONFIG` is per bridge on purpose: bbctl re-saves that file to backfill the username after a `whoami`, so a single shared file would be a write race between running bridges.

Note that the bridge databases hold message **ID mappings**, not message content — the messages themselves live in the Matrix rooms on your homeserver. Losing `/data` does not lose messages, but it does lose the mapping, so bridges would create fresh portal rooms and you would end up with duplicates.

## Operating

```bash
docker compose exec beeper-bridges supervisorctl status          # all bridges
docker compose exec beeper-bridges supervisorctl restart <name>  # one bridge
docker compose exec -e BBCTL_CONFIG=/data/bbctl/<name>.json beeper-bridges bbctl whoami
```

Removing a bridge takes two steps in this order, because `bbctl run` re-registers the bridge on **every** start — delete it while its program is running and it comes straight back:

1. Remove it from `BRIDGES` and recreate the container (or `supervisorctl stop <name>`).
2. Delete the registration: `bbctl delete <prefix><name>`.

`bbctl delete` also removes **all of that bridge's rooms on the Beeper servers** — it is not just a local operation. It prompts for confirmation through a TTY and panics without one, so from a non-interactive shell call the same endpoint it calls: `DELETE https://api.beeper.com/bridge/<name>` with `Authorization: Bearer <token>`.

## Build locally

```bash
docker build -t beeper-bridge-manager .
docker build --build-arg BASE_IMAGE=ghcr.io/beeper/bridge-manager@sha256:... -t beeper-bridge-manager .
```

| Arg | Default | Purpose |
|---|---|---|
| `BASE_IMAGE` | `ghcr.io/beeper/bridge-manager:latest` | Base to build on; CI pins it to a digest |
| `PATCH_BRIDGES` | all Go bridges + discord | Which bridges to build patched binaries for |
| `SIGNAL_STAGE` | `signal-build` | `signal-none` skips the mautrix-signal build |
| `GO_IMAGE` / `RUST_IMAGE` | `golang:1-alpine` / `rust:1-alpine` | Toolchains for the builder stages |

Building the bridges is most of the build time. `mautrix-signal` alone pulls in
libsignal, a large Rust dependency with a BoringSSL submodule, and dominates
the wall clock; skip it while iterating:

```bash
docker build --build-arg SIGNAL_STAGE=signal-none \
             --build-arg PATCH_BRIDGES="discord" -t beeper-bridge-manager .
```

## Licence

This repository is **AGPL-3.0**, matching the bridges it patches. That covers
both its own work (Dockerfile, `docker-entrypoint.sh`, `build-bridges.sh`,
`patches/apply.py`, the workflow) and the two patch sources, which are compiled
into upstream code and are modifications of it.

Upstream licences: `bbctl` and the base image are Apache-2.0, `mautrix/go` is
MPL-2.0, and every bridge (`mautrix/whatsapp`, `mautrix/discord`, …) is
AGPL-3.0. MPL-2.0 permits the larger work to be distributed under AGPL-3.0
(MPL section 3.3), and the mautrix-go files `apply.py` edits keep their own MPL
headers, so nothing there needs relicensing.

**The published image contains modified builds of AGPL-3.0 bridges, so
AGPL-3.0 section 13 applies to it**: anyone interacting with those bridges over
a network must be offered the corresponding source. That source is upstream
`mautrix/<bridge>` and `mautrix/go` at the revisions `build-bridges.sh` clones,
plus the modifications in `patches/` — which is the whole of what this image
changes. `build-bridges.sh` is the complete build recipe.
