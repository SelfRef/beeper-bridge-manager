# patches

Source modifications compiled into the bridge binaries this image ships.
Applied by `apply.py`, driven by `../build-bridges.sh`.

Two patches, both inert until their environment variables are set, so a binary
built here behaves exactly like the stock one until it is configured:

| Patch | Switch | What it does |
|---|---|---|
| [keep-deleted](#keep-deleted) | `BRIDGE_KEEP_DELETED_MESSAGES` | a remote deletion marks the message instead of redacting it |
| [beeper-watch](#beeper-watch) | `BRIDGE_WATCH_URL` | every bridged event is reported, in plaintext, to an HTTP endpoint |

## keep-deleted

Stops a remote deletion from redacting the Matrix event. Instead the message is
left intact and marked — with a reaction, a one-line notice from the bridge bot,
or both — for every deletion, or only for one side of the conversation.

Runtime switch, read once at bridge start:

| Variable | Default | Effect |
|---|---|---|
| `BRIDGE_KEEP_DELETED_MESSAGES` | `off` | `off` / `all` / `self` / `other` — which side's deleted messages are kept. `true` and `false` still mean `all` and `off`; an unknown value warns and keeps everything |
| `BRIDGE_KEEP_DELETED_MARKER` | `🗑️` | Reaction key. Read with `LookupEnv`: **unset** = default, **set-but-empty** = no reaction |
| `BRIDGE_KEEP_DELETED_NOTICE` | follows `_MESSAGES` | Bool. One `m.notice` from the BRIDGE BOT replying to the kept message: `🗑️ <sender> deleted this message at <YYYY-MM-DD HH:MM>`. Unset = on whenever deletions are kept at all; an unparseable value warns and does the same |

The side is decided by who SENT the message, not by who issued the deletion —
the choice is whose content you keep. The two only differ when someone else can
delete your message (a group admin), which counts as your side.

`self` and `other` need to know whether a message is yours. bridgev2 takes the
first of three signals that is populated, because which of them are filled in
depends on the connector and on double puppeting: `Message.IsDoublePuppeted`,
`Message.SenderMXID == UserLogin.UserMXID`, and finally
`NetworkAPI.IsThisUser` (part of the interface, so every bridge has it, and
checked last because it is the only one that can touch the client). Discord
compares `Message.SenderID` against the portal receiver and the bridge's user
table (`GetUserByID`, a lookup that returns nil rather than inserting a row).

The notice sender is the bridge bot deliberately: Beeper renders an `m.notice`
from the bot as dim centred text with no bubble in any room, so the marker reads
as bridge bookkeeping rather than as something a participant said. The name in
it is the message's sender (falling back to `Someone` for an unsynced ghost),
looked up without inserting a row — `Bridge.GetGhostByID` on bridgev2,
`DB.Puppet.Get` rather than `GetPuppetByID` on Discord. The body carries no
`formatted_body`, so a display name containing HTML needs no escaping.

The container-level knobs drop the `BRIDGE_` prefix; the entrypoint translates
them. The master switch is set per program, and only for bridges that actually
have a patched binary; the marker and notice values are exported globally
instead, because supervisord expands `%(...)s` in `environment=` lines.

### Files

| File | Goes to | Package |
|---|---|---|
| `bridgev2_keep_deleted.go` | `bridgev2/zz_keep_deleted.go` in mautrix-go | `bridgev2` |
| `discord_keep_deleted.go` | `zz_keep_deleted.go` in mautrix-discord | `main` |
| `apply.py` | — | inserts the guard at the call site |

Two targets because the bridges do not share a deletion path: everything except
Discord goes through bridgev2 in mautrix-go, while Discord is still bridgev1
and has its own.

### Anchors

`apply.py` matches these and **fails loudly** if a match is missing or not
unique. It is deliberately not a context diff: mautrix-go v0.28 and v0.31 have
different signatures for both calls, and several bridges pin different
versions.

| Target | Anchor |
|---|---|
| bridgev2 | `res := portal.redactMessageParts(ctx, targetParts, intent, getEventTS(evt)…` |
| discord | `existing := portal.bridge.DB.Message.GetByDiscordID(portal.Key, msgID)` |

Re-running is safe: an insert whose marker is already present is skipped, and
a rename whose new name is already in the file is skipped.

### Why the call site and not `redactMessageParts`

`redactMessageParts` has a second caller (`portalinternal.go`) used for
Matrix-side redactions, which should keep working normally. Only the remote
deletion path is changed.

### Why the database rows are kept

Upstream calls `DB.Message.DeleteAllParts` right after redacting. The patch
returns before that, so replies, edits and reactions that point at the message
still resolve afterwards.

## beeper-watch

Reports every event the bridge handles to an HTTP endpoint, in plaintext, as
it happens.

This exists because Beeper has no event API at all — no webhooks, no gateway,
and the Desktop API is request/response only — while the portals a bridge
creates are end-to-end encrypted. A Matrix client watching `/sync` can see that
a message happened and who sent it, but not what it said. The bridge can: it
holds the plaintext right up until it encrypts it.

Runtime switch, read once at bridge start:

| Variable | Default | Effect |
|---|---|---|
| `BRIDGE_WATCH_URL` | unset | where to POST. **Empty disables everything**: the hooks cost one nil check |
| `BRIDGE_WATCH_TOKEN` | unset | sent as `Authorization: Bearer` |
| `BRIDGE_WATCH_NETWORK` | `$BRIDGE_NAME` minus `sh-` | the network label in the payload |
| `BRIDGE_WATCH_BRIDGE` | `$BRIDGE_NAME` | the bridge label in the payload |
| `BRIDGE_WATCH_OUTGOING` | `true` | `false` reports only what arrives from the network |
| `BRIDGE_WATCH_RAW_CONTENT` | `false` | `true` includes each event's full content |
| `BRIDGE_WATCH_MAX_BODY` | `8192` | truncate bodies to this many bytes |
| `BRIDGE_WATCH_QUEUE` | `256` | queued events before dropping |
| `BRIDGE_WATCH_WORKERS` | `2` | concurrent POSTs |
| `BRIDGE_WATCH_TIMEOUT` | `5` | seconds per POST |
| `BRIDGE_WATCH_ATTEMPTS` | `3` | POST attempts per event |

`BRIDGE_NAME` is what bbctl was invoked with, and the bridge binary inherits
it, so neither label needs configuring per bridge.

### What is reported

Messages, edits, reactions, unreactions, deletions and stickers, in both
directions (`in` = from the remote network, `out` = sent by the local user from
a Beeper client). Bridge bookkeeping is skipped: `com.beeper.message_send_status`,
the portal-creation dummy event, and anything already encrypted.

The payload is flat, one JSON object per event, with the fields a filter needs
— network, bridge, direction, kind, room, sender (MXID and the remote network's
own ID), timestamp, msgtype, body, reaction key, redaction target, reply and
thread IDs, and a media descriptor. `BRIDGE_WATCH_RAW_CONTENT=true` adds the
whole content on top.

### Where it hooks

| Target | Direction | Function |
|---|---|---|
| bridgev2 | in | `matrix.ASIntent.SendMessage` |
| bridgev2 | out | `Portal.handleMatrixEvent` |
| discord | in | `Portal.sendMatrixMessage`, `Portal.redactAllParts`, `Portal.handleDiscordReaction` |
| discord | out | `Portal.handleMatrixMessages` |

`ASIntent.SendMessage` is the whole reason this is only one hook for nine
bridges: every bridged event goes through it, and the encryption step is inside
it, so reading the content on the way in gets the plaintext. Discord is still
bridgev1 and shares none of that code, so it needs four.

### Wrapping, not injecting

Each hook **renames** the upstream function (suffix `Unhooked`) and adds a
wrapper under the original name in the helper file. Callers are untouched, and
the wrapper reports around the call — before it, because the content is
encrypted in place and a later read would find ciphertext, and after it,
because the event ID only exists once the send succeeded.

The other reason is maintenance: the wrapper has to repeat the signature, so
if upstream changes it the build fails with a compiler error naming the exact
function, instead of a regex quietly missing and shipping a binary that never
reports anything.

### Interaction with keep-deleted

With keep-deleted on, a remote deletion is *sent* as a reaction. Without help
the watcher could not tell that apart from someone reacting with the same
emoji, so the keep-deleted helper tags the content with
`dev.aperte.beeper_watch_kind: "deletion"`. The reporter reads that tag and
removes it before the event is sent, so it never reaches the homeserver, and
the event is reported as a deletion rather than as a reaction.

### Not blocking the bridge

Delivery is a bounded queue drained by background workers. A full queue drops
the event with a warning, a failed POST is retried a couple of times and then
dropped, and nothing here can return an error into the bridge's event handling.
Durability belongs on the other side: beeper-watch writes a match to disk
before it sends anything.

### Files

| File | Goes to | Package |
|---|---|---|
| `watch_client.go` | `bridgev2/zz_beeper_watch_client.go` / `zz_beeper_watch_client.go` | rewritten on install |
| `bridgev2_watch_hooks.go` | `bridgev2/zz_beeper_watch.go` | `bridgev2` |
| `bridgev2_matrix_watch.go` | `bridgev2/matrix/zz_beeper_watch.go` | `matrix` |
| `discord_watch_hooks.go` | `zz_beeper_watch.go` in mautrix-discord | `main` |

`watch_client.go` is the same file in both families — the queue, the HTTP
client and the payload extraction are identical — so it ships with
`package __WATCH_PACKAGE__` and `apply.py` rewrites the clause on install.
That is the one piece of magic here, and it exists so the two copies cannot
drift. The hooks differ per family and are separate files.

The bridgev2 client lives in `bridgev2` rather than in `matrix` because both
hooks need it and `matrix` already imports `bridgev2`; the reverse would be an
import cycle.

## Updating for a new upstream

If a build fails with `FATAL: expected exactly 1 … anchor`, upstream moved the
code. If it fails with a Go compiler error inside one of the `zz_` files, a
signature changed — which is the failure mode the wrappers are designed to
produce. Look at the function named in the table above, update the regex in
`apply.py`, and check that the helper still compiles against the new API — the
helper uses `intent.SendMessage`, `event.ReactionEventContent`,
`event.MessageEventContent`, `event.RelatesTo.InReplyTo`, `MatrixSendExtra`,
`Bridge.Bot` and `Bridge.GetGhostByID`, all of which are identical in v0.28 and
v0.31. Note the bridgev2 guard passes `source` to `shouldKeepDeleted` as well
as `intent` to the marker, because deciding whose message it was needs the
login.
