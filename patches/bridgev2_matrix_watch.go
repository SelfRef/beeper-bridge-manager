// The incoming half of the beeper-watch hook: remote network -> Matrix.
//
// Injected into mautrix-go's bridgev2/matrix package by patches/apply.py. The
// companion file bridgev2_watch.go (package bridgev2) holds the client and
// explains the design; this file is separate only because ASIntent lives here
// and this package imports bridgev2, not the other way round.
//
// ASIntent.SendMessage is the single point every bridged event passes through
// on its way to the homeserver — messages, edits, reactions, redactions and
// stickers alike — and it is where the content is still plaintext: the
// encryption step is inside the function being wrapped. That is what makes
// content rules possible without holding Matrix room keys.
//
// Wrapping rather than injecting a line has one specific reason: the function
// has two exits (a redaction sent through the redact endpoint, and everything
// else), and a wrapper reports both without patching either.
package matrix

import (
	"context"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// SendMessage wraps the original, which patches/apply.py renamed to
// sendMessageUnhooked. Reporting is split around the call: the plaintext is
// read before, because the content is encrypted in place, and the result is
// attached after, because the event ID only exists once the send succeeded.
func (as *ASIntent) SendMessage(ctx context.Context, roomID id.RoomID, eventType event.Type, content *event.Content, extra *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	watched := as.watchExtract(roomID, eventType, content, extra)
	resp, err := as.sendMessageUnhooked(ctx, roomID, eventType, content, extra)
	if watched != nil {
		if resp != nil {
			watched.EventID = resp.EventID.String()
		}
		if err != nil {
			watched.Error = err.Error()
		}
		bridgev2.SubmitWatchEvent(watched)
	}
	return resp, err
}

// watchExtract snapshots the event while it is still readable, or returns nil
// when nothing is listening.
func (as *ASIntent) watchExtract(roomID id.RoomID, eventType event.Type, content *event.Content, extra *bridgev2.MatrixSendExtra) *bridgev2.WatchEvent {
	if !bridgev2.WatchEnabled() {
		return nil
	}
	var ts time.Time
	if extra != nil {
		ts = extra.Timestamp
	}
	// A custom puppet is the local user's own Matrix account, so the bridge
	// is echoing back something the user did on the network itself rather
	// than relaying another party's message.
	return bridgev2.ExtractWatchEvent(
		"in", roomID, "", as.Matrix.UserID, as.Matrix.IsCustomPuppet, eventType, content, ts,
	)
}
