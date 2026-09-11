package api

import "testing"

// What a deployment will hand to anybody who asks.
//
// This route is unauthenticated by design (the token admits a machine, the
// binary admits nobody), which makes the name check the whole of the policy and
// worth stating rather than trusting.
func TestOnlyOurOwnArtefactsAreServed(t *testing.T) {
	for _, name := range []string{
		"install.sh",
		"sag-inference-linux-x86_64-cuda.tar.gz",
		"sag-inference-linux-x86_64-cpu.tar.gz",
		"sag-inference-linux-aarch64-cuda.tar.gz",
		"sag-inference-linux-x86_64-cuda.tar.gz.sha256",
	} {
		if !publishedArtefact(name) {
			t.Errorf("%q is one of ours and was refused", name)
		}
	}
}

func TestNothingElseOnThatDiskIsServed(t *testing.T) {
	refused := map[string]string{
		"climbing out":             "../../etc/passwd",
		"climbing out, encoded":    "..%2f..%2fetc%2fpasswd",
		"an absolute path":         "/etc/passwd",
		"a windows separator":      `..\..\windows\system32`,
		"a dot segment alone":      "..",
		"nothing at all":           "",
		"a neighbour's file":       "node.env",
		"the right prefix, wrong":  "sag-inference-linux-x86_64-cuda.tar.gz/../../etc/passwd",
		"a shell that is not ours": "installer.sh",
		"a directory":              "sag-inference-linux-x86_64",
	}
	for what, name := range refused {
		if publishedArtefact(name) {
			t.Errorf("%s: %q was accepted", what, name)
		}
	}
}
