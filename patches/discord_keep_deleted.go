// Keep remotely-deleted messages, marking them instead of redacting them.
//
// mautrix-discord is still a bridgev1 bridge, so it has its own deletion path
// (Portal.redactAllParts) rather than sharing bridgev2's. This file is the
// Discord equivalent of patches/bridgev2_keep_deleted.go; the call site is
// rewritten by patches/apply.py. Same modes (off/all/self/other), same two
// markers, same variables, same semantics — see that file for the details.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/database"
)

// off/all/self/other, with the legacy bools still meaning all/off and an
// unrecognised value erring towards keeping the message. See the bridgev2
// patch for the reasoning.
type keepDeletedMode int

const (
	keepDeletedOff keepDeletedMode = iota
	keepDeletedAll
	keepDeletedSelf
	keepDeletedOther
)

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

// Unset means the default 🗑️; set-but-empty means no reaction at all.
var keepDeletedMarker = func() string {
	v, ok := os.LookupEnv("BRIDGE_KEEP_DELETED_MARKER")
	if !ok {
		return "\U0001F5D1️"
	}
	return v
}()

// Bool, defaulting to whether deletions are kept at all. See the bridgev2
// patch.
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

const keepDeletedNoticeTimeFormat = "2006-01-02 15:04"

// shouldKeepDeleted decides whether this particular deletion is kept; see the
// bridgev2 patch for why the side is the message's sender rather than the
// party who issued the deletion.
func (portal *Portal) shouldKeepDeleted(parts []*database.Message) bool {
	switch keepDeletedMessages {
	case keepDeletedOff:
		return false
	case keepDeletedAll:
		if keepDeletedModeInvalid != "" {
			portal.log.Warn().
				Str("configured_mode", keepDeletedModeInvalid).
				Msg("Unknown BRIDGE_KEEP_DELETED_MESSAGES, keeping all deleted messages")
		}
		return true
	}
	if len(parts) == 0 {
		return false
	}
	isOwn := portal.keepDeletedIsOwnMessage(parts[0])
	if keepDeletedMessages == keepDeletedSelf {
		return isOwn
	}
	return !isOwn
}

// keepDeletedIsOwnMessage reports whether the message was sent by a logged-in
// user of this bridge.
//
// GetUserByID is a lookup, not a constructor: with no matching row it reaches
// loadUser(nil, nil), which returns nil without inserting anything. The portal
// receiver is checked as well because it holds the owner's Discord ID in DMs,
// which is the case where the user table lookup matters least and is most
// likely to be racing a fresh login.
func (portal *Portal) keepDeletedIsOwnMessage(msg *database.Message) bool {
	if msg.SenderID == "" {
		return false
	}
	if msg.SenderID == portal.Key.Receiver {
		return true
	}
	return portal.bridge.GetUserByID(msg.SenderID) != nil
}

// markDeletedParts marks the first part of a deleted message and keeps both
// the Matrix events and the bridge's database rows, so replies and edits
// pointing at the message still resolve.
func (portal *Portal) markDeletedParts(intent *appservice.IntentAPI, parts []*database.Message) (lastResp id.EventID) {
	var target *database.Message
	for _, dbMsg := range parts {
		if dbMsg.MXID != "" {
			target = dbMsg
			break
		}
	}
	if target == nil {
		return
	}

	if keepDeletedMarker != "" {
		resp, err := intent.SendMessageEvent(portal.MXID, event.EventReaction, &event.ReactionEventContent{
			RelatesTo: event.RelatesTo{
				Type:    event.RelAnnotation,
				EventID: target.MXID,
				Key:     keepDeletedMarker,
			},
		})
		if err != nil {
			portal.log.Err(err).
				Str("event_id", target.MXID.String()).
				Msg("Failed to react to deleted Matrix message")
		} else {
			portal.log.Debug().
				Str("event_id", target.MXID.String()).
				Str("reaction_id", resp.EventID.String()).
				Msg("Marked deleted message instead of redacting it")
			lastResp = resp.EventID
		}
	}

	if keepDeletedNotice {
		if keepDeletedNoticeInvalid != "" {
			portal.log.Warn().
				Str("configured_notice", keepDeletedNoticeInvalid).
				Msg("Unknown BRIDGE_KEEP_DELETED_NOTICE, following BRIDGE_KEEP_DELETED_MESSAGES")
		}
		if evtID := portal.sendKeepDeletedNotice(target); evtID != "" {
			lastResp = evtID
		}
	}

	return
}

// sendKeepDeletedNotice posts the deletion notice as a reply to the kept
// message, from the BRIDGE BOT — see the bridgev2 patch for why the bot and
// not a ghost, and why the body carries no HTML.
//
// Discord has no deletion timestamp in the gateway event, so the notice is
// stamped with the time the bridge saw it, which is within a second of it.
func (portal *Portal) sendKeepDeletedNotice(target *database.Message) id.EventID {
	bot := portal.bridge.Bot
	if bot == nil {
		portal.log.Warn().Msg("No bridge bot intent, skipping keep-deleted notice")
		return ""
	}
	// Guild channels have the bot in them; a DM portal may not.
	if err := bot.EnsureJoined(portal.MXID); err != nil {
		portal.log.Debug().Err(err).Msg("Failed to ensure the bridge bot is in the room for a keep-deleted notice")
	}
	body := fmt.Sprintf(
		"🗑️ %s deleted this message at %s",
		portal.keepDeletedSenderName(target),
		time.Now().Local().Format(keepDeletedNoticeTimeFormat),
	)
	resp, err := bot.SendMessageEvent(portal.MXID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    body,
		RelatesTo: &event.RelatesTo{
			InReplyTo: &event.InReplyTo{EventID: target.MXID},
		},
	})
	if err != nil {
		portal.log.Err(err).
			Str("event_id", target.MXID.String()).
			Msg("Failed to send keep-deleted notice")
		return ""
	}
	portal.log.Debug().
		Str("event_id", target.MXID.String()).
		Str("notice_id", resp.EventID.String()).
		Msg("Sent keep-deleted notice")
	return resp.EventID
}

// keepDeletedSenderName names the sender of the deleted message.
//
// DB.Puppet.Get rather than bridge.GetPuppetByID, because the latter INSERTS a
// puppet row for an unknown ID; a display name is not worth a write.
func (portal *Portal) keepDeletedSenderName(target *database.Message) string {
	if target.SenderID == "" {
		return "Someone"
	}
	if puppet := portal.bridge.DB.Puppet.Get(target.SenderID); puppet != nil && puppet.Name != "" {
		return puppet.Name
	}
	return "Someone"
}
