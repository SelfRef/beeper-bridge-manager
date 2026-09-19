// Keep remotely-deleted messages, marking them instead of redacting them.
//
// Injected into mautrix-go's bridgev2 package by patches/apply.py. Upstream
// bridgev2 redacts the Matrix event when the remote network reports a
// deletion (Portal.handleRemoteMessageRemove), and a Matrix redaction strips
// the content server-side and irreversibly. On a self-hosted bridge holding
// your own history that is usually not what you want.
//
// BRIDGE_KEEP_DELETED_MESSAGES picks WHOSE deletions are kept:
//
//	off (default)  stock upstream: every deletion redacts
//	all            keep everything
//	self           keep only messages you sent; other people's still redact
//	other          keep only other people's messages; your own still redact
//
// Three independent markers, each off when its variable is empty:
//
//	BRIDGE_KEEP_DELETED_MARKER      reaction on the message (default 🗑️)
//	BRIDGE_KEEP_DELETED_NOTICE      text of one m.notice replying to the message
//	BRIDGE_KEEP_DELETED_NOTICE_SIDE whose side that notice appears on:
//	                                "self" (default) or "sender"
//
// With both empty the message is simply kept, silently. Nothing here is
// ever relayed to the remote network: notices go out through intents, and
// mautrix stamps double-puppeted events with fi.mau.double_puppet_source,
// which Connector.shouldIgnoreEvent drops on the way back in.
package bridgev2

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
)

// keepDeletedMode is the master switch, read from
// $BRIDGE_KEEP_DELETED_MESSAGES at startup. It is an enum rather than a bool
// because the two sides of a conversation are worth treating differently:
// keeping what the other party deleted is the point of the patch, while your
// own deletions are usually meant to be deletions.
type keepDeletedMode int

const (
	keepDeletedOff keepDeletedMode = iota
	keepDeletedAll
	keepDeletedSelf
	keepDeletedOther
)

// keepDeletedMessages is parsed once at startup. "true"/"false" and the other
// strconv.ParseBool spellings still work and mean all/off, so an older
// deployment keeps behaving the same. An unrecognised value keeps the message
// rather than redacting it: this switch exists to avoid irreversible content
// loss, so a typo must not cause any. keepDeletedModeInvalid then holds that
// value, so the fallback can be reported at the call site, where there is a
// logger to report it to.
var keepDeletedMessages, keepDeletedModeInvalid = func() (keepDeletedMode, string) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("BRIDGE_KEEP_DELETED_MESSAGES")))
	switch raw {
	case "", "off":
		return keepDeletedOff, ""
	case "all":
		return keepDeletedAll, ""
	case "self", "mine":
		return keepDeletedSelf, ""
	case "other", "others", "theirs":
		return keepDeletedOther, ""
	}
	if v, err := strconv.ParseBool(raw); err == nil {
		if v {
			return keepDeletedAll, ""
		}
		return keepDeletedOff, ""
	}
	return keepDeletedAll, raw
}()

// keepDeletedMarker is the reaction placed on a kept message.
//
// UNSET means the default 🗑️ (U+1F5D1 U+FE0F); set-but-EMPTY means no
// reaction at all. That distinction is why this uses LookupEnv rather than
// Getenv — "" has to be a usable value, not a synonym for "give me the
// default".
var keepDeletedMarker = func() string {
	v, ok := os.LookupEnv("BRIDGE_KEEP_DELETED_MARKER")
	if !ok {
		return "\U0001F5D1️"
	}
	return v
}()

// keepDeletedNotice is the body of a single m.notice replying to the kept
// message. Empty disables it.
var keepDeletedNotice = os.Getenv("BRIDGE_KEEP_DELETED_NOTICE")

// keepDeletedNoticeSide decides who that notice is sent as:
//
//	self   (default) your own Matrix user, so it renders on your side.
//	               Requires double puppeting.
//	sender         the party who deleted the message, so it renders on theirs.
//
// There is deliberately only one notice; picking a side is the whole choice.
var keepDeletedNoticeSide = strings.ToLower(strings.TrimSpace(os.Getenv("BRIDGE_KEEP_DELETED_NOTICE_SIDE")))

// shouldKeepDeleted decides whether this particular deletion is kept. It is
// the whole guard inserted at the call site, so an off switch costs one
// comparison and never touches the database or the network client.
//
// The side is taken from who SENT the message, not from who deleted it: the
// choice being made is whose content you keep. Most networks only let you
// delete your own messages anyway, so the two coincide except for admin
// deletions in groups, which count as the original sender's side.
func (portal *Portal) shouldKeepDeleted(ctx context.Context, parts []*database.Message, source *UserLogin) bool {
	switch keepDeletedMessages {
	case keepDeletedOff:
		return false
	case keepDeletedAll:
		if keepDeletedModeInvalid != "" {
			zerolog.Ctx(ctx).Warn().
				Str("configured_mode", keepDeletedModeInvalid).
				Msg("Unknown BRIDGE_KEEP_DELETED_MESSAGES, keeping all deleted messages")
		}
		return true
	}
	if len(parts) == 0 {
		return false
	}
	isOwn := portal.keepDeletedIsOwnMessage(ctx, parts[0], source)
	if keepDeletedMessages == keepDeletedSelf {
		return isOwn
	}
	return !isOwn
}

// keepDeletedIsOwnMessage reports whether the local user sent the message.
//
// Three signals, because which ones are populated depends on the network
// connector and on whether double puppeting is enabled:
//
//	IsDoublePuppeted  the Matrix event was sent as the local user
//	SenderMXID        same, for connectors that store the MXID without the flag
//	IsThisUser        the connector's own answer for a remote user ID
//
// IsThisUser is part of NetworkAPI, so every bridge implements it; it is
// checked last because it is the only one that can hit the network client.
func (portal *Portal) keepDeletedIsOwnMessage(ctx context.Context, part *database.Message, source *UserLogin) bool {
	if part.IsDoublePuppeted {
		return true
	}
	if part.SenderMXID != "" && part.SenderMXID == source.UserMXID {
		return true
	}
	return part.SenderID != "" && source.Client != nil && source.Client.IsThisUser(ctx, part.SenderID)
}

// markRemovedMessageParts handles a remote deletion without redacting.
//
// Only the first real part is marked: one deletion should produce one set of
// markers, not one per attachment or caption event.
func (portal *Portal) markRemovedMessageParts(
	ctx context.Context,
	parts []*database.Message,
	intent MatrixAPI,
	source *UserLogin,
	ts time.Time,
) EventHandlingResult {
	log := zerolog.Ctx(ctx)
	var target *database.Message
	for _, part := range parts {
		if part.MXID != "" {
			target = part
			break
		}
	}
	if target == nil {
		log.Debug().Msg("Remote deletion target has no Matrix event, nothing to mark")
		return EventHandlingResultIgnored
	}

	var attempted, failed int

	if keepDeletedMarker != "" {
		attempted++
		content := &event.Content{
			Parsed: &event.ReactionEventContent{
				RelatesTo: event.RelatesTo{
					Type:    event.RelAnnotation,
					EventID: target.MXID,
					Key:     keepDeletedMarker,
				},
			},
		}
		// What is being sent is a reaction; what HAPPENED is a deletion.
		// Without this the watcher could not tell this marker apart from
		// someone reacting with the same emoji. The marker is consumed and
		// removed by the reporter, so it never reaches the homeserver.
		WatchMarkKind(content, "deletion")
		resp, err := intent.SendMessage(ctx, portal.MXID, event.EventReaction, content, &MatrixSendExtra{Timestamp: ts})
		if err != nil {
			failed++
			log.Err(err).Stringer("event_id", target.MXID).Msg("Failed to react to remotely deleted message")
		} else {
			log.Debug().
				Stringer("event_id", target.MXID).
				Stringer("reaction_id", resp.EventID).
				Msg("Marked remotely deleted message instead of redacting it")
		}
	}

	if keepDeletedNotice != "" {
		attempted++
		side := keepDeletedNoticeSide
		if side != "self" && side != "sender" && side != "" {
			log.Warn().
				Str("configured_side", side).
				Msg("Unknown BRIDGE_KEEP_DELETED_NOTICE_SIDE, falling back to self")
			side = "self"
		}
		noticeIntent := intent
		if side != "sender" {
			side = "self"
			noticeIntent = source.User.DoublePuppet(ctx)
		}
		if noticeIntent == nil {
			failed++
			log.Warn().Msg("Double puppeting unavailable, skipping keep-deleted notice")
		} else if !portal.sendKeepDeletedNotice(ctx, noticeIntent, target, keepDeletedNotice, ts, side) {
			failed++
		}
	}

	// Nothing configured is a valid setup: keep the message silently.
	if attempted > 0 && failed == attempted {
		return EventHandlingResultFailed
	}
	return EventHandlingResultSuccess
}

// sendKeepDeletedNotice posts one m.notice as a reply to the kept message.
func (portal *Portal) sendKeepDeletedNotice(
	ctx context.Context,
	intent MatrixAPI,
	target *database.Message,
	body string,
	ts time.Time,
	kind string,
) bool {
	log := zerolog.Ctx(ctx)
	content := &event.Content{
		Parsed: &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    body,
			RelatesTo: &event.RelatesTo{
				InReplyTo: &event.InReplyTo{EventID: target.MXID},
			},
		},
	}
	// Same reasoning as the marker reaction: this bubble is a deletion, not
	// a message someone typed.
	WatchMarkKind(content, "deletion")
	resp, err := intent.SendMessage(ctx, portal.MXID, event.EventMessage, content, &MatrixSendExtra{Timestamp: ts})
	if err != nil {
		log.Err(err).
			Str("notice_kind", kind).
			Stringer("event_id", target.MXID).
			Msg("Failed to send keep-deleted notice")
		return false
	}
	log.Debug().
		Str("notice_kind", kind).
		Stringer("event_id", target.MXID).
		Stringer("notice_id", resp.EventID).
		Msg("Sent keep-deleted notice")
	return true
}
