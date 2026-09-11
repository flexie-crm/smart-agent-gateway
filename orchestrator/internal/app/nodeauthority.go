package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"flexie.io/sag/internal/nodeca"
	"flexie.io/sag/internal/store"
)

// The authority both ends of a machine connection chain to, and the certificate
// we present when we dial one.
//
// KB/35 put a machine inside one network and let plain HTTP carry its key. A
// machine on a public address makes that a key anybody in the path can read, so
// the two ends authenticate each other with certificates instead. The authority
// that issues them is OURS (see internal/nodeca for why a public one cannot do
// this job), it is made the first time anything needs it, and it lives in one
// sealed row.

// What we call ourselves in the certificate we present to machines. It is not
// checked by anything: what a machine checks is that the certificate chains to
// the authority and is a CLIENT certificate, which its own cannot be. The name
// is here so a person reading a log on a machine can see who called.
const orchestratorName = "sag-orchestrator"

// machineTrust is the authority plus the certificate we dial with, worked out
// once.
//
// Our own certificate is minted in memory at first use and never stored: it is
// on this side of the network, so there is nothing to be gained by making it
// travel. It is therefore also never old, which removes a renewal path that
// would otherwise have to be got right.
type machineTrust struct {
	authority *nodeca.Authority
	ours      tls.Certificate
	// client is the one used for INFERENCE, which is every model call to every
	// machine. Built once and kept, because a transport per call keeps no
	// connections and pools nothing: a fleet answering a busy workspace would
	// pay a handshake per turn.
	client *http.Client

	// control holds the client for each machine's CONTROL surface, one per
	// machine because that surface names the machine it means to reach and pins
	// the certificate to it, which is a different tls.Config and therefore a
	// different pool for every one.
	//
	// Kept for a harder reason than the one above, and the difference is worth
	// stating because getting it wrong took a machine down. A transport is a
	// CONNECTION POOL, and a pool that is dropped is not closed: Go holds a
	// transport's idle connections open until that transport says otherwise,
	// and a transport nothing refers to any more never says anything. So a
	// client built per call does not merely fail to pool, it LEAKS one live
	// socket per call, for as long as the process runs. See KB/29.
	controlMu sync.Mutex
	control   map[string]*http.Client
	// Which certificate each pinned client was built for, so a machine added
	// again with a new one is not dialled with the old client for ever.
	pinnedFor map[string]string
}

// controlClient hands back the client for one machine, making it the first time.
func (t *machineTrust) controlClient(nodeID string) *http.Client {
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	if c, ok := t.control[nodeID]; ok {
		return c
	}
	c := &http.Client{Transport: machineTransport(t.authority.ClientTLS(t.ours, nodeID))}
	t.control[nodeID] = c
	return c
}

// pinnedClient is the client for a machine we hold the certificate of.
//
// Keyed by the node id like the others, and for the same reason: a transport
// built per call leaks a live socket per call (see machineTrust.control). The
// certificate is not part of the key, so a machine re-enrolled with a new one
// would keep being dialled with the old client. That is handled by dropping the
// entry when the certificate changes rather than by holding two, because two
// clients for one machine is two pools to the same address.
func (t *machineTrust) pinnedClient(nodeID, pinnedPEM string) (*http.Client, error) {
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	key := "pinned:" + nodeID
	if c, ok := t.control[key]; ok && t.pinnedFor[nodeID] == pinnedPEM {
		return c, nil
	}
	cfg, err := nodeca.ClientTLSPinned(t.ours, pinnedPEM)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Transport: machineTransport(cfg)}
	t.control[key] = c
	t.pinnedFor[nodeID] = pinnedPEM
	return c, nil
}

// machineTransport is how we hold connections to a machine.
//
// Every field beyond the certificate is a BOUND, and they are set explicitly
// because a hand-built http.Transport inherits none of the defaults people
// assume it does: http.DefaultTransport prunes idle connections after ninety
// seconds and caps them per host, while a transport written out like this one
// has no idle timeout and no cap at all. Reusing an unbounded pool is only a
// slower version of leaking one, so the size of this pool is a property of the
// code here rather than of how long the process has been up.
//
// No Timeout on the client that carries it: every call in internal/inference
// sets its own deadline on the context, which is the same ceiling expressed per
// call, where the difference between a five second probe and a thirty minute
// load can actually be said.
func machineTransport(cfg *tls.Config) *http.Transport {
	return &http.Transport{
		TLSClientConfig:     cfg,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
}

// Authority returns the deployment's machine authority, making one the first
// time it is asked for.
//
// On demand rather than at install, for the same reason the join token is: a
// deployment that never runs a machine of its own should not have a signing key
// sitting in it.
func (a *App) Authority(ctx context.Context) (*nodeca.Authority, error) {
	trust, err := a.machineTrust(ctx)
	if err != nil {
		return nil, err
	}
	return trust.authority, nil
}

// machineTrust loads the authority and our certificate, once.
func (a *App) machineTrust(ctx context.Context) (*machineTrust, error) {
	a.machineTrustOnce.Lock()
	defer a.machineTrustOnce.Unlock()
	if a.trust != nil {
		return a.trust, nil
	}

	authority, err := a.loadOrCreateAuthority(ctx)
	if err != nil {
		return nil, err
	}
	ours, err := authority.IssueClient(orchestratorName, time.Now())
	if err != nil {
		return nil, err
	}
	a.trust = &machineTrust{
		authority: authority,
		ours:      ours,
		client:    &http.Client{Transport: machineTransport(authority.ClientTLS(ours, ""))},
		control:   map[string]*http.Client{},
		pinnedFor: map[string]string{},
	}
	return a.trust, nil
}

// loadOrCreateAuthority reads the stored authority, or makes it.
//
// The create is a plain insert on a constant key, so two boots racing to make
// the first authority end with ONE, and the loser reads back the winner's rather
// than replacing it. An authority replaced in place would orphan every machine
// already holding a certificate from the old one, silently, with the symptom
// appearing only on the next call to a machine.
func (a *App) loadOrCreateAuthority(ctx context.Context) (*nodeca.Authority, error) {
	sealed, certPEM, err := a.Store.NodeAuthority().Get(ctx)
	switch {
	case err == nil:
		keyPEM, err := a.Keyring.Open(sealed)
		if err != nil {
			return nil, ErrCredentialsUnavailable
		}
		return nodeca.Load(keyPEM, certPEM)
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	authority, err := nodeca.Create(time.Now())
	if err != nil {
		return nil, err
	}
	keyPEM, err := authority.KeyPEM()
	if err != nil {
		return nil, err
	}
	sealedKey, err := a.Keyring.Seal(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("seal the machine authority: %w", err)
	}
	if err := a.Store.NodeAuthority().Create(ctx, sealedKey, authority.CertPEM()); err != nil {
		// Somebody else got there first. Theirs is the authority, because it may
		// already have signed a machine.
		sealed, certPEM, readErr := a.Store.NodeAuthority().Get(ctx)
		if readErr != nil {
			return nil, err
		}
		keyPEM, openErr := a.Keyring.Open(sealed)
		if openErr != nil {
			return nil, ErrCredentialsUnavailable
		}
		return nodeca.Load(keyPEM, certPEM)
	}
	a.Log.Info().Str("authority", authority.Fingerprint()).Msg("machine authority created")
	return authority, nil
}

// MachineControlClient is how we dial one machine's control surface: our
// certificate, and the authority to judge that machine's by.
//
// nodeID is the machine we mean to reach, and it is named here where the
// inference path leaves it empty. Both are honest positions and the difference
// is real: the control surface knows which machine it is calling and says so,
// while inference goes through the model gateway, where a vendor row is all
// there is and the machine behind it is not the gateway's business. The first
// is the stronger check and is used wherever it can be.
//
// It hands back a CLIENT rather than the tls.Config it is built from, so that
// the pool belongs to this, which lives as long as the process, instead of to
// the caller, which is a per-request value. That is not a convenience: see the
// note on machineTrust.control for what the other arrangement costs.
// pinnedPEM is the certificate of a machine that issued its own, and empty for
// one this authority signed. A machine enrolled by hand chains to nothing, so
// the fleet's authority cannot verify it and the certificate itself is what
// identifies it.
func (a *App) MachineControlClient(ctx context.Context, nodeID, pinnedPEM string) (*http.Client, error) {
	trust, err := a.machineTrust(ctx)
	if err != nil {
		return nil, err
	}
	if pinnedPEM != "" {
		return trust.pinnedClient(nodeID, pinnedPEM)
	}
	return trust.controlClient(nodeID), nil
}

// MachineClient is the client the model gateway calls a machine of ours with.
//
// No node id, because this is the INFERENCE path: it goes through the gateway,
// where a vendor row is all there is, and which machine is behind that row is
// not something the gateway knows or should. What it does prove is that whatever
// answered holds a certificate from our authority, which is what stops a model
// call being answered by something else that took the address.
func (a *App) MachineClient(nodeID int64) (*http.Client, error) {
	ctx := context.Background()
	// A machine that signed its own certificate is verified by holding those
	// exact bytes, because it chains to nothing. Reaching it with the fleet
	// client asks for a name it does not carry and fails every call, which is
	// what happened: the control plane knew which machine it was calling and
	// worked, while inference went through a vendor row and did not.
	if nodeID != 0 {
		if node, err := a.Store.Nodes().Get(ctx, nodeID); err == nil && node.PinnedCert != nil {
			trust, err := a.machineTrust(ctx)
			if err != nil {
				return nil, err
			}
			return trust.pinnedClient(node.NodeID, *node.PinnedCert)
		}
	}
	trust, err := a.machineTrust(ctx)
	if err != nil {
		return nil, err
	}
	return trust.client, nil
}
