// The bridgev2 half of the beeper-watch hooks: Matrix -> remote network.
//
// Injected into mautrix-go's bridgev2 package by patches/apply.py, alongside
// watch_client.go, which holds the client and the design notes. The incoming
// direction is hooked one package down, in bridgev2/matrix, because that is
// where ASIntent lives.
package bridgev2

import (
	"context"
	"time"

	"maunium.net/go/mautrix/event"
)

// handleMatrixEvent wraps the original (renamed by patches/apply.py) to report
// what the local user sends. Reporting happens BEFORE the handler runs, so a
// rule fires on a message whether or not the remote network accepts it —
// delivery failure is reported separately by the bridge itself.
func (portal *Portal) handleMatrixEvent(ctx context.Context, sender *User, evt *event.Event, isStateRequest bool) EventHandlingResult {
	portal.reportWatchMatrixEvent(sender, evt)
	return portal.handleMatrixEventUnhooked(ctx, sender, evt, isStateRequest)
}

func (portal *Portal) reportWatchMatrixEvent(sender *User, evt *event.Event) {
	if watcher == nil || evt == nil || sender == nil {
		return
	}
	// Ephemeral events (typing, receipts) share this entry point and are not
	// conversation. State events are not either.
	if evt.Mautrix.EventSource&event.SourceEphemeral != 0 || evt.StateKey != nil {
		return
	}
	ts := time.UnixMilli(evt.Timestamp)
	ReportWatchEvent(
		"out",
		evt.RoomID,
		evt.ID,
		evt.Sender,
		true, // outgoing means the local user sent it
		evt.Type,
		&evt.Content,
		ts,
		nil,
	)
}
