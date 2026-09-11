// Package ws is the bidirectional WebSocket surface: a hub that owns every
// live client connection, so any part of the server can push a notification to
// a person without polling, and a client can send messages back.
//
// It is complementary to the SSE chat stream, not a replacement: SSE carries
// one turn's answer (internal/run), this carries everything else. A connection
// authenticates with the same JWT the REST API uses, delivered in its first
// message because a browser cannot set an Authorization header on a socket.
package ws

import "encoding/json"

// Envelope is one message on the socket, in either direction. Type selects the
// shape; the rest is filled in as that type needs.
type Envelope struct {
	Type string `json:"type"`
	// Topic names a stream a client subscribes to (e.g. presence).
	Topic string `json:"topic,omitempty"`
	// Token carries the JWT on the first (authenticate) message only.
	Token string `json:"token,omitempty"`
	// Source says which app connected, on the authenticate message: "chat" or
	// "console". Only chat connections are counted as people online, so a
	// dashboard watching the count does not inflate it.
	Source string `json:"source,omitempty"`
	// Payload is the body of a notification or presence message.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Connection sources. Presence counts only the chat: a person is "online" when
// they are in the chat, not when an administrator is watching the dashboard.
const (
	SourceChat    = "chat"
	SourceConsole = "console"
)

const (
	// Inbound, client to server.
	TypeAuthenticate = "authenticate"
	TypeSubscribe    = "subscribe"
	TypeUnsubscribe  = "unsubscribe"

	// Outbound, server to client.
	TypeAuthenticated = "authenticated"
	TypeNotification  = "notification"
	// TypeTopic carries a topic payload pushed to that topic's subscribers (e.g.
	// the live dashboard). The client routes on Topic.
	TypeTopic = "topic"
)

// TopicDashboard streams the workspace's live picture (who is connected, how
// many turns are running, how many approvals wait) so the console dashboard
// shows it live. The payload is composed by the app's live-dashboard publisher;
// the hub only delivers it.
const TopicDashboard = "dashboard"

// Presence is the count of people connected to a workspace's chat: distinct
// people (Users) and the sockets they hold (Connections, since one person may
// have several tabs). It is a value the hub answers, not a wire frame; the
// publisher folds it into the dashboard payload.
type Presence struct {
	WorkspaceID int64 `json:"workspace_id"`
	Users       int   `json:"users"`
	Connections int   `json:"connections"`
}

// notificationFrame, topicFrame, and ackFrame pre-encode an outbound message
// once, so the write pump only copies bytes to the socket.
func notificationFrame(payload any) []byte {
	raw, _ := json.Marshal(payload)
	return frame(Envelope{Type: TypeNotification, Payload: raw})
}

// topicFrame encodes a payload for delivery to a topic's subscribers.
func topicFrame(topic string, payload any) []byte {
	raw, _ := json.Marshal(payload)
	return frame(Envelope{Type: TypeTopic, Topic: topic, Payload: raw})
}

func ackFrame() []byte {
	return frame(Envelope{Type: TypeAuthenticated})
}

func frame(e Envelope) []byte {
	raw, _ := json.Marshal(e)
	return raw
}
