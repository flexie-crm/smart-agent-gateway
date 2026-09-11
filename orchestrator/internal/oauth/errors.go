package oauth

import "fmt"

// Error is an RFC 6749 protocol error. It is one of the few error types
// whose message is deliberately shown to external clients, everything
// else must surface as a generic server_error (CRM error-hygiene rule).
type Error struct {
	Code        string // invalid_request, invalid_client, invalid_grant, ...
	Description string
	Status      int // suggested HTTP status
}

func (e *Error) Error() string {
	return fmt.Sprintf("oauth %s: %s", e.Code, e.Description)
}

func protoErr(status int, code, description string) *Error {
	return &Error{Code: code, Description: description, Status: status}
}
