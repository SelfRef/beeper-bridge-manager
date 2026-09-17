// Keep remotely-deleted messages, marking them with a reaction instead.
//
// mautrix-discord is still a bridgev1 bridge, so it has its own deletion path
// (Portal.redactAllParts) rather than sharing bridgev2's. This file is the
// Discord equivalent of patches/bridgev2_keep_deleted.go; the call site is
// rewritten by patches/apply.py.
package main

import (
	"os"
	"strconv"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/database"
)

// keepDeletedMessages is set from $BRIDGE_KEEP_DELETED_MESSAGES at startup.
var keepDeletedMessages = func() bool {
	v, err := strconv.ParseBool(os.Getenv("BRIDGE_KEEP_DELETED_MESSAGES"))
	return err == nil && v
}()

// keepDeletedMarker defaults to 🗑️; override with $BRIDGE_KEEP_DELETED_MARKER.
var keepDeletedMarker = func() string {
	if v := os.Getenv("BRIDGE_KEEP_DELETED_MARKER"); v != "" {
		return v
	}
	return "\U0001F5D1️"
}()

// markDeletedParts reacts to the first part of a deleted message and keeps
// both the Matrix events and the bridge's database rows, so replies and edits
// pointing at the message still resolve.
func (portal *Portal) markDeletedParts(intent *appservice.IntentAPI, parts []*database.Message) (lastResp id.EventID) {
	for _, dbMsg := range parts {
		if dbMsg.MXID == "" {
			continue
		}
		resp, err := intent.SendMessageEvent(portal.MXID, event.EventReaction, &event.ReactionEventContent{
			RelatesTo: event.RelatesTo{
				Type:    event.RelAnnotation,
				EventID: dbMsg.MXID,
				Key:     keepDeletedMarker,
			},
		})
		if err != nil {
			portal.log.Err(err).
				Str("event_id", dbMsg.MXID.String()).
				Msg("Failed to mark deleted Matrix message")
			return
		}
		portal.log.Debug().
			Str("event_id", dbMsg.MXID.String()).
			Str("reaction_id", resp.EventID.String()).
			Msg("Marked deleted message instead of redacting it")
		return resp.EventID
	}
	return
}
