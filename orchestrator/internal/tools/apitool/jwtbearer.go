package apitool

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"hash"
	"math/big"
	"strings"
	"time"
)

// The JWT authorization grant (RFC 7523 section 2.1), which is how a service
// account proves itself: no secret travels and no person signs in.
//
// The tool holds a PRIVATE KEY and signs a short-lived assertion with it; the
// service checks the signature against the public half it already has. That is
// the whole difference from client credentials, and the reason a service can
// offer this and not that: nothing shared has to be kept in two places.
//
// It is how Google's server-to-server access works, and Salesforce's
// flow, and Adobe's. The claims are the specification's, quoted where they are
// asserted, because "MUST contain" is the difference between a token and a 400.

// AuthJWTBearer is the grant's name in a tool's config.
//
//nolint:gosec // G101: the NAME of a grant, stored as auth.kind, not a credential
const AuthJWTBearer = "oauth_jwt_bearer"

// Signing algorithms. RS256 is what every service that offers this accepts and
// is the default; ES256 exists because some issue elliptic keys and a key the
// tool cannot sign with is a tool that cannot be configured at all.
const (
	// Symmetric: the token is signed with a SHARED SECRET the service also
	// holds. This is how most APIs that offer JWT do it, and it is why those
	// have no token endpoint: there is nothing to exchange, because a service
	// holding the same secret can check the signature on the token itself.
	AlgHS256 = "HS256"
	AlgHS384 = "HS384"
	AlgHS512 = "HS512"
	// Asymmetric: signed with a private key the service never sees. Used where
	// the token IS exchanged for an access token (RFC 7523).
	AlgRS256 = "RS256"
	AlgES256 = "ES256"
)

// HeaderAuthorization is where a token travels unless a service wants its own
// header, and it is the default the form offers.
//
// It is also the ONE name whose value is not the bare token: a service reading
// an Authorization header expects the bearer scheme in front of it, and a
// header of its own expects the token alone. That difference is a property of
// the name rather than a second setting, so it is decided from the name.
const HeaderAuthorization = "Authorization"

// tokenHeader is the header a signed token travels in and the value it carries.
func tokenHeader(a Auth, token string) (string, string) {
	// The configured NAME is honoured for the JWT driver and for nothing else,
	// because that is the only form offering the field. A tool switched from
	// the API-key driver can still carry that driver's header name in its
	// stored configuration, and reading it here would send an OAuth access
	// token under a name nobody chose for it.
	name := ""
	if a.Kind == AuthJWTBearer {
		name = strings.TrimSpace(a.Name)
	}
	if name == "" {
		name = HeaderAuthorization
	}
	if strings.EqualFold(name, HeaderAuthorization) {
		return HeaderAuthorization, "Bearer " + token
	}
	return name, token
}

// symmetric reports that an algorithm signs with a shared secret rather than a
// private key, which decides both what the signing field holds and whether
// there is anything to exchange.
func symmetric(alg string) bool {
	switch alg {
	case AlgHS256, AlgHS384, AlgHS512:
		return true
	}
	return false
}

// defaultLife is how long a minted token lasts when nobody says otherwise.
//
// An hour is what services that do this commonly allow, and asking for the
// maximum is wrong: a token is minted again whenever the last one is nearly
// over, so a shorter life costs nothing and limits what a leaked one is worth.
const defaultLife = 30 * time.Minute

// signAssertion builds and signs the JWT this grant exchanges for a token.
//
// Every claim the specification requires is present or the caller is told
// which is missing, rather than the service answering 400 with nothing useful:
// iss identifies who issued it, sub the principal it is about, aud the
// authorization server it is for, exp when it stops being usable.
func signAssertion(a Auth, now time.Time) (string, error) {
	issuer := strings.TrimSpace(a.Issuer)
	if issuer == "" {
		return "", fmt.Errorf("the issuer is required: it is the identity the service knows this key by")
	}
	// RFC 7523 section 3: the JWT "MUST contain a 'sub' (subject) claim". Most
	// services want the issuer again; one that impersonates a person wants
	// them, so it is asked rather than assumed.
	subject := strings.TrimSpace(a.Subject)
	if subject == "" {
		subject = issuer
	}
	// Only what the service asked for. A token carrying an audience it never
	// named can be refused for exactly that, so nothing is invented here.
	audience := strings.TrimSpace(a.Audience)

	alg := strings.TrimSpace(a.Algorithm)
	if alg == "" {
		alg = AlgRS256
	}
	header, err := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims := map[string]any{
		"iss": issuer,
		"iat": now.Unix(),
		"exp": now.Add(lifeOf(a)).Unix(),
	}
	// Only where they mean something. A self-signed token whose service asks
	// for two claims and is sent four can be refused for carrying an audience
	// it never named, and inventing a subject for it says something untrue
	// about who is calling.
	if audience != "" {
		claims["aud"] = audience
	}
	if strings.TrimSpace(a.Subject) != "" {
		claims["sub"] = subject
	}
	// Anything else a service asks for. Last, and unable to overwrite the four
	// above: a mistyped name must not quietly replace the expiry or the issuer.
	for _, pair := range a.ClaimsPairs {
		if _, taken := claims[pair.Name]; taken {
			continue
		}
		claims[pair.Name] = pair.Value
	}
	// Not a claim of the grant, but every service that wants scopes wants them
	// here rather than as a form field, Google among them.
	if scope := strings.TrimSpace(a.Scope); scope != "" {
		claims["scope"] = scope
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	signing := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(body)
	signature, err := sign(alg, a.PrivateKey, []byte(signing))
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// lifeOf is how long a minted token lasts.
//
// An administrator's choice where there is one, because the ceiling is the
// service's: some refuse anything over an hour, and a token that outlives what
// they allow is refused on every call with nothing said about why.
func lifeOf(a Auth) time.Duration {
	if a.Lifetime > 0 {
		return time.Duration(a.Lifetime) * time.Minute
	}
	return defaultLife
}

// sign signs the assertion with the administrator's key.
func sign(alg, keyPEM string, signing []byte) ([]byte, error) {
	// A shared secret signs directly: there is no key to parse, and the secret
	// is the bytes an administrator pasted.
	switch alg {
	case AlgHS256, AlgHS384, AlgHS512:
		if strings.TrimSpace(keyPEM) == "" {
			return nil, fmt.Errorf("the signing secret is required: %s signs with a secret the service also holds", alg)
		}
		var mac hash.Hash
		switch alg {
		case AlgHS256:
			mac = hmac.New(sha256.New, []byte(keyPEM))
		case AlgHS384:
			mac = hmac.New(sha512.New384, []byte(keyPEM))
		default:
			mac = hmac.New(sha512.New, []byte(keyPEM))
		}
		mac.Write(signing)
		return mac.Sum(nil), nil
	}

	key, err := readPrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(signing)

	switch alg {
	case AlgRS256:
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("this key is not an RSA key, so it cannot sign with RS256; " +
				"choose ES256 if the service issued an elliptic key")
		}
		return rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest[:])
	case AlgES256:
		ecKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("this key is not an elliptic key, so it cannot sign with ES256; " +
				"choose RS256 if the service issued an RSA key")
		}
		r, s, err := ecdsa.Sign(rand.Reader, ecKey, digest[:])
		if err != nil {
			return nil, err
		}
		// JWS wants R and S as fixed-width halves, NOT the ASN.1 sequence
		// crypto/ecdsa hands back from SignASN1. A service given the DER form
		// answers that the signature is invalid, which says nothing about why.
		size := (ecKey.Curve.Params().BitSize + 7) / 8
		out := make([]byte, 2*size)
		pad(out[:size], r)
		pad(out[size:], s)
		return out, nil
	default:
		return nil, fmt.Errorf("%q is not a signing algorithm this tool knows", alg)
	}
}

// pad writes a big integer right-aligned into a fixed-width slot, which is what
// JWS requires of each half of an ECDSA signature.
func pad(into []byte, value *big.Int) {
	raw := value.Bytes()
	copy(into[len(into)-len(raw):], raw)
}

// readPrivateKey reads a PEM key in the two encodings services actually hand
// out, and says which is wrong rather than "failed to parse".
func readPrivateKey(keyPEM string) (crypto.PrivateKey, error) {
	text := strings.TrimSpace(keyPEM)
	if text == "" {
		return nil, fmt.Errorf("the private key is required: this grant signs an assertion rather than sending a secret")
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, fmt.Errorf("that is not a PEM private key: it should begin with -----BEGIN and be pasted whole, " +
			"including both marker lines")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("that PEM block is not a private key this tool can read (%q); "+
		"an encrypted key has to be decrypted first, and a certificate is not a key", block.Type)
}
