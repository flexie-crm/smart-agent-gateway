package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/nodeca"
	"flexie.io/sag/internal/store"
)

// How a machine adds itself (KB/35).
//
// A machine that runs models we own is an `ai_vendors` row, and until now
// somebody typed it: the address, and the key to reach it. That does not survive
// a farm, so a machine JOINS instead, the way a node joins a swarm. It is
// started with where we are and a shared token, it registers itself, and the row
// appears.
//
// **This is the one call that goes the other way**, and it is worth saying so
// plainly because KB/35 recorded the opposite: the orchestrator polls and the
// node never calls back, so that a machine holding weights needs no route to us
// and no second key. Joining reverses that for exactly one request. Everything
// after it, the control surface and inference alike, is still us calling the
// machine. What we gain is the only thing that makes a fleet usable: nobody
// types a row.
//
// A machine belongs to the PLATFORM, so there is no workspace anywhere in here.
// It is racked once and every workspace may have models on it; what a workspace
// owns is the model row that routes to one, which is a separate decision made
// separately (see AttachNodeModel).
//
// # The join is also where the two ends learn to trust each other
//
// Everything after this call is mutually authenticated TLS, which needs a
// certificate at each end, which needs an authority (internal/nodeca). This is
// where a machine gets one, and the ordering is the interesting part, because at
// the moment a machine first calls us it knows nothing except what was pasted
// onto it.
//
// So the token is `<authority-fingerprint>_<secret>` and the secret NEVER
// TRAVELS. The machine signs its request with it and sends the signature; we
// recompute it and compare. An eavesdropper therefore learns nothing reusable,
// and cannot alter a byte of the request without the signature failing. Coming
// back, we send the authority's certificate, and the machine checks its
// fingerprint against the one in its own token before trusting anything in the
// reply. Neither side ever has to assume the other is who it says it is.
//
// Two secrets, doing different jobs. The **join token** admits a machine that is
// not yet known, once. The **node key** is minted by the machine, is unique to
// it, and is what that machine is ever after: it rides inside the authenticated
// channel on every call we make to it, and it is what the machine signs its own
// check-ins with. A machine that is removed loses its key without any other
// machine being affected.
//
// # Admission is not renewal, and they use different credentials
//
// A machine checks back in every day, and each check-in reissues its certificate
// (main.rs, RE_ENROL). That arrives here, at this same endpoint, because it is
// the same conversation: here is who I am, here is where to reach me, please
// sign this.
//
// What it must NOT be is the same credential. If a machine presented its join
// token daily forever, the token could never expire and could never be spent,
// which is to say it would be a permanent shared key to the fleet sitting in an
// environment file on every box. So:
//
//   - a machine we have never seen signs with the JOIN TOKEN, which is checked
//     against the unspent invitations and then destroyed;
//   - a machine we already have signs with ITS OWN KEY, which is checked against
//     the one on its row, and costs no invitation at all.
//
// The machine picks by whether it already holds a certificate, and falls back to
// the token if its key is refused, which is the case of a machine carried over
// from a deployment that no longer knows it.

// How long an invitation stands.
//
// It is the gap between somebody clicking Add a machine and somebody finishing a
// paste into a terminal on that machine, plus room for the install itself, which
// downloads a binary. An hour covers a slow line and a phone call; it does not
// cover leaving the tab open until tomorrow, which is the case this whole change
// is about.
const joinTokenLife = time.Hour

// Bytes of randomness behind a join token and a node id.
const (
	joinTokenBytes = 24
	nodeIDBytes    = 12
)

// How far out a machine's clock may be before its join is refused.
//
// The request carries when it was signed, so that a captured one cannot be
// replayed a week later to point a machine's row at somebody else's address.
// Five minutes is the usual allowance for two machines that have never spoken:
// tight enough that a replay window is not a hole, loose enough that an
// unsynchronised clock is not an outage.
const joinSkew = 5 * time.Minute

// NodeJoinRequest is what a machine says about itself when it registers.
//
// Note what is NOT in it: the join token. The machine proves it holds one by
// signing these bytes with it, so the shared secret never crosses the network
// even once.
type NodeJoinRequest struct {
	// NodeID is minted on the machine's own disk and is how a machine that comes
	// back is recognised as the same one.
	NodeID string `json:"node_id"`
	// Name is what to call it. A hostname, usually.
	Name string `json:"name"`
	// Advertise is where WE should reach it, host and port. Blank is the normal
	// case and means "the address you saw this request come from, on the port I
	// tell you I listen on", which is right for every machine we can reach
	// directly. It is set only when that is not where we can reach it: a port
	// forward, or a proxy in front.
	Advertise string `json:"advertise"`
	// Port is what the machine listens on. It is what makes Advertise optional:
	// the connection tells us the host and nothing else, so without this there is
	// no address to write down and a machine that said nothing was refused.
	Port int `json:"port"`
	// Key is the machine's own secret, minted by it, which every later call
	// carries. It is sealed here and never sent back.
	Key string `json:"key"`
	// Version is what the machine is running, for a person reading the list.
	Version string `json:"version"`
	// CertificateRequest is the machine asking to be given an identity: a PEM
	// certificate request over a key that never leaves it.
	CertificateRequest string `json:"certificate_request"`
	// IssuedAt is when the machine signed this request, which is what stops a
	// captured one being replayed later.
	IssuedAt time.Time `json:"issued_at"`
}

// NodeJoinResponse tells a machine what it became, and gives it the identity it
// asked for.
//
// It carries no secret: the certificate is public by construction and useless
// without the key the machine kept, and the authority's certificate is the thing
// we hand to everybody. What the machine must do with it is check the
// fingerprint against its own token before believing a word of this.
type NodeJoinResponse struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	// Joined is false when the machine was already registered and this call only
	// refreshed it, so a node can say "rejoined" rather than "joined".
	Joined bool `json:"joined"`
	// Authority is the certificate both ends chain to.
	Authority string `json:"authority"`
	// Certificate is the machine's own, signed by that authority.
	Certificate string `json:"certificate"`
}

// ErrJoinRefused is a signature that is not this deployment's, or no signature at
// all.
//
// One error for every way of failing, on purpose. A caller that could tell "no
// token has been made here" from "wrong token" from "stale timestamp" learns
// something about a deployment it has not authenticated to.
var ErrJoinRefused = errors.New("that join token is not valid here")

// MintJoinToken gives one person an invitation, replacing the one they had.
//
// There is no "read mine back". Every caller here is somebody about to install a
// machine, so the answer is always a live token, and a token that can be read
// back is a token that outlives the reason it was made. Asking twice updates the
// same row: one person cannot leave a trail of working credentials behind them,
// and cannot take anybody else's away either.
//
// On demand rather than at install, because a deployment that never runs a
// machine of its own should not have a credential sitting in it.
func (a *App) MintJoinToken(ctx context.Context, userID int64) (string, error) {
	authority, err := a.Authority(ctx)
	if err != nil {
		return "", err
	}
	secret, err := mintSecret(joinTokenBytes)
	if err != nil {
		return "", err
	}
	// The fingerprint travels IN the token because that is the only channel a
	// machine has before it trusts anything: somebody carries this string to the
	// machine by hand. It is what turns "whatever answered" into "the deployment
	// I was told about".
	//
	// `<fingerprint>_<secret>` and nothing else. It carried a `sagjoin_` label as
	// well, on the theory that somebody finding one in a shell history should be
	// able to tell what it was; a credential that lives an hour is not found in a
	// shell history later, and the label was eleven characters of every line
	// somebody has to read before pasting.
	token := authority.Fingerprint() + "_" + secret
	sealed, err := a.SealCredentials(token)
	if err != nil {
		return "", err
	}

	now := time.Now()
	// Cheap, and it happens exactly when somebody is here anyway: an invitation
	// nobody used is gone by the time the next one is made, rather than sitting
	// in the table waiting for its owner to come back.
	if err := a.Store.NodeJoin().Sweep(ctx, now); err != nil {
		return "", err
	}
	if err := a.Store.NodeJoin().Mint(ctx, userID, sealed, now.Add(joinTokenLife), now); err != nil {
		return "", err
	}
	a.Log.Info().Int64("user", userID).Msg("a machine join token was minted")
	return token, nil
}

// JoinNode registers a machine, or refreshes the registration it already had.
//
// It takes the RAW body and the signature over it, rather than a decoded struct,
// because the signature covers exactly the bytes that arrived. Decoding and
// re-encoding to check it would be checking a signature over something the
// machine never signed.
//
// `source` is the address the request arrived from, used when the machine did
// not say where to reach it.
func (a *App) JoinNode(ctx context.Context, source string, body []byte, signature string) (NodeJoinResponse, error) {
	// Read before authenticating, which is the opposite of the usual order and is
	// right here: WHICH credential this request should be checked against depends
	// on which machine it claims to be. Nothing is trusted from the body, and
	// nothing is written until something has verified; the API layer caps its
	// size, so the worst an unauthenticated caller gets out of this is a JSON
	// parse.
	var req NodeJoinRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return NodeJoinResponse{}, fmt.Errorf("%w: the machine did not say anything we could read", ErrJoinRefused)
	}
	if err := validateJoin(&req); err != nil {
		return NodeJoinResponse{}, err
	}

	known, err := a.knownNode(ctx, req.NodeID)
	if err != nil {
		return NodeJoinResponse{}, err
	}
	invitation, err := a.authenticateJoin(ctx, known, body, signature)
	if err != nil {
		return NodeJoinResponse{}, err
	}

	baseURL, err := nodeBaseURL(req.Advertise, source, req.Port)
	if err != nil {
		return NodeJoinResponse{}, err
	}
	sealedKey, err := a.SealCredentials(req.Key)
	if err != nil {
		return NodeJoinResponse{}, err
	}

	authority, err := a.Authority(ctx)
	if err != nil {
		return NodeJoinResponse{}, err
	}
	now := time.Now()
	certPEM, err := authority.Issue(pemBlock(req.CertificateRequest), req.NodeID, addressesOf(baseURL), now)
	if err != nil {
		// The machine's own request was unusable. It is the one thing here worth
		// telling it plainly, because it is the one thing it can fix.
		return NodeJoinResponse{}, fmt.Errorf("%w: %w", ErrJoinRefused, err)
	}
	expires := now.Add(nodeca.CertificateLife)

	reply := NodeJoinResponse{
		BaseURL:     baseURL,
		Authority:   string(authority.CertPEM()),
		Certificate: string(certPEM),
	}

	// The invitation is spent BEFORE the machine is written, and the order is the
	// decision. Spending last would mean a machine already in the fleet when we
	// discover its token was taken by somebody else a millisecond earlier, which
	// is the one outcome single use exists to prevent. Spending first means a
	// database failure between the two costs somebody a fresh token and another
	// paste, which takes five seconds and admits nobody.
	//
	// Nothing to spend on a re-enrolment: the machine authenticated as itself.
	if invitation != nil {
		spent, err := a.Store.NodeJoin().Spend(ctx, invitation.ID, now)
		if err != nil {
			return NodeJoinResponse{}, err
		}
		if !spent {
			// Two machines were started with the same token and both got this far.
			// The other one has it.
			return NodeJoinResponse{}, ErrJoinRefused
		}
	}

	if known != nil {
		// The same machine, back again. Its address may have changed and its key
		// certainly has, so both are written; the row keeps its id, so every
		// workspace already pointing at it goes on working and every model on it
		// stays routable.
		known.Name = req.Name
		known.BaseURL = baseURL
		known.Key = sealedKey
		known.Version = req.Version
		known.CertExpiresAt = &expires
		if err := a.Store.Nodes().Update(ctx, known); err != nil {
			return NodeJoinResponse{}, err
		}
		a.Log.Info().Str("node", req.NodeID).Str("name", req.Name).Str("at", baseURL).
			Msg("machine rejoined")
		reply.Name, reply.Joined = known.Name, false
		return reply, nil
	}

	node := &model.InferenceNode{
		NodeID:        req.NodeID,
		Name:          req.Name,
		BaseURL:       baseURL,
		Key:           sealedKey,
		Version:       req.Version,
		CertExpiresAt: &expires,
	}
	if err := a.Store.Nodes().Create(ctx, node); err != nil {
		return NodeJoinResponse{}, err
	}
	a.Log.Info().Str("node", req.NodeID).Str("name", req.Name).Str("at", baseURL).
		Msg("machine joined")
	reply.Name, reply.Joined = node.Name, true
	return reply, nil
}

// knownNode is the machine this request claims to be, or nil for one we have
// never seen. Claims, because nothing has been verified yet: what it is for is
// deciding WHICH secret to check the signature against.
func (a *App) knownNode(ctx context.Context, nodeID string) (*model.InferenceNode, error) {
	node, err := a.Store.Nodes().ByNodeID(ctx, nodeID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return node, nil
}

// authenticateJoin proves the request is allowed, and says what that cost.
//
// It returns the invitation to spend, or nil when the machine authenticated as
// itself and no invitation was involved.
//
// The signature is over the bytes as they arrived, so it authenticates the whole
// request and not merely the sender: an intermediary cannot redirect a machine's
// address, swap its key or substitute its certificate request while leaving a
// valid signature behind.
func (a *App) authenticateJoin(ctx context.Context, known *model.InferenceNode, body []byte, signature string) (*store.NodeJoinToken, error) {
	presented, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(presented) == 0 {
		return nil, ErrJoinRefused
	}

	// A machine we already have, signing as itself. This is every check-in after
	// the first, which is to say almost all of them, and it costs nothing.
	if known != nil {
		key, err := a.Keyring.Open(known.Key)
		if err != nil {
			return nil, ErrCredentialsUnavailable
		}
		if hmac.Equal(presented, joinSignature(key, body)) {
			return nil, nil
		}
		// Fall through rather than refuse. A machine carried over from a
		// deployment that no longer knows it has an id and a key on its disk that
		// mean nothing here, and it is supposed to be able to present a token and
		// be admitted.
	}

	live, err := a.Store.NodeJoin().Live(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	for _, invitation := range live {
		token, err := a.Keyring.Open(invitation.Sealed)
		if err != nil {
			return nil, ErrCredentialsUnavailable
		}
		// Constant time, for the same reason the node's own key check is: an
		// attacker who can time this can otherwise recover the expected value a
		// byte at a time. Every candidate is tried even after one matches would be
		// the stricter version; it is not done here because the list is one row per
		// administrator with an unused invitation, and the loop leaks how many
		// there are rather than anything about any of them.
		if hmac.Equal(presented, joinSignature(token, body)) {
			return &invitation, nil
		}
	}
	// Including "nobody has made one", which is a refusal and not an error worth
	// distinguishing to a caller that has not authenticated.
	return nil, ErrJoinRefused
}

// joinSignature is the machine's proof, computed the way the machine computes
// it.
//
// Its own function because it is a CONTRACT with something written in another
// language, and a contract wants somewhere to point at and something to test
// against a value neither side produced.
func joinSignature(token, body []byte) []byte {
	mac := hmac.New(sha256.New, token)
	mac.Write(body)
	return mac.Sum(nil)
}

func validateJoin(req *NodeJoinRequest) error {
	req.NodeID = strings.TrimSpace(req.NodeID)
	req.Name = strings.TrimSpace(req.Name)
	req.Advertise = strings.TrimSpace(req.Advertise)
	req.Key = strings.TrimSpace(req.Key)

	switch {
	case req.NodeID == "" || len(req.NodeID) > 40:
		return fmt.Errorf("%w: the machine did not say which machine it is", ErrJoinRefused)
	case req.Name == "" || len(req.Name) > 255:
		return fmt.Errorf("%w: the machine did not say what to call it", ErrJoinRefused)
	case len(req.Key) < 24:
		// The same floor the machine enforces on itself. A short key here would
		// be a machine anyone on the network could drive.
		return fmt.Errorf("%w: the machine offered a key that is too short", ErrJoinRefused)
	case req.CertificateRequest == "":
		return fmt.Errorf("%w: the machine did not ask for a certificate", ErrJoinRefused)
	}

	// A signature is only good until its request goes stale. Both directions:
	// a clock far in the future would otherwise mint a request good for as long
	// as its owner liked.
	if skew := time.Since(req.IssuedAt); skew > joinSkew || skew < -joinSkew {
		return fmt.Errorf("%w: the machine's clock is too far from ours", ErrJoinRefused)
	}
	return nil
}

// nodeBaseURL works out where to reach a machine.
//
// What is stored is the INFERENCE url, ending in the version segment, because
// that is what a vendor row is and what the gateway uses unchanged. The control
// surface is its sibling and is derived when needed.
//
// Always https. A machine now holds a certificate and serves nothing without it,
// so there is no plain-text address to record; an administrator who advertises
// one is told rather than quietly given a connection that cannot be made.
func nodeBaseURL(advertise, source string, port int) (string, error) {
	where := advertise
	if where == "" {
		// The half we saw, and the half only the machine knows.
		//
		// The connection tells us the HOST it came from, which is the address
		// that reaches it from here rather than the one it believes it has, and
		// that is the right half to take from us. It tells us nothing useful
		// about the port: the source port of a request is ephemeral and belongs
		// to that one connection. So the machine sends the port it listens on
		// and we put the two together.
		//
		// This used to be `where = source` with no port at all, which meant a
		// machine that did not set an address was ALWAYS refused, with an error
		// about a missing port and a comment claiming this was the default that
		// usually just works. Setting the address was mandatory and documented
		// as optional.
		if source == "" {
			return "", fmt.Errorf("%w: there is no address to reach that machine on", ErrJoinRefused)
		}
		if port <= 0 {
			return "", fmt.Errorf("%w: the machine did not say which port it listens on", ErrJoinRefused)
		}
		where = net.JoinHostPort(source, strconv.Itoa(port))
	}
	if !strings.Contains(where, "://") {
		where = "https://" + where
	}

	parsed, err := url.Parse(where)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("%w: %q is not an address", ErrJoinRefused, advertise)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("%w: a machine is reached over an encrypted channel, so %q cannot be its address", ErrJoinRefused, where)
	}
	if _, _, err := net.SplitHostPort(parsed.Host); err != nil {
		return "", fmt.Errorf("%w: %q does not say which port", ErrJoinRefused, where)
	}
	return strings.TrimSuffix(parsed.Scheme+"://"+parsed.Host, "/") + "/v1", nil
}

// addressesOf is the host we will dial, for the certificate to carry.
//
// Verification does not depend on it (every machine answers to one fleet name,
// see nodeca.ServerName), so this is for a person reading a certificate and for
// any tool that insists on matching what it dialled.
func addressesOf(baseURL string) []string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return nil
	}
	return []string{host}
}

// fingerprintIn reads the authority fingerprint out of a join token, and
// returns empty for anything not shaped like one.
//
// It is the machine that has to do this for real (join.rs), and this is here so
// there is one place to test the shape both sides agree on.
func fingerprintIn(token string) string {
	// The fingerprint is hex and the secret may contain underscores, so the
	// FIRST underscore is the boundary and there is no ambiguity about it.
	fingerprint, secret, ok := strings.Cut(token, "_")
	if !ok || fingerprint == "" || secret == "" {
		return ""
	}
	return fingerprint
}

// pemBlock turns the machine's certificate request into the bytes the authority
// signs over. An unreadable one comes back as nothing, which the authority
// refuses with a message about what it actually is.
func pemBlock(text string) []byte {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil
	}
	return block.Bytes
}

// mintSecret makes a URL-safe secret nobody has to quote in a shell.
func mintSecret(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mint secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
