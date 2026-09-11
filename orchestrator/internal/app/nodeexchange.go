package app

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"time"

	"flexie.io/sag/internal/model"
)

// Adding a machine by swapping certificates, in both directions, by hand.
//
// Joining (nodejoin.go) is the good path and is used wherever the machine can
// open a connection to us. It cannot on the personal edition, whose gateway sits
// on a laptop behind a router, nor on any deployment inside a network the
// machine is outside of. There, the exchange the join performs in one request
// is done by a person, in two halves, and it is worth being exact about what
// each half is for:
//
//	OURS, going out    the certificate a caller must present to that machine.
//	                   It is what makes the machine refuse everybody else, and
//	                   it is public: it proves nothing on its own, since holding
//	                   it does not give anybody our private key.
//
//	THEIRS, coming back the certificate the machine generated over a key that
//	                   never left it. We keep the bytes and accept nothing else,
//	                   which is what makes us refuse an impostor at that address.
//
// Both ends therefore hold the other's certificate and nothing wider. That is
// mutual TLS in the strict sense, and it is NARROWER than the fleet arrangement
// rather than a weakening of it: a machine that joined trusts any client our
// authority signed, while one added this way trusts exactly one certificate.
//
// The machine's private key never travels. An earlier cut of this minted the
// key here and shipped it in a bundle, which is one paste instead of two and
// puts a private key in a clipboard. This does not.

// MachineCertificate is what a person pastes into the installer.
//
// It is the AUTHORITY's certificate rather than the client certificate we
// actually dial with, and the difference matters: client certificates are minted
// per process and rotate, so pinning one on the machine would mean a restart
// here locking us out of every machine in the fleet. The authority is stable for
// ten years and is what every client of ours chains to.
// It also mints the KEY the two ends will use, and that is not an extra: a node
// refuses any caller whose bearer token does not match the one in its identity
// file (inference/src/api/auth.rs), which is a second lock that revokes on a
// different clock from the certificate. A joining machine mints that key and
// tells us. Here nobody tells anybody, so it was minted twice, once at each end,
// and every call was refused with "Unauthorised" after a perfectly good TLS
// handshake. It goes out with the certificate and comes back with the address.
func (a *App) MachineCertificate(ctx context.Context) (certPEM, key string, err error) {
	authority, err := a.Authority(ctx)
	if err != nil {
		return "", "", err
	}
	key, err = mintNodeKey()
	if err != nil {
		return "", "", err
	}
	return string(authority.CertPEM()), key, nil
}

// AddPinnedMachine records a machine we can now reach.
//
// Everything is validated BEFORE the row is written, because this is the point
// at which somebody has already done the work: they installed the node, copied
// two things out of a terminal, and pressed a button. A refusal here has to be
// about what they pasted, and it has to be specific enough to fix.
func (a *App) AddPinnedMachine(ctx context.Context, name, address, certPEM, key string) (*model.InferenceNode, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("that machine needs a name")
	}
	baseURL, err := machineURL(address)
	if err != nil {
		return nil, err
	}
	cert, err := readPastedCertificate(certPEM)
	if err != nil {
		return nil, err
	}

	// The node id is ours to mint here. The machine has one of its own on disk
	// and never tells us, because it never calls: nothing in this arrangement
	// asks it to identify itself by name, since the certificate does that.
	nodeID, err := mintNodeID()
	if err != nil {
		return nil, err
	}
	// The key the machine was given when the command was generated. Minting one
	// here instead is what made every call fail: the machine has the other one.
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("that command is from an older version of this screen. " +
			"Close this window, open it again, and use the new command")
	}
	sealedKey, err := a.SealCredentials(key)
	if err != nil {
		return nil, err
	}

	pinned := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
	expires := cert.NotAfter
	node := &model.InferenceNode{
		NodeID:        nodeID,
		Name:          name,
		BaseURL:       baseURL,
		Key:           sealedKey,
		CertExpiresAt: &expires,
		PinnedCert:    &pinned,
	}
	if err := a.Store.Nodes().Create(ctx, node); err != nil {
		return nil, err
	}
	a.Log.Info().Str("node", nodeID).Str("name", name).Str("at", baseURL).
		Time("certificate expires", expires).Msg("machine added by certificate")
	return node, nil
}

// readPastedCertificate turns what somebody copied out of a terminal into a
// certificate, or says exactly what is wrong with it.
//
// Every refusal here is a thing a person can act on, which is the whole reason
// this is not one `if err != nil`. Pasting the wrong half of a terminal is the
// normal mistake, not the exceptional one: the private key sits directly above
// the certificate in the installer's output.
func readPastedCertificate(pasted string) (*x509.Certificate, error) {
	trimmed := strings.TrimSpace(pasted)
	if trimmed == "" {
		return nil, fmt.Errorf("paste the certificate the installer printed")
	}
	block, rest := pem.Decode([]byte(trimmed))
	if block == nil {
		return nil, fmt.Errorf("that does not look like a certificate. It starts with " +
			"-----BEGIN CERTIFICATE----- and ends with -----END CERTIFICATE-----")
	}
	if block.Type == "EC PRIVATE KEY" || block.Type == "PRIVATE KEY" || block.Type == "RSA PRIVATE KEY" {
		return nil, fmt.Errorf("that is the machine's PRIVATE KEY, which must stay on the " +
			"machine. Copy the part beginning -----BEGIN CERTIFICATE----- instead")
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("that is a %s, not a certificate", strings.ToLower(block.Type))
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("that certificate could not be read, so it is probably " +
			"incomplete. Copy all of it, including both -----  lines")
	}
	// A second certificate means a chain was pasted. Only the first is ever
	// compared, so accepting the rest silently would mean holding bytes that
	// decide nothing, and somebody believing the chain was checked.
	if len(strings.TrimSpace(string(rest))) > 0 {
		if extra, _ := pem.Decode(rest); extra != nil {
			return nil, fmt.Errorf("that is more than one certificate. Paste only the " +
				"machine's own")
		}
	}
	now := time.Now()
	if now.After(cert.NotAfter) {
		return nil, fmt.Errorf("that certificate expired on %s. Run the installer again "+
			"on the machine to make a new one", cert.NotAfter.Format("2 January 2006"))
	}
	if now.Before(cert.NotBefore) {
		return nil, fmt.Errorf("that certificate is not valid until %s, so the clock on "+
			"one of these two machines is wrong", cert.NotBefore.Format("2 January 2006"))
	}
	return cert, nil
}

// machineURL settles what a person typed into an address we can store.
//
// A bare host or `host:port` is normal, and https is assumed because that is the
// only thing a machine speaks. The port is defaulted rather than demanded: 19443
// is what the installer uses unless told otherwise, and making somebody type it
// is making them repeat our own default back to us.
func machineURL(address string) (string, error) {
	where := strings.TrimSpace(address)
	if where == "" {
		return "", fmt.Errorf("that machine needs an address this gateway can reach it on")
	}
	if strings.HasPrefix(where, "http://") {
		return "", fmt.Errorf("a machine is reached over https, so http:// will not work")
	}
	if !strings.Contains(where, "://") {
		where = "https://" + where
	}
	parsed, err := url.Parse(where)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("%q is not an address", address)
	}
	if parsed.Port() == "" {
		parsed.Host += ":19443"
	}
	// Path, query and fragment are dropped rather than kept: somebody pasting a
	// browser URL should not end up with `/v1/v1`.
	//
	// And it ENDS IN /v1, because a node serves its OpenAI-compatible routes
	// there and this row is what the model gateway posts to. Joining has always
	// appended it (nodejoin.go, nodeBaseURL); this path did not, so a machine
	// added by certificate answered every control-plane call, loaded a model,
	// and then returned 404 for /chat/completions.
	return (&url.URL{Scheme: "https", Host: parsed.Host}).String() + "/v1", nil
}

func mintNodeID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("could not mint an id for that machine: %w", err)
	}
	return "nd_" + hex.EncodeToString(raw), nil
}

func mintNodeKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("could not mint a key for that machine: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// EditMachine changes where a machine is, and optionally the certificate it
// answers with.
//
// This existed only as a row in the database, and the omission was not small.
// The Add flow asks for the address up front, which is right for a machine
// whose address you already know and wrong for the common case that follows:
// a rented GPU behind a mapping, where the port reaching it from outside is
// assigned by the provider AFTER the install and is not the one the machine
// listens on. Getting that wrong left a machine permanently unreachable with no
// way to correct it, since nothing about the address was editable.
//
// The certificate is optional here because the two change for different
// reasons. An address changes when a mapping or a lease does, and the machine is
// otherwise untouched. A certificate changes only when the machine made a new
// one, which means somebody re-ran the installer.
func (a *App) EditMachineByNodeID(ctx context.Context, nodeID, name, address, certPEM string) (*model.InferenceNode, error) {
	node, err := a.Store.Nodes().ByNodeID(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if name = strings.TrimSpace(name); name != "" {
		node.Name = name
	}
	if strings.TrimSpace(address) != "" {
		baseURL, err := machineURL(address)
		if err != nil {
			return nil, err
		}
		node.BaseURL = baseURL
	}
	if strings.TrimSpace(certPEM) != "" {
		// Only a machine we hold a certificate for can be given a new one. One
		// that joined is verified by chaining to our authority, and pinning a
		// certificate onto it would silently change how it is trusted.
		if node.PinnedCert == nil {
			return nil, fmt.Errorf("that machine was added by joining, so its certificate " +
				"comes from this gateway and is not pasted in")
		}
		cert, err := readPastedCertificate(certPEM)
		if err != nil {
			return nil, err
		}
		pinned := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
		node.PinnedCert = &pinned
		expires := cert.NotAfter
		node.CertExpiresAt = &expires
	}
	if err := a.Store.Nodes().Update(ctx, node); err != nil {
		return nil, err
	}
	a.Log.Info().Str("node", node.NodeID).Str("at", node.BaseURL).Msg("machine changed")
	return node, nil
}
