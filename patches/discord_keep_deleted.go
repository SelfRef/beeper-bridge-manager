// Keep remotely-deleted messages, marking them instead of redacting them.
//
// mautrix-discord is still a bridgev1 bridge, so it has its own deletion path
// (Portal.redactAllParts) rather than sharing bridgev2's. This file is the
// Discord equivalent of patches/bridgev2_keep_deleted.go; the call site is
// rewritten by patches/apply.py. Same modes (off/all/self/other), same three
// independent markers, same variables, same semantics — see that file for the
// details.
package main

import (
	"os"
	"strconv"
	"strings"

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

var keepDeletedNotice = os.Getenv("BRIDGE_KEEP_DELETED_NOTICE")

// "self" (default) or "sender" — see the bridgev2 patch for the rationale.
var keepDeletedNoticeSide = strings.ToLower(strings.TrimSpace(os.Getenv("BRIDGE_KEEP_DELETED_NOTICE_SIDE")))

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

	if keepDeletedNotice != "" {
		side := keepDeletedNoticeSide
		if side != "self" && side != "sender" && side != "" {
			portal.log.Warn().
				Str("configured_side", side).
				Msg("Unknown BRIDGE_KEEP_DELETED_NOTICE_SIDE, falling back to self")
			side = "self"
		}
		noticeIntent := intent
		if side != "sender" {
			side = "self"
			// The portal's receiver is the Discord ID of the user who owns it;
			// guild channels have no receiver, so there is nobody to speak as.
			noticeIntent = nil
			if portal.Key.Receiver != "" {
				if user := portal.bridge.GetUserByID(portal.Key.Receiver); user != nil {
					noticeIntent = portal.bridge.GetPuppetByCustomMXID(user.MXID).CustomIntent()
				}
			}
		}
		if noticeIntent == nil {
			portal.log.Warn().Msg("Double puppeting unavailable, skipping keep-deleted notice")
		} else if evtID := portal.sendKeepDeletedNotice(noticeIntent, target, keepDeletedNotice, side); evtID != "" {
			lastResp = evtID
		}
	}

	return
}

// sendKeepDeletedNotice posts one m.notice as a reply to the kept message.
func (portal *Portal) sendKeepDeletedNotice(intent *appservice.IntentAPI, target *database.Message, body, kind string) id.EventID {
	resp, err := intent.SendMessageEvent(portal.MXID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    body,
		RelatesTo: &event.RelatesTo{
			InReplyTo: &event.InReplyTo{EventID: target.MXID},
		},
	})
	if err != nil {
		portal.log.Err(err).
			Str("notice_kind", kind).
			Str("event_id", target.MXID.String()).
			Msg("Failed to send keep-deleted notice")
		return ""
	}
	portal.log.Debug().
		Str("notice_kind", kind).
		Str("event_id", target.MXID.String()).
		Str("notice_id", resp.EventID.String()).
		Msg("Sent keep-deleted notice")
	return resp.EventID
}
