// Keep remotely-deleted messages, marking them with a reaction instead.
//
// Injected into mautrix-go's bridgev2 package by patches/apply.py. Upstream
// bridgev2 redacts the Matrix event when the remote network reports a
// deletion (Portal.handleRemoteMessageRemove), and a Matrix redaction strips
// the content server-side and irreversibly. On a self-hosted bridge holding
// your own history that is usually not what you want.
//
// This file only adds the alternative path; the call site is rewritten by the
// patcher so that the behaviour is opt-in at runtime.
package bridgev2

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
)

// keepDeletedMessages is set from $BRIDGE_KEEP_DELETED_MESSAGES at startup.
// When false, the bridge behaves exactly like upstream.
var keepDeletedMessages = func() bool {
	v, err := strconv.ParseBool(os.Getenv("BRIDGE_KEEP_DELETED_MESSAGES"))
	return err == nil && v
}()

// keepDeletedMarker is the reaction key used to mark a deleted message.
// Defaults to 🗑️ (U+1F5D1 U+FE0F); override with $BRIDGE_KEEP_DELETED_MARKER.
var keepDeletedMarker = func() string {
	if v := os.Getenv("BRIDGE_KEEP_DELETED_MARKER"); v != "" {
		return v
	}
	return "\U0001F5D1️"
}()

// markRemovedMessageParts reacts to the first real part of a message whose
// sender deleted it on the remote network, leaving every part intact.
//
// Only the first part is marked: one deletion should produce one marker, not
// one per attachment/caption event.
func (portal *Portal) markRemovedMessageParts(
	ctx context.Context,
	parts []*database.Message,
	intent MatrixAPI,
	ts time.Time,
) EventHandlingResult {
	log := zerolog.Ctx(ctx)
	for _, part := range parts {
		if part.MXID == "" {
			continue
		}
		resp, err := intent.SendMessage(ctx, portal.MXID, event.EventReaction, &event.Content{
			Parsed: &event.ReactionEventContent{
				RelatesTo: event.RelatesTo{
					Type:    event.RelAnnotation,
					EventID: part.MXID,
					Key:     keepDeletedMarker,
				},
			},
		}, &MatrixSendExtra{Timestamp: ts})
		if err != nil {
			log.Err(err).
				Stringer("event_id", part.MXID).
				Msg("Failed to mark remotely deleted message")
			return EventHandlingResultFailed.WithError(err)
		}
		log.Debug().
			Stringer("event_id", part.MXID).
			Stringer("reaction_id", resp.EventID).
			Msg("Marked remotely deleted message instead of redacting it")
		return EventHandlingResultSuccess
	}
	return EventHandlingResultIgnored
}
