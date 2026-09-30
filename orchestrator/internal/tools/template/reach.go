package template

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
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

// Ticked reads a checkbox.
//
// A declared setting arrives as a STRING whatever its type, because the form is
// one shape for all of them, so every template that offers a checkbox has to
// decide what counts as on. Two of them had decided it privately and
// identically; a third copy is how a rule stops being one rule. It lives here,
// with the field it reads, for the same reason tool.ReadPairs lives with the
// pairs field.
func Ticked(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// ReachChat reads the checkbox out of a stored configuration.
//
// IT IS ONE FIELD AND IT HAD TWO SPELLINGS, which is why this exists. The form
// names it with a dot ("reach.chat"), every template stores its settings by
// splitting dotted keys into nested objects, and so what lands in the database
// is {"reach":{"chat":"true"}}. Both templates that offered the checkbox then
// read it back as a FLAT key with a dot in its JSON name, which matches
// nothing: the value was always false, the box did nothing, and a database or a
// server on an office network was quietly dialled from this server instead of
// from the computer that can see it. The server tool was worse still, because
// it re-marshals a typed struct and so DROPPED the setting altogether.
//
// Nothing reported it because the failure looks like the thing it is for: an
// address that cannot be reached is exactly what somebody ticking this box
// already has.
//
// Both spellings are accepted. The nested one is what every template writes;
// the flat one is what a configuration assembled by hand or by a future path
// would hold, and a field with one meaning should not depend on which.
func ReachChat(config json.RawMessage) bool {
	var held struct {
		Nested struct {
			Chat any `json:"chat"`
		} `json:"reach"`
		Flat any `json:"reach.chat"`
	}
	if err := json.Unmarshal(config, &held); err != nil {
		return false
	}
	if held.Nested.Chat != nil {
		return Ticked(fmt.Sprint(held.Nested.Chat))
	}
	if held.Flat != nil {
		return Ticked(fmt.Sprint(held.Flat))
	}
	return false
}
