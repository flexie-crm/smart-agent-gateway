package apitool

import (
	"fmt"
	"sort"
	"strings"
)

// Policy is which HTTP verbs this tool may use, read as an allowlist or a
// denylist. The same shape the query tool's policy has, for the same reason: an
// administrator decides what a tool may do, and the decision is enforced by
// code rather than by asking the model nicely.
//
// The verb is the whole of it, deliberately. What a path may be reached is the
// API's own business and cannot be known from here; what a verb DOES is
// universal, and the line people actually want to draw is between reading and
// changing.
//
// Approval is not this tool's concern. Whether a call stops for a person is the
// agent's setting, and a policy that also asked would be two answers to one
// question.
type Policy struct {
	Mode  string   `json:"mode"`
	Verbs []string `json:"verbs,omitempty"`
}

// The two readings.
const (
	Allowlist = "allowlist"
	Denylist  = "denylist"
)

// WriteVerbs are the verbs that change something. Not the default (see
// DefaultDeniedVerbs), but still the honest answer to "which of these writes",
// and what a policy denying every write is built from.
func WriteVerbs() []string { return []string{"POST", "PUT", "PATCH", "DELETE"} }

// DefaultDeniedVerbs is what a NEW tool starts denying: DELETE and nothing
// else.
//
// The other three writes are the ordinary business of using an API. Creating a
// record, updating one, patching a field: an assistant that cannot do any of
// them is a read-only tool, and somebody who wanted that would have said so.
// DELETE is the one that is different in kind, because it is the one whose
// mistake cannot be undone by making the opposite call.
//
// It is a DEFAULT and not a rule. The field is right there and an administrator
// who wants the reading-only tool adds the other three, or switches to an
// allowlist and names what they want.
func DefaultDeniedVerbs() []string { return []string{"DELETE"} }

// KnownVerbs is every verb this tool will send. HEAD and OPTIONS are here
// because an API client legitimately uses them and they change nothing.
func KnownVerbs() []string {
	return []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE"}
}

func (p *Policy) check() error {
	p.Mode = strings.TrimSpace(strings.ToLower(p.Mode))
	if p.Mode == "" {
		p.Mode = Denylist
	}
	if p.Mode != Allowlist && p.Mode != Denylist {
		return fmt.Errorf("the rule must be an allowlist or a denylist, and %q is neither", p.Mode)
	}
	cleaned := make([]string, 0, len(p.Verbs))
	seen := map[string]bool{}
	for _, v := range p.Verbs {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v == "" || seen[v] {
			continue
		}
		if !known(v) {
			return fmt.Errorf("%q is not an HTTP verb this tool sends. Use one of %s, one per line",
				v, strings.Join(KnownVerbs(), ", "))
		}
		seen[v] = true
		cleaned = append(cleaned, v)
	}
	sort.Strings(cleaned)
	p.Verbs = cleaned
	// An allowlist with nothing in it permits nothing, which is a tool that
	// cannot do anything at all. That is the mistake the SSH tool shipped with
	// (KB/33), so it is refused here instead of saved and puzzled over.
	if p.Mode == Allowlist && len(p.Verbs) == 0 {
		return fmt.Errorf("an allowlist with no verbs in it permits nothing, so this tool could never make a call. List the verbs it may use, or change the rule to a denylist")
	}
	return nil
}

func known(verb string) bool {
	for _, v := range KnownVerbs() {
		if v == verb {
			return true
		}
	}
	return false
}

// Permits says whether the policy lets this verb through, and why not when it
// does not. The reason is model-safe text: it says what was refused and what is
// allowed, so the model can choose a different call rather than retry the same
// one.
func (p Policy) Permits(verb string) (bool, string) {
	verb = strings.ToUpper(strings.TrimSpace(verb))
	if !known(verb) {
		return false, fmt.Sprintf("%q is not an HTTP verb this tool sends; it sends %s",
			verb, strings.Join(KnownVerbs(), ", "))
	}
	listed := false
	for _, v := range p.Verbs {
		if v == verb {
			listed = true
			break
		}
	}
	switch p.Mode {
	case Allowlist:
		if listed {
			return true, ""
		}
		return false, fmt.Sprintf("this tool may only use %s, so %s is not allowed",
			strings.Join(p.Verbs, ", "), verb)
	default:
		if !listed {
			return true, ""
		}
		return false, fmt.Sprintf("this tool is not allowed to use %s", verb)
	}
}
