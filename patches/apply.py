#!/usr/bin/env python3
"""Rewrite a bridge's deletion path so remote deletes mark instead of redact.

Two targets, because the bridges do not share one deletion path:

  bridgev2  -- maunium.net/go/mautrix, used by every bridge except Discord.
               Rewrites Portal.handleRemoteMessageRemove in bridgev2/portal.go.
  discord   -- go.mau.fi/mautrix-discord, still bridgev1.
               Rewrites Portal.redactAllParts in portal.go.

Anchors are matched with regexes that tolerate upstream signature changes
(mautrix-go v0.28 and v0.31 differ in both calls), but every anchor is
REQUIRED: a miss is a hard failure, never a silent no-op. That matters because
a silently unpatched binary looks identical until the first deletion.

Usage:  apply.py bridgev2 <path-to-mautrix-go>
        apply.py discord  <path-to-mautrix-discord>
"""

import pathlib
import re
import shutil
import sys

HERE = pathlib.Path(__file__).resolve().parent

# The upstream line that performs the redaction, in both known shapes:
#   v0.31: redactMessageParts(ctx, targetParts, intent, getEventTS(evt), "", dontRenderPlaceholder)
#   v0.28: redactMessageParts(ctx, targetParts, intent, getEventTS(evt))
BRIDGEV2_ANCHOR = re.compile(
    r"^(?P<indent>[ \t]*)res := portal\.redactMessageParts\(ctx, targetParts, intent, getEventTS\(evt\).*$",
    re.MULTILINE,
)

BRIDGEV2_GUARD = """{indent}// PATCH keep-deleted: mark the message instead of redacting it.
{indent}if keepDeletedMessages {{
{indent}\treturn portal.markRemovedMessageParts(ctx, targetParts, intent, source, getEventTS(evt))
{indent}}}
"""

# Discord's deletion loop; the guard goes after the DB lookup so the helper can
# reuse it, and before the loop that redacts and deletes the rows.
DISCORD_ANCHOR = re.compile(
    r"^(?P<indent>[ \t]*)existing := portal\.bridge\.DB\.Message\.GetByDiscordID\(portal\.Key, msgID\)[ \t]*$",
    re.MULTILINE,
)

DISCORD_GUARD = """{indent}// PATCH keep-deleted: mark the message instead of redacting it.
{indent}if keepDeletedMessages {{
{indent}\treturn portal.markDeletedParts(intent, existing)
{indent}}}
"""

MARKER = "PATCH keep-deleted"


def patch_file(path: pathlib.Path, anchor: re.Pattern, guard: str, what: str) -> None:
    src = path.read_text()
    if MARKER in src:
        print(f"  {path}: already patched, skipping")
        return
    matches = list(anchor.finditer(src))
    if len(matches) != 1:
        raise SystemExit(
            f"FATAL: expected exactly 1 {what} anchor in {path}, found {len(matches)}.\n"
            f"       Upstream changed shape; update patches/apply.py before shipping."
        )
    m = matches[0]
    indent = m.group("indent")
    insert_at = m.start() if what == "bridgev2" else m.end() + 1
    block = guard.format(indent=indent)
    path.write_text(src[:insert_at] + block + src[insert_at:])
    print(f"  {path}: guard inserted at offset {insert_at}")


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    target, root = sys.argv[1], pathlib.Path(sys.argv[2]).resolve()
    if not root.is_dir():
        raise SystemExit(f"FATAL: {root} is not a directory")

    if target == "bridgev2":
        portal = root / "bridgev2" / "portal.go"
        helper = root / "bridgev2" / "zz_keep_deleted.go"
        src = HERE / "bridgev2_keep_deleted.go"
        anchor, guard = BRIDGEV2_ANCHOR, BRIDGEV2_GUARD
    elif target == "discord":
        portal = root / "portal.go"
        helper = root / "zz_keep_deleted.go"
        src = HERE / "discord_keep_deleted.go"
        anchor, guard = DISCORD_ANCHOR, DISCORD_GUARD
    else:
        raise SystemExit(f"FATAL: unknown target {target!r}")

    if not portal.exists():
        raise SystemExit(f"FATAL: {portal} not found — wrong checkout?")

    print(f"patching {target} in {root}")
    shutil.copyfile(src, helper)
    print(f"  {helper}: helper installed")
    patch_file(portal, anchor, guard, target)


if __name__ == "__main__":
    main()
