#!/usr/bin/env python3
"""Apply this image's source patches to a freshly cloned bridge checkout.

Two targets, because the bridges do not share a code base:

  bridgev2  -- maunium.net/go/mautrix, used by every bridge except Discord.
  discord   -- go.mau.fi/mautrix-discord, still bridgev1.

Two patches, applied together to whichever target is given:

  keep-deleted  a remote deletion marks the message instead of redacting it.
  beeper-watch  every bridged event is reported, in plaintext, to an HTTP
                endpoint, so something outside the bridge can react to it.

Both are inert unless their environment variables are set, so a binary built
here behaves exactly like the stock one until it is configured.

Each patch is a set of helper files copied in, plus edits at named anchors.
Anchors are regexes rather than context diffs, because mautrix-go versions
differ in these signatures and several bridges pin different versions — but
every anchor is REQUIRED: a miss is a hard failure, never a silent no-op. That
matters because a silently unpatched binary looks identical until the first
deletion, or until the first message that should have triggered something.

Two edit kinds:

  insert  put a guard above (or below) the anchor line.
  rename  give a function a new name, so a wrapper in a helper file can take
          the original name and call through. Upstream changing that
          function's signature then breaks the build instead of quietly
          dropping the hook.

Usage:  apply.py bridgev2 <path-to-mautrix-go>
        apply.py discord  <path-to-mautrix-discord>
"""

import pathlib
import re
import sys

HERE = pathlib.Path(__file__).resolve().parent

# Rewritten into whichever package the client is installed in; see
# watch_client.go.
WATCH_PACKAGE_PLACEHOLDER = "__WATCH_PACKAGE__"


# --- keep-deleted ----------------------------------------------------------
#
# The upstream line that performs the redaction, in both known shapes:
#   v0.31: redactMessageParts(ctx, targetParts, intent, getEventTS(evt), "", dontRenderPlaceholder)
#   v0.28: redactMessageParts(ctx, targetParts, intent, getEventTS(evt))
BRIDGEV2_KEEP_DELETED_ANCHOR = re.compile(
    r"^(?P<indent>[ \t]*)res := portal\.redactMessageParts\(ctx, targetParts, intent, getEventTS\(evt\).*$",
    re.MULTILINE,
)

BRIDGEV2_KEEP_DELETED_GUARD = """{indent}// PATCH keep-deleted: mark the message instead of redacting it.
{indent}if portal.shouldKeepDeleted(ctx, targetParts, source) {{
{indent}\treturn portal.markRemovedMessageParts(ctx, targetParts, intent, getEventTS(evt))
{indent}}}
"""

# Discord's deletion loop; the guard goes after the DB lookup so the helper can
# reuse it, and before the loop that redacts and deletes the rows.
DISCORD_KEEP_DELETED_ANCHOR = re.compile(
    r"^(?P<indent>[ \t]*)existing := portal\.bridge\.DB\.Message\.GetByDiscordID\(portal\.Key, msgID\)[ \t]*$",
    re.MULTILINE,
)

DISCORD_KEEP_DELETED_GUARD = """{indent}// PATCH keep-deleted: mark the message instead of redacting it.
{indent}if portal.shouldKeepDeleted(existing) {{
{indent}\treturn portal.markDeletedParts(intent, existing)
{indent}}}
"""


def rename(receiver: str, name: str) -> re.Pattern:
    """Anchor for a method declaration, matched by receiver and name only.

    The parameter list is deliberately not part of the pattern: the wrapper in
    the helper file has to repeat the signature, so a signature change is
    already caught by the compiler with a far better error message than a
    regex miss would give.
    """
    return re.compile(
        r"^func \(" + re.escape(receiver) + r"\) " + re.escape(name) + r"\(",
        re.MULTILINE,
    )


PATCHES = {
    "bridgev2": {
        "root_check": "bridgev2/portal.go",
        "keep-deleted": {
            "files": {"bridgev2_keep_deleted.go": "bridgev2/zz_keep_deleted.go"},
            "edits": [
                {
                    "kind": "insert",
                    "file": "bridgev2/portal.go",
                    "anchor": BRIDGEV2_KEEP_DELETED_ANCHOR,
                    "guard": BRIDGEV2_KEEP_DELETED_GUARD,
                    "where": "above",
                    "marker": "PATCH keep-deleted",
                }
            ],
        },
        "beeper-watch": {
            "files": {
                "watch_client.go": "bridgev2/zz_beeper_watch_client.go",
                "bridgev2_watch_hooks.go": "bridgev2/zz_beeper_watch.go",
                "bridgev2_matrix_watch.go": "bridgev2/matrix/zz_beeper_watch.go",
            },
            "package": {"bridgev2/zz_beeper_watch_client.go": "bridgev2"},
            "edits": [
                # The one point every bridged event passes through before
                # encryption.
                {
                    "kind": "rename",
                    "file": "bridgev2/matrix/intent.go",
                    "anchor": rename("as *ASIntent", "SendMessage"),
                    "to": "sendMessageUnhooked",
                },
                # The Matrix -> remote direction.
                {
                    "kind": "rename",
                    "file": "bridgev2/portal.go",
                    "anchor": rename("portal *Portal", "handleMatrixEvent"),
                    "to": "handleMatrixEventUnhooked",
                },
            ],
        },
    },
    "discord": {
        "root_check": "portal.go",
        "keep-deleted": {
            "files": {"discord_keep_deleted.go": "zz_keep_deleted.go"},
            "edits": [
                {
                    "kind": "insert",
                    "file": "portal.go",
                    "anchor": DISCORD_KEEP_DELETED_ANCHOR,
                    "guard": DISCORD_KEEP_DELETED_GUARD,
                    "where": "below",
                    "marker": "PATCH keep-deleted",
                }
            ],
        },
        "beeper-watch": {
            "files": {
                "watch_client.go": "zz_beeper_watch_client.go",
                "discord_watch_hooks.go": "zz_beeper_watch.go",
            },
            "package": {"zz_beeper_watch_client.go": "main"},
            "edits": [
                # Messages, edits, media and notices, before encryption.
                {
                    "kind": "rename",
                    "file": "portal.go",
                    "anchor": rename("portal *Portal", "sendMatrixMessage"),
                    "to": "sendMatrixMessageUnhooked",
                },
                # Remote deletions.
                {
                    "kind": "rename",
                    "file": "portal.go",
                    "anchor": rename("portal *Portal", "redactAllParts"),
                    "to": "redactAllPartsUnhooked",
                },
                # Reactions, added and removed.
                {
                    "kind": "rename",
                    "file": "portal.go",
                    "anchor": rename("portal *Portal", "handleDiscordReaction"),
                    "to": "handleDiscordReactionUnhooked",
                },
                # Everything the local user sends.
                {
                    "kind": "rename",
                    "file": "portal.go",
                    "anchor": rename("portal *Portal", "handleMatrixMessages"),
                    "to": "handleMatrixMessagesUnhooked",
                },
            ],
        },
    },
}


def fatal(msg: str) -> None:
    raise SystemExit(f"FATAL: {msg}")


def install_file(src: pathlib.Path, dest: pathlib.Path, package: str | None) -> None:
    if not src.exists():
        fatal(f"{src} is missing from the patch set")
    if not dest.parent.is_dir():
        fatal(f"{dest.parent} not found — wrong checkout?")
    body = src.read_text()
    if package is not None:
        if WATCH_PACKAGE_PLACEHOLDER not in body:
            fatal(f"{src} has no {WATCH_PACKAGE_PLACEHOLDER} to replace")
        body = body.replace(f"package {WATCH_PACKAGE_PLACEHOLDER}", f"package {package}")
    dest.write_text(body)
    print(f"  {dest}: installed")


def apply_insert(path: pathlib.Path, edit: dict, what: str) -> None:
    src = path.read_text()
    if edit["marker"] in src:
        print(f"  {path}: already patched, skipping")
        return
    matches = list(edit["anchor"].finditer(src))
    if len(matches) != 1:
        fatal(
            f"expected exactly 1 {what} anchor in {path}, found {len(matches)}.\n"
            f"       Upstream changed shape; update patches/apply.py before shipping."
        )
    m = matches[0]
    at = m.start() if edit["where"] == "above" else m.end() + 1
    path.write_text(src[:at] + edit["guard"].format(indent=m.group("indent")) + src[at:])
    print(f"  {path}: guard inserted at offset {at}")


def apply_rename(path: pathlib.Path, edit: dict, what: str) -> None:
    src = path.read_text()
    if re.search(r"\b" + re.escape(edit["to"]) + r"\b", src):
        print(f"  {path}: already renamed to {edit['to']}, skipping")
        return
    matches = list(edit["anchor"].finditer(src))
    if len(matches) != 1:
        fatal(
            f"expected exactly 1 {what} anchor in {path}, found {len(matches)}.\n"
            f"       Upstream moved or renamed it; update patches/apply.py before shipping."
        )
    m = matches[0]
    head = m.group(0)
    # The anchor ends at the opening paren of the parameter list, so the new
    # name is a straight substitution of the last identifier in it.
    renamed = head[: head.rindex(" ") + 1] + edit["to"] + "("
    path.write_text(src[: m.start()] + renamed + src[m.end() :])
    print(f"  {path}: renamed to {edit['to']}")


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    target, root = sys.argv[1], pathlib.Path(sys.argv[2]).resolve()
    spec = PATCHES.get(target)
    if spec is None:
        fatal(f"unknown target {target!r} (known: {', '.join(PATCHES)})")
    if not root.is_dir():
        fatal(f"{root} is not a directory")
    if not (root / spec["root_check"]).exists():
        fatal(f"{root / spec['root_check']} not found — wrong checkout?")

    print(f"patching {target} in {root}")
    for name, patch in spec.items():
        if name == "root_check":
            continue
        print(f"[{name}]")
        packages = patch.get("package", {})
        for src, dest in patch["files"].items():
            install_file(HERE / src, root / dest, packages.get(dest))
        for edit in patch["edits"]:
            path = root / edit["file"]
            if not path.exists():
                fatal(f"{path} not found — wrong checkout?")
            if edit["kind"] == "insert":
                apply_insert(path, edit, name)
            else:
                apply_rename(path, edit, name)


if __name__ == "__main__":
    main()
