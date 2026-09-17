// Keep remotely-deleted messages, marking them instead of redacting them.
//
// Injected into mautrix-go's bridgev2 package by patches/apply.py. Upstream
// bridgev2 redacts the Matrix event when the remote network reports a
// deletion (Portal.handleRemoteMessageRemove), and a Matrix redaction strips
// the content server-side and irreversibly. On a self-hosted bridge holding
// your own history that is usually not what you want.
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

// keepDeletedMessages is the master switch, read from
// $BRIDGE_KEEP_DELETED_MESSAGES at startup. When false the bridge behaves
// exactly like upstream and none of the settings below are consulted.
var keepDeletedMessages = func() bool {
	v, err := strconv.ParseBool(os.Getenv("BRIDGE_KEEP_DELETED_MESSAGES"))
	return err == nil && v
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
		resp, err := intent.SendMessage(ctx, portal.MXID, event.EventReaction, &event.Content{
			Parsed: &event.ReactionEventContent{
				RelatesTo: event.RelatesTo{
					Type:    event.RelAnnotation,
					EventID: target.MXID,
					Key:     keepDeletedMarker,
				},
			},
		}, &MatrixSendExtra{Timestamp: ts})
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
	resp, err := intent.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{
		Parsed: &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    body,
			RelatesTo: &event.RelatesTo{
				InReplyTo: &event.InReplyTo{EventID: target.MXID},
			},
		},
	}, &MatrixSendExtra{Timestamp: ts})
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
