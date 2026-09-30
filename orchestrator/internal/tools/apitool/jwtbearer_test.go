package apitool

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/tool"
)

// A service account proves itself by SIGNING, and the assertion has to be one
// the service will accept: the grant name, the parameter name and the four
// claims RFC 7523 requires of it.

func rsaKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("make a key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func ecKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("make a key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func claimsOf(t *testing.T, assertion string) map[string]any {
	t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("an assertion has three parts, this has %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("the claims are not base64url: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("the claims are not JSON: %v", err)
	}
	return claims
}

// Everything the specification requires of the assertion, and the defaults
// that make the form short.
func TestTheAssertionCarriesWhatTheSpecificationRequires(t *testing.T) {
	auth := Auth{
		Kind: AuthJWTBearer, TokenURL: "https://oauth2.example.com/token",
		Issuer: "robot@project.example.com", PrivateKey: rsaKeyPEM(t),
		Scope: "records.read records.write",
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	assertion, err := signAssertion(auth, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims := claimsOf(t, assertion)

	// The key and the expiry, which is what a service asking for two claims
	// asks for, and what every service wants at minimum.
	for _, required := range []string{"iss", "exp"} {
		if claims[required] == nil {
			t.Fatalf("the token carries no %q: %v", required, claims)
		}
	}
	if claims["iss"] != auth.Issuer {
		t.Fatalf("iss = %v, want %q", claims["iss"], auth.Issuer)
	}
	// And NOTHING a service did not ask for. A token carrying an audience or a
	// subject it never named can be refused for exactly that, so neither is
	// invented.
	for _, absent := range []string{"aud", "sub"} {
		if _, present := claims[absent]; present {
			t.Fatalf("%q was invented though the service asked for no such claim: %v", absent, claims)
		}
	}
	if claims["exp"].(float64) <= claims["iat"].(float64) {
		t.Fatalf("the assertion expires before it was issued: %v", claims)
	}
	if claims["scope"] != auth.Scope {
		t.Fatalf("the scope did not reach the assertion, so a service wanting one gets none: %v", claims)
	}

	// And a delegated subject is used when it is given, which is the whole
	// point of the field.
	auth.Subject = "someone@example.com"
	assertion, err = signAssertion(auth, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if got := claimsOf(t, assertion)["sub"]; got != auth.Subject {
		t.Fatalf("sub = %v, want %q", got, auth.Subject)
	}

	// An audience where the service wants one, and anything else it asks for
	// beyond the four, which is what the extra claims are for.
	auth.Audience = "https://api.example.com"
	auth.ClaimsPairs = []tool.Pair{{Name: "tenant", Value: "acme"}}
	assertion, err = signAssertion(auth, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got := claimsOf(t, assertion)
	if got["aud"] != auth.Audience {
		t.Fatalf("aud = %v, want %q", got["aud"], auth.Audience)
	}
	if got["tenant"] != "acme" {
		t.Fatalf("an extra claim did not reach the token: %v", got)
	}

	// An extra claim cannot overwrite one of the four: a mistyped name must not
	// quietly replace the expiry or the issuer.
	auth.ClaimsPairs = []tool.Pair{{Name: "iss", Value: "somebody-else"}}
	assertion, err = signAssertion(auth, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if again := claimsOf(t, assertion); again["iss"] != auth.Issuer {
		t.Fatalf("an extra claim overwrote the issuer: %v", again["iss"])
	}
}

// The token the tool sends IS the one it signed: there is no exchange, no
// request to a token endpoint, and nothing for a service to be down for.
func TestTheSignedTokenIsTheCredential(t *testing.T) {
	auth := Auth{
		Kind: AuthJWTBearer, Issuer: "a-key", PrivateKey: "a-secret", Algorithm: AlgHS256,
	}
	token, expires, err := fetch(context.Background(), nil, auth)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// A JWT has three parts, and what came back is the token itself.
	if strings.Count(token, ".") != 2 {
		t.Fatalf("what came back is not a token: %q", token)
	}
	if claimsOf(t, token)["iss"] != "a-key" {
		t.Fatalf("the token is not ours: %v", claimsOf(t, token))
	}
	// It expires, so the cache renews it rather than sending a dead one.
	if expires.IsZero() || !expires.After(time.Now()) {
		t.Fatalf("the token has no usable expiry: %v", expires)
	}
	// And no HTTP client was needed, which is the point: nil above would have
	// panicked if a request had been made.
}

// An elliptic signature is two fixed-width halves, NOT the ASN.1 sequence Go
// hands back. A service given the DER form answers that the signature is
// invalid and says nothing about why, so this is asserted by SIZE.
func TestAnEllipticSignatureIsTheShapeJWSWants(t *testing.T) {
	auth := Auth{
		Kind: AuthJWTBearer, TokenURL: "https://oauth2.example.com/token",
		Issuer: "robot@example.com", PrivateKey: ecKeyPEM(t), Algorithm: AlgES256,
	}
	assertion, err := signAssertion(auth, time.Now().UTC())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parts := strings.Split(assertion, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("the signature is not base64url: %v", err)
	}
	// P-256: two 32-byte halves, always, whatever the integers happen to be.
	if len(signature) != 64 {
		t.Fatalf("an ES256 signature is 64 bytes, this is %d, which is the ASN.1 form a service refuses",
			len(signature))
	}
}

// A key and an algorithm that disagree is a configuration mistake, and it is
// said as one rather than as a cast failure.
func TestAKeyThatCannotSignThatWayIsRefusedClearly(t *testing.T) {
	rsaAuth := Auth{Kind: AuthJWTBearer, TokenURL: "https://x.example.com/token",
		Issuer: "a", PrivateKey: rsaKeyPEM(t), Algorithm: AlgES256}
	if _, err := signAssertion(rsaAuth, time.Now()); err == nil ||
		!strings.Contains(err.Error(), "not an elliptic key") {
		t.Fatalf("an RSA key signing ES256 was not reported usefully: %v", err)
	}
	ecAuth := Auth{Kind: AuthJWTBearer, TokenURL: "https://x.example.com/token",
		Issuer: "a", PrivateKey: ecKeyPEM(t), Algorithm: AlgRS256}
	if _, err := signAssertion(ecAuth, time.Now()); err == nil ||
		!strings.Contains(err.Error(), "not an RSA key") {
		t.Fatalf("an elliptic key signing RS256 was not reported usefully: %v", err)
	}
}

// What somebody actually pastes wrong, each answered by what to do about it.
func TestAKeyThatIsNotAKeyIsExplained(t *testing.T) {
	for _, tc := range []struct{ name, key, want string }{
		{"nothing at all", "", "required"},
		{"the base64 without the markers", "MIIEvQIBADANBgkqh", "-----BEGIN"},
		{"a certificate rather than a key",
			string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a key")})),
			"not a private key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readPrivateKey(tc.key)
			if err == nil {
				t.Fatal("it was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not say what to do: %v", err)
			}
		})
	}
}

// Two tools acting as different subjects of one issuer must not share a token,
// and a rotated key must not hand back what the old one obtained.
func TestTheCacheKeyCoversTheAssertionsIdentity(t *testing.T) {
	base := Auth{Kind: AuthJWTBearer, TokenURL: "https://x.example.com/token",
		Issuer: "robot@example.com", PrivateKey: rsaKeyPEM(t)}

	delegated := base
	delegated.Subject = "someone@example.com"
	if keyOf(base, "") == keyOf(delegated, "") {
		t.Fatal("two subjects of one issuer share a cached token, so one acts as the other")
	}
	rotated := base
	rotated.PrivateKey = rsaKeyPEM(t)
	if keyOf(base, "") == keyOf(rotated, "") {
		t.Fatal("a rotated key hands back the token the old one obtained")
	}
	otherAudience := base
	otherAudience.Audience = "https://login.example.com"
	if keyOf(base, "") == keyOf(otherAudience, "") {
		t.Fatal("two audiences share a cached token")
	}
}

// The assertion VERIFIES, which is the only thing a service actually does with
// it. Everything above checks the shape; this checks that the signature is
// over the right bytes with the right algorithm, which is what stands between
// a working service account and a 400 nobody can read.
func TestTheAssertionVerifiesWithThePublicHalf(t *testing.T) {
	t.Run("RS256", func(t *testing.T) {
		keyPEM := rsaKeyPEM(t)
		auth := Auth{Kind: AuthJWTBearer, TokenURL: "https://x.example.com/token",
			Issuer: "robot@example.com", PrivateKey: keyPEM}
		assertion, err := signAssertion(auth, time.Now().UTC())
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		signed, signature := split(t, assertion)
		key, _ := readPrivateKey(keyPEM)
		rsaKey := key.(*rsa.PrivateKey)
		digest := sha256.Sum256([]byte(signed))
		if err := rsa.VerifyPKCS1v15(&rsaKey.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
			t.Fatalf("the service could not verify the assertion: %v", err)
		}
	})

	t.Run("ES256", func(t *testing.T) {
		keyPEM := ecKeyPEM(t)
		auth := Auth{Kind: AuthJWTBearer, TokenURL: "https://x.example.com/token",
			Issuer: "robot@example.com", PrivateKey: keyPEM, Algorithm: AlgES256}
		assertion, err := signAssertion(auth, time.Now().UTC())
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		signed, signature := split(t, assertion)
		key, _ := readPrivateKey(keyPEM)
		ecKey := key.(*ecdsa.PrivateKey)
		digest := sha256.Sum256([]byte(signed))
		// Read back the two halves the way a verifier does.
		half := len(signature) / 2
		r := new(big.Int).SetBytes(signature[:half])
		s := new(big.Int).SetBytes(signature[half:])
		if !ecdsa.Verify(&ecKey.PublicKey, digest[:], r, s) {
			t.Fatal("the service could not verify the assertion")
		}
	})
}

func split(t *testing.T, assertion string) (string, []byte) {
	t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("an assertion has three parts, this has %d", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("the signature is not base64url: %v", err)
	}
	return parts[0] + "." + parts[1], signature
}

// The driver is OFFERED and its form asks for the right things.
//
// Added because the variant and its section were written and then silently not
// applied: the grant worked, was tested, and could not be chosen by anybody,
// which no test above would have noticed.
func TestTheJWTDriverIsOnTheForm(t *testing.T) {
	tpl := New(nil)

	offered := false
	for _, v := range tpl.Variants() {
		if v.Key == AuthJWTBearer {
			offered = true
			if v.Label == "" {
				t.Fatal("the driver has no label, so the dropdown shows an empty row")
			}
		}
	}
	if !offered {
		t.Fatal("the service-account grant is not in the driver list, so it cannot be chosen")
	}

	sections, err := tpl.Fields(AuthJWTBearer)
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	required, seen := map[string]bool{}, map[string]bool{}
	for _, section := range sections {
		for _, f := range section.Fields {
			seen[f.Key] = true
			required[f.Key] = f.Required
		}
	}
	// The two things a service hands you, and nothing else is required.
	for _, key := range []string{"auth.issuer", "auth.private_key"} {
		if !seen[key] {
			t.Fatalf("%s is not on the form", key)
		}
		if !required[key] {
			t.Fatalf("%s is optional, but nothing can supply it", key)
		}
	}
	// The rest of the claims are on the form and optional, because a service
	// wanting two and sent four can refuse the lot.
	for _, key := range []string{"auth.subject", "auth.audience", "auth.lifetime_minutes", "auth.algorithm"} {
		if !seen[key] {
			t.Fatalf("%s is not on the form, so a service wanting it cannot be configured", key)
		}
		if required[key] {
			t.Fatalf("%s is required, though a service may not want it at all", key)
		}
	}
	// And there is no token address: this driver exchanges nothing.
	if seen["auth.token_url"] {
		t.Fatal("the form asks for a token address, but a signed token is sent as it is")
	}
	// And the key is the one thing sealed at rest.
	sealed := tpl.SecretPaths(AuthJWTBearer)
	held := false
	for _, p := range sealed {
		if p == "auth.private_key" {
			held = true
		}
	}
	if !held {
		t.Fatalf("the private key is not sealed at rest: %v", sealed)
	}
}

// Where the token travels, and the one thing that changes with it.
//
// The header name is a plain field with Authorization as its default, rather
// than an empty box and a sentence explaining what empty means. That makes the
// name the only thing deciding the VALUE too: Authorization carries the bearer
// scheme in front of the token, and a service's own header carries the token
// alone. Sending the scheme where it is not wanted is a 401, and omitting it
// where it is expected is the same 401.
func TestTheTokenTravelsWhereTheServiceWantsIt(t *testing.T) {
	for _, tc := range []struct{ name, field, wantName, wantValue string }{
		{"the default, which the form fills in", HeaderAuthorization, "Authorization", "Bearer TOKEN"},
		{"nothing set at all, which behaves as the default", "", "Authorization", "Bearer TOKEN"},
		{"however it is capitalised", "authorization", "Authorization", "Bearer TOKEN"},
		{"a service's own header takes the token alone", "Token", "Token", "TOKEN"},
		{"and any other name does too", "X-Api-Token", "X-Api-Token", "TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotValue := tokenHeader(Auth{Kind: AuthJWTBearer, Name: tc.field}, "TOKEN")
			if gotName != tc.wantName || gotValue != tc.wantValue {
				t.Fatalf("%q -> %q: %q, want %q: %q", tc.field, gotName, gotValue, tc.wantName, tc.wantValue)
			}
		})
	}
}

// The configured name belongs to the JWT driver and to no other.
//
// A tool switched from the API-key driver still carries that driver's header
// name in its stored configuration (the form writes the fields of the chosen
// variant and does not erase the others), and reading it here would send an
// OAuth access token under a name nobody chose for it. Every grant that fetches
// a token now goes through this one function, which is what made the question
// reachable at all.
func TestOnlyTheJWTDriverDecidesWhereItsTokenGoes(t *testing.T) {
	for _, kind := range []string{AuthClientCredentials, AuthAuthorizationCode, AuthBearer, AuthBasic} {
		name, value := tokenHeader(Auth{Kind: kind, Name: "X-Api-Token"}, "TOKEN")
		if name != HeaderAuthorization || value != "Bearer TOKEN" {
			t.Fatalf("%s honoured a stale header name: %q: %q", kind, name, value)
		}
	}
	// And the JWT driver still does decide.
	if name, _ := tokenHeader(Auth{Kind: AuthJWTBearer, Name: "X-Api-Token"}, "TOKEN"); name != "X-Api-Token" {
		t.Fatalf("the JWT driver's own name was ignored: %q", name)
	}
}

// And the form offers that default rather than explaining an empty box.
func TestTheHeaderFieldComesFilledIn(t *testing.T) {
	sections, err := New(nil).Fields(AuthJWTBearer)
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	for _, section := range sections {
		for _, f := range section.Fields {
			if f.Key != "auth.name" {
				continue
			}
			if f.Default != HeaderAuthorization {
				t.Fatalf("the header field starts empty (default %q), so somebody has to be told what empty means", f.Default)
			}
			return
		}
	}
	t.Fatal("the header field is not on the form")
}

// The issuer and the secret each take a whole row: they are what a service
// hands you, they are long, and they are what somebody fills in first.
func TestTheThingsAServiceHandsYouGetAWholeRow(t *testing.T) {
	sections, err := New(nil).Fields(AuthJWTBearer)
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	width := map[string]int{}
	for _, section := range sections {
		for _, f := range section.Fields {
			width[f.Key] = f.Span
		}
	}
	for _, key := range []string{"auth.issuer", "auth.private_key"} {
		if width[key] != 6 {
			t.Fatalf("%s spans %d of 6, not the whole row", key, width[key])
		}
	}
}
