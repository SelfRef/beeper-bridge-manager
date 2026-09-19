// Report bridged events to beeper-watch, in plaintext, as they happen.
//
// Installed by patches/apply.py into BOTH bridge families, which is why the
// package clause is a placeholder it rewrites: mautrix-go's bridgev2 package
// for the nine bridgev2 bridges, and mautrix-discord's package main for
// Discord, which is still bridgev1 and shares no code with the others. The
// hooks that call into this differ per family and live in their own files;
// this is only the client.
//
// Why the bridge and not a Matrix client: Beeper has no event API of any kind
// — no webhooks, no gateway, and the Desktop API is request/response only —
// so the only way to react to a message as it arrives is to watch the events
// themselves, and portals are encrypted. Every bridged event passes through
// this process, BEFORE encryption, which is the one place in the system where
// the content is complete and readable without holding Matrix room keys.
//
// Hooks, by family and direction:
//
//	bridgev2 in   matrix.ASIntent.SendMessage
//	bridgev2 out  Portal.handleMatrixEvent
//	discord  in   Portal.sendMatrixMessage, redactAllParts, handleDiscordReaction
//	discord  out  Portal.handleMatrixMessages
//
// Everything is off unless BRIDGE_WATCH_URL is set; with it unset the hooks
// cost one nil check and never allocate.
//
//	BRIDGE_WATCH_URL        where to POST. Empty disables the whole thing
//	BRIDGE_WATCH_TOKEN      bearer token for that endpoint
//	BRIDGE_WATCH_NETWORK    network label (default: $BRIDGE_NAME minus "sh-")
//	BRIDGE_WATCH_BRIDGE     bridge label  (default: $BRIDGE_NAME)
//	BRIDGE_WATCH_OUTGOING   false to report only remote -> Matrix events
//	BRIDGE_WATCH_RAW_CONTENT true to include the full event content
//	BRIDGE_WATCH_MAX_BODY   truncate bodies to this many bytes (default 8192)
//	BRIDGE_WATCH_QUEUE      queued events before dropping (default 256)
//	BRIDGE_WATCH_WORKERS    concurrent POSTs (default 2)
//	BRIDGE_WATCH_TIMEOUT    per-POST timeout in seconds (default 5)
//	BRIDGE_WATCH_ATTEMPTS   POST attempts per event (default 3)
//
// The bridge is never blocked and never fails because of this: delivery is a
// bounded queue drained by background workers, a full queue drops the event
// with a warning, and nothing here can return an error into the bridge's own
// event handling.
package __WATCH_PACKAGE__

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// WatchEventSchema is the version of the payload below. beeper-watch rejects
// a payload it does not know, so bump this when a field changes meaning.
const WatchEventSchema = 1

// WatchKindKey marks an event the bridge is sending as a substitute for
// something else, so the watcher can report what actually happened rather
// than what was sent. The keep-deleted patch uses it: with that patch a
// remote deletion arrives as a reaction, and without this it would be
// indistinguishable from someone reacting with the same emoji.
//
// It is set in the content's raw map and REMOVED again by the reporter,
// before the event is sent, so it never reaches the homeserver.
const WatchKindKey = "dev.aperte.beeper_watch_kind"

// WatchEvent is the payload POSTed to beeper-watch. It is deliberately flat:
// the receiving end matches rules against these fields and forwards the whole
// object to n8n, so anything that needs a JSON path expression to reach would
// make every downstream workflow harder to write.
type WatchEvent struct {
	Schema int `json:"schema"`

	Bridge  string `json:"bridge"`  // sh-telegram
	Network string `json:"network"` // telegram

	// "in" = remote network -> Matrix, "out" = Matrix -> remote network.
	Direction string `json:"direction"`
	// message | edit | reaction | unreaction | deletion | other
	Kind string `json:"kind"`

	RoomID  string `json:"room_id"`
	EventID string `json:"event_id,omitempty"`

	Sender string `json:"sender"`
	// The remote network's own ID for the sender, parsed out of a ghost MXID
	// (@sh-telegram_1127943894:beeper.local -> 1127943894). Empty for the
	// local user and for anything that is not a ghost of this bridge.
	SenderRemoteID string `json:"sender_remote_id,omitempty"`
	// True when the event is the local user's own, from either direction.
	IsSelf bool `json:"is_self"`

	Timestamp int64  `json:"timestamp"` // ms, the remote timestamp when known
	Type      string `json:"type"`      // Matrix event type

	MsgType       string `json:"msgtype,omitempty"`
	Body          string `json:"body,omitempty"`
	FormattedBody string `json:"formatted_body,omitempty"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`

	ReactionKey string `json:"reaction_key,omitempty"`
	Redacts     string `json:"redacts,omitempty"`
	ReplyTo     string `json:"reply_to,omitempty"`
	ThreadRoot  string `json:"thread_root,omitempty"`
	Edits       string `json:"edits,omitempty"`

	Media *WatchMedia `json:"media,omitempty"`

	// The whole content, only when BRIDGE_WATCH_RAW_CONTENT is on.
	Content json.RawMessage `json:"content,omitempty"`

	// Set when the send this event describes failed. The event is still
	// reported, because "the bridge could not deliver X" is itself worth
	// reacting to.
	Error string `json:"error,omitempty"`
}

// WatchMedia describes an attachment without downloading it.
type WatchMedia struct {
	MimeType  string `json:"mimetype,omitempty"`
	Size      int    `json:"size,omitempty"`
	FileName  string `json:"filename,omitempty"`
	URL       string `json:"url,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

type watchConfig struct {
	url        string
	token      string
	bridge     string
	network    string
	outgoing   bool
	rawContent bool
	maxBody    int
	attempts   int
	client     *http.Client
	queue      chan *WatchEvent
	log        zerolog.Logger
}

// watcher is nil unless BRIDGE_WATCH_URL is set, which is the off switch for
// every hook: one nil comparison on a path that runs for every message.
var watcher = initWatcher()

func initWatcher() *watchConfig {
	url := strings.TrimSpace(os.Getenv("BRIDGE_WATCH_URL"))
	if url == "" {
		return nil
	}
	// BRIDGE_NAME is what bbctl was invoked with (sh-telegram); the bridge
	// binary inherits it, so neither label has to be configured per bridge.
	bridge := watchEnvDefault("BRIDGE_WATCH_BRIDGE", os.Getenv("BRIDGE_NAME"))
	network := watchEnvDefault("BRIDGE_WATCH_NETWORK", strings.TrimPrefix(bridge, "sh-"))
	if network == "" {
		network = os.Getenv("BEEPER_BRIDGE_TYPE")
	}
	w := &watchConfig{
		url:        url,
		token:      os.Getenv("BRIDGE_WATCH_TOKEN"),
		bridge:     bridge,
		network:    network,
		outgoing:   watchEnvBool("BRIDGE_WATCH_OUTGOING", true),
		rawContent: watchEnvBool("BRIDGE_WATCH_RAW_CONTENT", false),
		maxBody:    watchEnvInt("BRIDGE_WATCH_MAX_BODY", 8192),
		attempts:   watchEnvInt("BRIDGE_WATCH_ATTEMPTS", 3),
		client:     &http.Client{Timeout: time.Duration(watchEnvInt("BRIDGE_WATCH_TIMEOUT", 5)) * time.Second},
		queue:      make(chan *WatchEvent, watchEnvInt("BRIDGE_WATCH_QUEUE", 256)),
		log: zerolog.New(os.Stderr).With().
			Timestamp().
			Str("component", "beeper-watch").
			Logger(),
	}
	for i := 0; i < watchEnvInt("BRIDGE_WATCH_WORKERS", 2); i++ {
		go w.worker()
	}
	w.log.Info().
		Str("url", w.url).
		Str("bridge", w.bridge).
		Str("network", w.network).
		Bool("outgoing", w.outgoing).
		Msg("Reporting bridged events to beeper-watch")
	return w
}

func watchEnvDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func watchEnvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func watchEnvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

// worker drains the queue. Retries are few and short: beeper-watch owns
// durability (it has a disk-backed outbox), so the only failure this needs to
// survive is the watcher being restarted underneath a running bridge.
func (w *watchConfig) worker() {
	for evt := range w.queue {
		w.deliver(evt)
	}
}

func (w *watchConfig) deliver(evt *WatchEvent) {
	body, err := json.Marshal(evt)
	if err != nil {
		w.log.Err(err).Msg("Failed to encode event for beeper-watch")
		return
	}
	for attempt := 1; attempt <= w.attempts; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt-1) * time.Second)
		}
		req, err := http.NewRequest(http.MethodPost, w.url, bytes.NewReader(body))
		if err != nil {
			w.log.Err(err).Msg("Failed to build beeper-watch request")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if w.token != "" {
			req.Header.Set("Authorization", "Bearer "+w.token)
		}
		resp, err := w.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
			// 4xx is a contract problem; retrying an identical body cannot fix it.
			if resp.StatusCode < 500 {
				w.log.Warn().
					Int("status", resp.StatusCode).
					Str("event_id", evt.EventID).
					Msg("beeper-watch rejected an event")
				return
			}
			err = errWatchStatus(resp.StatusCode)
		}
		if attempt == w.attempts {
			w.log.Warn().Err(err).
				Str("event_id", evt.EventID).
				Int("attempts", attempt).
				Msg("Giving up on delivering an event to beeper-watch")
		}
	}
}

type errWatchStatus int

func (e errWatchStatus) Error() string { return "HTTP " + strconv.Itoa(int(e)) }

// submit hands the event to the workers, or drops it. Dropping is the right
// failure: blocking here would back up into the bridge's event handling, and
// a watcher that is down is not a reason to stop bridging messages.
func (w *watchConfig) submit(evt *WatchEvent) {
	select {
	case w.queue <- evt:
	default:
		w.log.Warn().
			Str("event_id", evt.EventID).
			Msg("beeper-watch queue is full, dropping event")
	}
}

// watchSkippedTypes are bridge bookkeeping rather than conversation. They pass
// through the same send path as messages, and reporting them would mean every
// rule in the watcher needs to exclude them.
var watchSkippedTypes = map[string]bool{
	"com.beeper.message_send_status": true,
	"fi.mau.dummy.portal_created":    true,
	"m.room.encrypted":               true, // already encrypted: nothing to read
}

// WatchEnabled reports whether anything is listening. Hooks check this first
// so that with the feature off they allocate nothing at all.
func WatchEnabled() bool { return watcher != nil }

// ExtractWatchEvent builds the payload from content that is about to be sent.
// It must run on the caller's goroutine, before the send: the content it
// reads is encrypted in place further down, so a deferred read would find
// ciphertext. It returns nil when there is nothing to report.
//
// It is exported because the incoming hook lives in the matrix package, which
// imports this one; the reverse would be an import cycle.
func ExtractWatchEvent(
	direction string,
	roomID id.RoomID,
	eventID id.EventID,
	sender id.UserID,
	isSelf bool,
	eventType event.Type,
	content *event.Content,
	ts time.Time,
) *WatchEvent {
	w := watcher
	if w == nil || content == nil {
		return nil
	}
	// Read and clear the substitution marker even when the type is skipped,
	// so it can never leak onto the wire.
	kindOverride := watchTakeKindOverride(content)
	if watchSkippedTypes[eventType.Type] {
		return nil
	}
	if direction == "out" && !w.outgoing {
		return nil
	}
	evt := &WatchEvent{
		Schema:    WatchEventSchema,
		Bridge:    w.bridge,
		Network:   w.network,
		Direction: direction,
		RoomID:    roomID.String(),
		EventID:   eventID.String(),
		Sender:    sender.String(),
		IsSelf:    isSelf,
		Type:      eventType.Type,
	}
	if !ts.IsZero() {
		evt.Timestamp = ts.UnixMilli()
	} else {
		evt.Timestamp = time.Now().UnixMilli()
	}
	evt.SenderRemoteID = watchRemoteID(w.bridge, sender)
	w.fill(evt, eventType, content)
	if kindOverride != "" {
		evt.Kind = kindOverride
	}
	return evt
}

// SubmitWatchEvent queues a payload from ExtractWatchEvent for delivery.
func SubmitWatchEvent(evt *WatchEvent) {
	w := watcher
	if w == nil || evt == nil {
		return
	}
	w.submit(evt)
}

// ReportWatchEvent is ExtractWatchEvent plus SubmitWatchEvent, for callers
// that already know the outcome.
func ReportWatchEvent(
	direction string,
	roomID id.RoomID,
	eventID id.EventID,
	sender id.UserID,
	isSelf bool,
	eventType event.Type,
	content *event.Content,
	ts time.Time,
	sendErr error,
) {
	evt := ExtractWatchEvent(direction, roomID, eventID, sender, isSelf, eventType, content, ts)
	if evt == nil {
		return
	}
	if sendErr != nil {
		evt.Error = sendErr.Error()
	}
	SubmitWatchEvent(evt)
}

// watchTakeKindOverride reads and deletes WatchKindKey from the raw content.
func watchTakeKindOverride(content *event.Content) string {
	if content.Raw == nil {
		return ""
	}
	raw, ok := content.Raw[WatchKindKey]
	if !ok {
		return ""
	}
	delete(content.Raw, WatchKindKey)
	kind, _ := raw.(string)
	return kind
}

// watchRemoteID pulls the network's own user ID out of a ghost MXID. Ghosts
// are @<bridge>_<remote id>:beeper.local, so anything without that prefix is
// the local user or another bridge's ghost and has no remote ID here.
func watchRemoteID(bridge string, sender id.UserID) string {
	localpart := sender.String()
	if !strings.HasPrefix(localpart, "@") {
		return ""
	}
	localpart = localpart[1:]
	if idx := strings.IndexByte(localpart, ':'); idx >= 0 {
		localpart = localpart[:idx]
	}
	prefix := bridge + "_"
	if bridge == "" || !strings.HasPrefix(localpart, prefix) {
		return ""
	}
	return strings.TrimPrefix(localpart, prefix)
}

// fill classifies the event and copies the fields rules match on.
func (w *watchConfig) fill(evt *WatchEvent, eventType event.Type, content *event.Content) {
	evt.Kind = "other"
	if w.rawContent {
		if raw, err := json.Marshal(watchContentValue(content)); err == nil {
			evt.Content = raw
		}
	}
	switch eventType.Type {
	case event.EventReaction.Type:
		evt.Kind = "reaction"
		if parsed, ok := content.Parsed.(*event.ReactionEventContent); ok {
			evt.ReactionKey = parsed.RelatesTo.Key
			evt.Redacts = parsed.RelatesTo.EventID.String()
		} else {
			rel := watchRawMap(content, "m.relates_to")
			evt.ReactionKey, _ = rel["key"].(string)
			evt.Redacts, _ = rel["event_id"].(string)
		}
	case event.EventRedaction.Type:
		evt.Kind = "deletion"
		if parsed, ok := content.Parsed.(*event.RedactionEventContent); ok {
			evt.Redacts = parsed.Redacts.String()
		} else if raw, ok := content.Raw["redacts"].(string); ok {
			evt.Redacts = raw
		}
	case event.EventMessage.Type, event.EventSticker.Type:
		evt.Kind = "message"
		w.fillMessage(evt, content)
	}
}

func (w *watchConfig) fillMessage(evt *WatchEvent, content *event.Content) {
	msg, ok := content.Parsed.(*event.MessageEventContent)
	if !ok {
		// Unparsed content still carries everything a text rule needs.
		evt.MsgType, _ = content.Raw["msgtype"].(string)
		body, _ := content.Raw["body"].(string)
		evt.Body, evt.BodyTruncated = w.truncate(body)
		formatted, _ := content.Raw["formatted_body"].(string)
		evt.FormattedBody, _ = w.truncate(formatted)
		return
	}
	evt.MsgType = string(msg.MsgType)
	body := msg.Body
	formatted := msg.FormattedBody
	// An edit's real text is in m.new_content; the top-level body is the
	// "* new text" fallback, which no rule should be matching against.
	if msg.NewContent != nil {
		body = msg.NewContent.Body
		formatted = msg.NewContent.FormattedBody
	}
	evt.Body, evt.BodyTruncated = w.truncate(body)
	evt.FormattedBody, _ = w.truncate(formatted)
	if msg.RelatesTo != nil {
		switch msg.RelatesTo.Type {
		case event.RelReplace:
			evt.Kind = "edit"
			evt.Edits = msg.RelatesTo.EventID.String()
		case event.RelThread:
			evt.ThreadRoot = msg.RelatesTo.EventID.String()
		}
		if msg.RelatesTo.InReplyTo != nil {
			evt.ReplyTo = msg.RelatesTo.InReplyTo.EventID.String()
		}
	}
	if msg.URL != "" || msg.File != nil || msg.Info != nil {
		media := &WatchMedia{FileName: msg.FileName}
		if msg.Info != nil {
			media.MimeType = msg.Info.MimeType
			media.Size = msg.Info.Size
		}
		if msg.File != nil {
			media.URL = string(msg.File.URL)
			media.Encrypted = true
		} else if msg.URL != "" {
			media.URL = string(msg.URL)
		}
		if media.URL != "" || media.MimeType != "" {
			evt.Media = media
		}
	}
}

func (w *watchConfig) truncate(s string) (string, bool) {
	if w.maxBody > 0 && len(s) > w.maxBody {
		return s[:w.maxBody], true
	}
	return s, false
}

// watchContentValue prefers the parsed struct (it is the complete, typed
// version) and falls back to the raw map.
func watchContentValue(content *event.Content) any {
	if content.Parsed != nil {
		return content.Parsed
	}
	return content.Raw
}

func watchRawMap(content *event.Content, key string) map[string]any {
	if content.Raw == nil {
		return nil
	}
	m, _ := content.Raw[key].(map[string]any)
	return m
}

// WatchMarkKind tags a content as a substitute for something else, so the
// reporter can label it correctly. See WatchKindKey.
func WatchMarkKind(content *event.Content, kind string) {
	if watcher == nil || content == nil {
		return
	}
	if content.Raw == nil {
		content.Raw = make(map[string]any)
	}
	content.Raw[WatchKindKey] = kind
}
