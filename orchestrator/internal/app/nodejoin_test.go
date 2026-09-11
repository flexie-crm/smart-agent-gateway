package app

import (
	"encoding/hex"
	"strings"
	"testing"
)

// The two ends of a join are written in different languages, so the things they
// must agree on are asserted here as VALUES rather than as behaviour. A test
// that runs our own code to work out what to expect cannot catch the two sides
// drifting apart, which is the only failure worth guarding against: it would
// show up as a machine that simply cannot register, with a refusal that says
// nothing on purpose.

func TestTheSignatureIsTheOneTheMachineWillCompute(t *testing.T) {
	// Computed with neither side's code. The machine asserts the same number
	// against its own implementation.
	const (
		token = "abc_secret"
		body  = `{"node_id":"nd_1"}`
		want  = "0b8a9318c0a506d45b715d44549c90f095e9c86fefec3ba1ce8d235ae9ee5f47"
	)
	if got := hex.EncodeToString(joinSignature([]byte(token), []byte(body))); got != want {
		t.Errorf("signature = %s, want %s", got, want)
	}
}

func TestASignatureCoversEveryByteOfTheRequest(t *testing.T) {
	// Which is what makes it an authentication of the REQUEST and not merely of
	// the sender: an intermediary that can change a byte can redirect a machine.
	const token = "abc_secret"
	first := joinSignature([]byte(token), []byte(`{"node_id":"nd_1"}`))
	for _, altered := range []string{
		`{"node_id":"nd_2"}`,
		`{"node_id":"nd_1" }`,
		`{"node_id":"nd_1"} `,
		``,
	} {
		if hex.EncodeToString(joinSignature([]byte(token), []byte(altered))) ==
			hex.EncodeToString(first) {
			t.Errorf("%q signed the same as the original", altered)
		}
	}
	// And a different token is a different signature, which is the whole point.
	if hex.EncodeToString(joinSignature([]byte("abc_other"), []byte(`{"node_id":"nd_1"}`))) ==
		hex.EncodeToString(first) {
		t.Error("two different tokens produced one signature")
	}
}

func TestTheTokenIsReadTheSameWayOnBothSides(t *testing.T) {
	// The machine splits this string to find out who it is about to talk to. If
	// the two disagree about where the fingerprint ends, every join fails at the
	// one check that has no other way to be diagnosed.
	cases := map[string]string{
		"abc123_secret": "abc123",
		// The secret is base64url and may contain underscores; the fingerprint
		// is hex and cannot, so the FIRST underscore is the boundary.
		"abc123_se_cr_et": "abc123",
	}
	for token, want := range cases {
		if got := fingerprintIn(token); got != want {
			t.Errorf("fingerprintIn(%q) = %q, want %q", token, got, want)
		}
	}

	for _, notOne := range []string{
		"",
		"hello",
		"_",
		"abc123",
		"_secret",
		"abc123_",
	} {
		if got := fingerprintIn(notOne); got != "" {
			t.Errorf("fingerprintIn(%q) = %q, want nothing", notOne, got)
		}
	}
}

func TestAMachineIsOnlyEverRegisteredAtAnEncryptedAddress(t *testing.T) {
	// There is no unencrypted way to reach a machine, so a row pointing at a
	// plain address is a row nothing could use. Refusing here names the mistake;
	// storing it turns a configuration slip into a connection failure days later.
	if _, err := nodeBaseURL("http://10.0.0.9:19443", "", 0); err == nil {
		t.Error("a plain address was accepted")
	}
	if _, err := nodeBaseURL("ws://10.0.0.9:19443", "", 0); err == nil {
		t.Error("an address that is not a web address was accepted")
	}
}

func TestAMachineNeedsNobodyToTypeItsAddress(t *testing.T) {
	// The whole reason nothing but --url and --token is pasted onto a machine.
	//
	// Two halves and each side has exactly one of them. The CONNECTION tells us
	// the host it came from, which is the address that reaches the machine from
	// here rather than the one it believes it has. The MACHINE tells us the port
	// it listens on, which the connection cannot: a request's source port is
	// ephemeral and belongs to that one connection.
	got, err := nodeBaseURL("", "10.0.0.9", 19443)
	if err != nil {
		t.Fatalf("nodeBaseURL: %v", err)
	}
	if got != "https://10.0.0.9:19443/v1" {
		t.Errorf("base url = %q, want the address we saw on the port it declared", got)
	}

	// It used to take the host on its own and then refuse it for having no port,
	// so an address WAS mandatory while being documented as optional. A machine
	// that declares its port must never hit that path again.
	if _, err := nodeBaseURL("", "10.0.0.9", 0); err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("a machine that declared no port gave %v", err)
	}
	if _, err := nodeBaseURL("", "", 19443); err == nil {
		t.Error("a machine we saw no address for was accepted")
	}

	// An address given by hand still wins, for the case it is for: a port
	// forward, where the port we should dial is not the port it listens on.
	got, err = nodeBaseURL("https://gpu1.example.com:9000", "10.0.0.9", 19443)
	if err != nil {
		t.Fatalf("nodeBaseURL: %v", err)
	}
	if got != "https://gpu1.example.com:9000/v1" {
		t.Errorf("an explicit address was overridden: %q", got)
	}
}

func TestTheCertificateCarriesTheHostWeWillDial(t *testing.T) {
	// Not what identity rests on (every machine answers to one fleet name), but
	// it is what a person reading a certificate wants to see, and what any tool
	// that insists on matching the address it dialled will look for.
	if got := addressesOf("https://10.0.0.9:8081/v1"); len(got) != 1 || got[0] != "10.0.0.9" {
		t.Errorf("addresses = %v", got)
	}
	if got := addressesOf("nonsense"); len(got) != 0 {
		t.Errorf("addresses = %v, want none", got)
	}
}
