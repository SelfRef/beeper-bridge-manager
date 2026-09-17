# patches

Source modifications compiled into the bridge binaries this image ships.
Applied by `apply.py`, driven by `../build-bridges.sh`.

## keep-deleted

Stops a remote deletion from redacting the Matrix event. Instead the message is
left intact and marked with a single 🗑️ reaction.

Runtime switch, read once at bridge start:

| Variable | Default | Effect |
|---|---|---|
| `BRIDGE_KEEP_DELETED_MESSAGES` | unset | `true` enables the behaviour; anything else is stock upstream |
| `BRIDGE_KEEP_DELETED_MARKER` | `🗑️` | Reaction key used as the marker |

The container-level knobs are `KEEP_DELETED_MESSAGES` / `KEEP_DELETED_MARKER`;
the entrypoint translates them into the `BRIDGE_*` variables above, and only
for bridges that actually have a patched binary.

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

Re-running is safe: a file already containing `PATCH keep-deleted` is skipped.

### Why the call site and not `redactMessageParts`

`redactMessageParts` has a second caller (`portalinternal.go`) used for
Matrix-side redactions, which should keep working normally. Only the remote
deletion path is changed.

### Why the database rows are kept

Upstream calls `DB.Message.DeleteAllParts` right after redacting. The patch
returns before that, so replies, edits and reactions that point at the message
still resolve afterwards.

## Updating for a new upstream

If a build fails with `FATAL: expected exactly 1 … anchor`, upstream moved the
code. Look at the function named in the table above, update the regex in
`apply.py`, and check that the helper still compiles against the new API — the
helper uses `intent.SendMessage`, `event.ReactionEventContent` and
`MatrixSendExtra`, which have been stable across v0.28–v0.31.
