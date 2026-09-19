// The Discord half of the beeper-watch hooks.
//
// Injected into mautrix-discord's package main by patches/apply.py, alongside
// watch_client.go, which holds the client and the design notes.
//
// mautrix-discord is still a bridgev1 bridge, so it shares no send path with
// the other nine and needs its own hooks. There is no single funnel here the
// way bridgev2 has ASIntent.SendMessage, so this wraps four functions:
//
//	sendMatrixMessage      messages, edits, media and notices (pre-encryption)
//	redactAllParts         remote deletions
//	handleDiscordReaction  reactions, added and removed
//	handleMatrixMessages   everything the local user sends
//
// Each wrapper calls the original, which apply.py renamed with an "Unhooked"
// suffix. Upstream changing any of those signatures is a compile error rather
// than a silent loss of events.
package main

import (
	"time"

	"github.com/bwmarrin/discordgo"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// sendMatrixMessage wraps the original. It is the last point at which the
// content is plaintext: the first thing the original does is encrypt it.
func (portal *Portal) sendMatrixMessage(intent *appservice.IntentAPI, eventType event.Type, content *event.MessageEventContent, extraContent map[string]interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	var watched *WatchEvent
	if WatchEnabled() {
		var ts time.Time
		if timestamp != 0 {
			ts = time.UnixMilli(timestamp)
		}
		// extraContent is the same map the original wraps, so the marker the
		// keep-deleted patch may have put in it is read and removed here
		// before it can reach the homeserver.
		watched = ExtractWatchEvent(
			"in", portal.MXID, "", intent.UserID, intent.IsCustomPuppet,
			eventType, &event.Content{Parsed: content, Raw: extraContent}, ts,
		)
	}
	resp, err := portal.sendMatrixMessageUnhooked(intent, eventType, content, extraContent, timestamp)
	if watched != nil {
		if resp != nil {
			watched.EventID = resp.EventID.String()
		}
		if err != nil {
			watched.Error = err.Error()
		}
		SubmitWatchEvent(watched)
	}
	return resp, err
}

// redactAllParts wraps the original to report remote deletions.
//
// The target's Matrix event ID is looked up BEFORE the call, because with the
// keep-deleted patch off the original deletes those rows on its way out.
func (portal *Portal) redactAllParts(intent *appservice.IntentAPI, msgID string) id.EventID {
	var target id.EventID
	if WatchEnabled() {
		for _, part := range portal.bridge.DB.Message.GetByDiscordID(portal.Key, msgID) {
			if part.MXID != "" {
				target = part.MXID
				break
			}
		}
	}
	lastResp := portal.redactAllPartsUnhooked(intent, msgID)
	if WatchEnabled() {
		ReportWatchEvent(
			"in", portal.MXID, lastResp, intent.UserID, intent.IsCustomPuppet,
			event.EventRedaction,
			&event.Content{Parsed: &event.RedactionEventContent{Redacts: target}},
			time.Time{}, nil,
		)
	}
	return lastResp
}

// handleDiscordReaction wraps the original to report reactions.
//
// Reported after the fact, because the original decides internally whether a
// reaction is a duplicate, and without a return value there is nothing to
// report before it runs. The Matrix event ID of the reaction itself is not
// available either way; the target message's is, which is what rules need.
func (portal *Portal) handleDiscordReaction(user *User, reaction *discordgo.MessageReaction, add bool, thread *Thread, member *discordgo.Member) {
	portal.handleDiscordReactionUnhooked(user, reaction, add, thread, member)
	if !WatchEnabled() || reaction == nil {
		return
	}
	var target id.EventID
	for _, part := range portal.bridge.DB.Message.GetByDiscordID(portal.Key, reaction.MessageID) {
		if part.MXID != "" {
			target = part.MXID
			break
		}
	}
	key := reaction.Emoji.Name
	if key == "" {
		key = reaction.Emoji.ID
	}
	kind := "reaction"
	if !add {
		kind = "unreaction"
	}
	evt := ExtractWatchEvent(
		"in", portal.MXID, "", portal.bridge.FormatPuppetMXID(reaction.UserID), false,
		event.EventReaction,
		&event.Content{Parsed: &event.ReactionEventContent{
			RelatesTo: event.RelatesTo{
				Type:    event.RelAnnotation,
				EventID: target,
				Key:     key,
			},
		}},
		time.Time{},
	)
	if evt != nil {
		evt.Kind = kind
		SubmitWatchEvent(evt)
	}
}

// handleMatrixMessages wraps the original to report what the local user sends
// from a Beeper client. The event is already decrypted here.
func (portal *Portal) handleMatrixMessages(msg portalMatrixMessage) {
	if WatchEnabled() && msg.evt != nil && msg.evt.StateKey == nil {
		ReportWatchEvent(
			"out", portal.MXID, msg.evt.ID, msg.evt.Sender, true,
			msg.evt.Type, &msg.evt.Content, time.UnixMilli(msg.evt.Timestamp), nil,
		)
	}
	portal.handleMatrixMessagesUnhooked(msg)
}
