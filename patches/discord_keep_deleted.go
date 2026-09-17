// Keep remotely-deleted messages, marking them instead of redacting them.
//
// mautrix-discord is still a bridgev1 bridge, so it has its own deletion path
// (Portal.redactAllParts) rather than sharing bridgev2's. This file is the
// Discord equivalent of patches/bridgev2_keep_deleted.go; the call site is
// rewritten by patches/apply.py. Same three independent markers, same
// variables, same semantics — see that file for the details.
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

var keepDeletedMessages = func() bool {
	v, err := strconv.ParseBool(os.Getenv("BRIDGE_KEEP_DELETED_MESSAGES"))
	return err == nil && v
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
