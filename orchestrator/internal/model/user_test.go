package model

import "testing"

// The rule an address has to satisfy, stated once because more than one place
// depends on it: the endpoint that refuses a bad one, and the personal edition,
// which seeds one nobody types.
func TestWhatCountsAsAnAddress(t *testing.T) {
	for _, ok := range []string{
		"somebody@example.com",
		"owner@localhost.fx", // what a personal installation seeds
		"a@b.c",
		"first.last+tag@sub.example.co.uk",
	} {
		if !ValidEmail(ok) {
			t.Errorf("%q was refused, and it is the kind of address people have", ok)
		}
	}
	for _, bad := range []string{
		"",
		"owner@localhost",       // no domain: the one that broke renaming yourself
		"nobody",                // no @ at all
		"@example.com",          // nothing before it
		"somebody@",             // nothing after it
		"some body@example.com", // a space somebody did not mean to type
		"somebody@exa mple.com",
	} {
		if ValidEmail(bad) {
			t.Errorf("%q was accepted, and it cannot be delivered to", bad)
		}
	}
}
