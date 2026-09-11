package template

import (
	"context"
	"net"
)

// Reaching what the gateway cannot.
//
// A tool is configured with an address, and some of those addresses only exist
// on somebody else's network: the accounts database on a machine in an office,
// a server on an isolated network. The chat application is installed on a
// computer that CAN see them, and it is already connected to us, so it carries
// the connection.
//
// It is one checkbox because it needs no more than one. The tool already says
// where to connect; this says from where.

// ReachChatKey is the setting, on every template that opens a connection. One
// key, so the console, the config and the tools all mean the same thing by it.
const ReachChatKey = "reach.chat"

// ReachChatField is the checkbox itself. It belongs beside the address, in the
// connection section, because it is part of the answer to "where is this".
//
// thing is what the tool connects to, in the reader's words: a database, a
// server. The sentence is for somebody deciding whether this applies to them,
// so it describes their situation rather than our mechanism.
func ReachChatField(thing string) Field {
	return Field{
		Key:   ReachChatKey,
		Label: "Proxy through Chat UI for local reach",
		Type:  FieldCheckbox,
		// The whole row. A checkbox is its label plus a line explaining it, and
		// at half width that line wraps into a narrow column beside empty space.
		Span: 6,
		Help: "Tick this for a " + thing + " on a local network that cannot be reached from the internet, " +
			"such as one in an office. The connection is made from the computer of whoever is using the tool.",
	}
}

// Machines is the chat applications connected right now, as a template needs
// them: something to dial through, and a way to ask whether anybody is there.
//
// An interface, and a small one, so a template depends on the capability rather
// than on the package that implements it, and a test can supply a pair of pipes.
type Machines interface {
	// Dial opens a connection to host:port through the chat application the
	// call came from. Reason is what the person is told it is for.
	Dial(ctx context.Context, workspaceID, userID int64, deviceID, host string, port int, reason string) (net.Conn, error)
	// Online reports whether that installation is connected, so a Test can say
	// so before an administrator saves a tool that cannot work.
	Online(workspaceID, userID int64, deviceID string) bool
}
