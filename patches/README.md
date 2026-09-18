# patches

Source modifications compiled into the bridge binaries this image ships.
Applied by `apply.py`, driven by `../build-bridges.sh`.

## keep-deleted

Stops a remote deletion from redacting the Matrix event. Instead the message is
left intact and marked with a single 🗑️ reaction — for every deletion, or only
for one side of the conversation.

Runtime switch, read once at bridge start:

| Variable | Default | Effect |
|---|---|---|
| `BRIDGE_KEEP_DELETED_MESSAGES` | `off` | `off` / `all` / `self` / `other` — which side's deleted messages are kept. `true` and `false` still mean `all` and `off`; an unknown value warns and keeps everything |
| `BRIDGE_KEEP_DELETED_MARKER` | `🗑️` | Reaction key. Read with `LookupEnv`: **unset** = default, **set-but-empty** = no reaction |
| `BRIDGE_KEEP_DELETED_NOTICE` | unset | Body of one `m.notice` replying to the kept message |
| `BRIDGE_KEEP_DELETED_NOTICE_SIDE` | `self` | `self` (local user via double puppeting) or `sender` (the deleting party). Unknown values warn and fall back to `self` |

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
helper uses `intent.SendMessage`, `event.ReactionEventContent`,
`event.MessageEventContent`, `event.RelatesTo.InReplyTo`, `MatrixSendExtra` and
`(*UserLogin).User.DoublePuppet`, all of which are identical in v0.28 and
v0.31. Note the bridgev2 guard passes `source` as well as `intent`, because the
self notice needs the local user's double puppet.
