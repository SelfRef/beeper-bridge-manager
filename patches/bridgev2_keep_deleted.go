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
// Two independent markers on a kept message:
//
//	BRIDGE_KEEP_DELETED_MARKER  reaction on the message (default 🗑️), empty
//	                            for none
//	BRIDGE_KEEP_DELETED_NOTICE  bool; one bridge-bot m.notice replying to the
//	                            message, saying who deleted it and when.
//	                            Unset follows BRIDGE_KEEP_DELETED_MESSAGES
//
// With both off the message is simply kept, silently. Nothing here is ever
// relayed to the remote network: markers go out through intents, and mautrix
// stamps double-puppeted events with fi.mau.double_puppet_source, which
// Connector.shouldIgnoreEvent drops on the way back in.
package bridgev2

import (
	"context"
	"fmt"
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

// keepDeletedNotice turns on the deletion notice. Its default is derived from
// the master switch — someone who asked for deletions to be kept wants to see
// that one happened — but it is an independent bool, so either combination
// can be asked for explicitly. Go initialises package variables in dependency
// order, not source order, so reading keepDeletedMessages here is safe.
//
// As with the mode, an unparseable value falls back rather than failing, and
// keepDeletedNoticeInvalid carries it to the call site to be logged.
var keepDeletedNotice, keepDeletedNoticeInvalid = func() (bool, string) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("BRIDGE_KEEP_DELETED_NOTICE")))
	if raw == "" {
		return keepDeletedMessages != keepDeletedOff, ""
	}
	if v, err := strconv.ParseBool(raw); err == nil {
		return v, ""
	}
	return keepDeletedMessages != keepDeletedOff, raw
}()

// keepDeletedNoticeTimeFormat is the datetime in the notice. Local time,
// because the person reading it is the one running the bridge.
const keepDeletedNoticeTimeFormat = "2006-01-02 15:04"

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
// Two signals, because which one is populated depends on the connector and on
// whether double puppeting is enabled:
//
//	SenderMXID  the Matrix event was sent as the local user
//	IsThisUser  the connector's own answer for a remote user ID
//
// IsThisUser is part of NetworkAPI, so every bridge implements it; it is
// checked last because it is the only one that can hit the network client.
//
// Message.IsDoublePuppeted is deliberately NOT used, although it is the field
// that names this exact question. It is unusable as read back from the
// database: Message.Scan sets it from `doublePuppeted.Valid`, which reports
// whether the column was non-NULL rather than whether it was true, and
// sqlVariables always writes a plain bool — so the column is never NULL and
// every stored message loads with the flag set. Trusting it made every message
// look like ours, which silently turned the `other` mode into `off`.
// SenderMXID covers the same case correctly: a double-puppeted message has the
// local user's MXID there.
func (portal *Portal) keepDeletedIsOwnMessage(ctx context.Context, part *database.Message, source *UserLogin) bool {
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

	if keepDeletedNotice {
		if keepDeletedNoticeInvalid != "" {
			log.Warn().
				Str("configured_notice", keepDeletedNoticeInvalid).
				Msg("Unknown BRIDGE_KEEP_DELETED_NOTICE, following BRIDGE_KEEP_DELETED_MESSAGES")
		}
		attempted++
		if !portal.sendKeepDeletedNotice(ctx, target, ts) {
			failed++
		}
	}

	// Nothing configured is a valid setup: keep the message silently.
	if attempted > 0 && failed == attempted {
		return EventHandlingResultFailed
	}
	return EventHandlingResultSuccess
}

// sendKeepDeletedNotice posts the deletion notice as a reply to the kept
// message.
//
// The sender is the BRIDGE BOT, not a ghost and not the local user: Beeper
// renders an m.notice from the bridge bot as dim centred text with no bubble,
// in any room, which is exactly the weight a piece of bridge bookkeeping
// should carry next to real messages. A ghost would get an ordinary bubble
// and read like something the other person said.
//
// The body is plain text with no formatted_body, so a display name containing
// HTML needs no escaping; the client never parses markdown in a plain body.
func (portal *Portal) sendKeepDeletedNotice(ctx context.Context, target *database.Message, ts time.Time) bool {
	log := zerolog.Ctx(ctx)
	bot := portal.Bridge.Bot
	if bot == nil {
		log.Warn().Msg("No bridge bot intent, skipping keep-deleted notice")
		return false
	}
	// Portals normally have the bot in them already, but a DM portal created
	// before the bot was a functional member does not.
	if err := bot.EnsureJoined(ctx, portal.MXID); err != nil {
		log.Debug().Err(err).Msg("Failed to ensure the bridge bot is in the room for a keep-deleted notice")
	}
	content := &event.Content{
		Parsed: &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body: fmt.Sprintf(
				"🗑️ %s deleted this message at %s",
				portal.keepDeletedSenderName(ctx, target),
				ts.Local().Format(keepDeletedNoticeTimeFormat),
			),
			RelatesTo: &event.RelatesTo{
				InReplyTo: &event.InReplyTo{EventID: target.MXID},
			},
		},
	}
	// Same reasoning as the marker reaction: this notice is a deletion, not
	// a message someone typed.
	WatchMarkKind(content, "deletion")
	resp, err := bot.SendMessage(ctx, portal.MXID, event.EventMessage, content, &MatrixSendExtra{Timestamp: ts})
	if err != nil {
		log.Err(err).
			Stringer("event_id", target.MXID).
			Msg("Failed to send keep-deleted notice")
		return false
	}
	log.Debug().
		Stringer("event_id", target.MXID).
		Stringer("notice_id", resp.EventID).
		Msg("Sent keep-deleted notice")
	return true
}

// keepDeletedSenderName names whoever the notice blames for the deletion.
//
// It is the message's SENDER rather than the party who issued the deletion,
// for the same reason shouldKeepDeleted uses that side: on most networks only
// the sender can delete, and where they differ the sender is the one whose
// content went away. A ghost with no synced profile yet, or a message with no
// sender recorded, falls back to a neutral word rather than an opaque ID.
func (portal *Portal) keepDeletedSenderName(ctx context.Context, target *database.Message) string {
	if target.SenderID == "" {
		return "Someone"
	}
	ghost, err := portal.Bridge.GetGhostByID(ctx, target.SenderID)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to look up the sender for a keep-deleted notice")
	} else if ghost != nil && ghost.Name != "" {
		return ghost.Name
	}
	return "Someone"
}
