// Package config loads runtime configuration from the environment.
// Every deployable mode (server, worker) reads the same Config; unused
// fields are simply ignored by the mode that does not need them.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"flexie.io/sag/internal/model"
)

type Config struct {
	// HTTPAddr is the listen address for the API server (server mode).
	HTTPAddr string
	// BaseURL is the external URL of this deployment; it is the OAuth
	// issuer and must match what clients see (scheme included).
	BaseURL string
	// DBDSN is the MariaDB data source name.
	DBDSN string

	// UploadDir is where the bytes of an uploaded file are kept. One folder,
	// one file per attachment, named after the attachment's own id. Object
	// storage replaces the implementation behind it without moving the seam.
	UploadDir string
	// NATSURL is the NATS server URL (queue signals + event fan-out).
	NATSURL string
	// WorkerCapabilities is a comma-separated capability list this worker
	// serves (worker mode), e.g. "default" or "default,gpu,model-host".
	WorkerCapabilities string
	// VerboseEvents turns on a running commentary of what the fleet machinery is
	// doing: a worker taking a job, starting an agent, reporting back; the master
	// hearing it, moving the counter, closing the batch. It is off by default
	// because it is one line per event per agent, and on it is the only way to
	// watch a distributed thing happen in one `tail -f`.
	VerboseEvents bool
	// WorkerConcurrency is how many goroutines this process runs PER capability.
	// A goroutine holds one job at a time, so it is also the most work this
	// process does at once. Scaling past one machine is more processes, started
	// by whatever starts processes (KB/35), not a bigger number here.
	WorkerConcurrency int
	// SessionSecret signs browser sessions (OAuth login/consent flow).
	// 32 bytes, hex-encoded in SAG_SESSION_SECRET. When unset, a random
	// per-boot secret is generated: sessions then do not survive a
	// restart, which is acceptable for dev only.
	SessionSecret []byte
	// SessionSecretGenerated reports that no secret was configured and a
	// per-boot one is in use (callers should log a warning).
	SessionSecretGenerated bool
	// EncryptionKeys holds the key encryption keys that seal stored
	// secrets (vendor API keys, MCP credentials), as "id:hexkey,id:hexkey".
	// Several may be present so a rotation can still open older values.
	EncryptionKeys string
	// EncryptionPrimaryKeyID names the key that seals new secrets.
	EncryptionPrimaryKeyID string
	// ApprovalTTL is how long a confirmation stays answerable, for a workspace
	// that has not said otherwise. An agent or a workflow may override it: this
	// is the deployment's opinion, not the last word (KB/15).
	ApprovalTTL time.Duration
	// ShutdownGrace is how long a restart waits for work already under way to
	// finish before it gives up and cancels it (SAG_SHUTDOWN_GRACE).
	//
	// It is the whole of the graceful-shutdown policy. Too short and a deploy
	// interrupts answers people are reading; too long and a deploy hangs on an
	// agent that has settled in for ten minutes. Nothing waits forever: when it
	// expires the remaining work is cancelled and written down as interrupted,
	// which is the honest outcome rather than a silence.
	ShutdownGrace time.Duration
	// RefreshReuseGrace is how long after a refresh token is spent a replay of
	// it is read as a client that never received the replacement, rather than
	// as a stolen token (SAG_REFRESH_REUSE_GRACE). Zero restores strict reuse
	// detection: every replay revokes the whole rotation family.
	RefreshReuseGrace time.Duration
	// EncryptionKeysGenerated reports that no key was configured and a
	// per-boot one is in use. Anything sealed then is unreadable after a
	// restart, so this is a development-only fallback.
	EncryptionKeysGenerated bool
	// AllowedOrigins lists the browser origins that may call the API from
	// another origin (SAG_ALLOWED_ORIGINS, comma-separated). Development
	// serves the console and the API on different ports, so the console's
	// origin goes here. Empty means same-origin only, which is production.
	AllowedOrigins []string
	// ConsoleDir is where the console lives (SAG_CONSOLE_DIR). When set, this
	// server serves the console itself: one process answers both the API and
	// the page that calls it, from one origin. Empty means the API runs alone,
	// which is a deployment that puts a proxy in front of each page.
	//
	// Normally a directory of built files. An http:// address instead is the
	// build tool serving the page as somebody edits it, and this server passes
	// requests to it: same origin, same paths, live reload carried through, so
	// a change to the page is a reload rather than a rebuild.
	ConsoleDir string
	// InferenceDistDir holds the inference node's installer and binaries
	// (SAG_INFERENCE_DIST_DIR). Set, a deployment serves them at
	// /inference/download/v1 so a GPU machine on a closed network can install
	// from the server it is joining. Unset, the routes do not exist.
	InferenceDistDir string
	// ChatDir is where the chat lives (SAG_CHAT_DIR), read the same way and for
	// the same reason as ConsoleDir. A server deployment puts a proxy in front
	// of the chat; a personal installation has no proxy, so the one process
	// answers the API and both pages that call it.
	ChatDir string
	// PersonalBundleDir holds the database server the desktop carries
	// (SAG_PERSONAL_BUNDLE). Empty means beside the executable, which is where
	// an installer puts it. Only desktop mode reads it.
	PersonalBundleDir string
	// Personal says this is one person on one machine with their own database:
	// the personal assistant rather than the deployed product. Set from the
	// subcommand rather than the environment, because an environment variable is
	// a thing somebody can get wrong and this one decides whether a passwordless
	// sign-in exists.
	//
	// It is NOT "is this a desktop application". Both editions ship a desktop
	// chat; what differs is whether there are other people in it. Naming this
	// after the packaging made two unrelated questions share one answer.
	//
	// What it turns on: a single seeded owner who holds every permission, a local
	// sign-in with no password, and a console with no user administration in it.
	// What it does NOT turn off is any of the machinery underneath, which runs
	// exactly as it does on a server.
	Personal bool
	// PersonalOwnerEmail and PersonalOwnerSecret are the seeded owner's
	// credentials, put here by desktop mode after it has seeded them. They do
	// NOT come from the environment and have no meaning in any other mode.
	//
	// They exist so the local sign-in can call the same Login every other client
	// calls. That is the point: there is no second authentication path, no
	// bypass, and nothing about sessions, rotation or revocation behaves
	// differently. The Enter button removes the typing, not the check.
	PersonalOwnerEmail  string
	PersonalOwnerSecret string
	// MCPClientMetadataURL is a client id to present to a service that takes one
	// as a URL (SAG_MCP_CLIENT_METADATA_URL).
	//
	// It exists for the installation that has no address of its own that a
	// remote server could fetch: every laptop, and the whole personal edition.
	// The document is fetched by the AUTHORIZATION SERVER, so it has to be
	// somewhere public, but it does NOT have to be this machine: one document,
	// hosted once, describing this application and naming a loopback redirect,
	// is an identity every installation can share. That is how a desktop
	// application has always been an OAuth client, and a client id metadata
	// document is what lets it be one without registering anywhere first.
	//
	// Empty means only this deployment's own document is used, and an install
	// that cannot serve one falls back to a client entered by hand.
	MCPClientMetadataURL string
	// Dev says this server is somebody's working copy rather than a deployment.
	//
	// Set from the subcommand (`sag dev`) for the same reason Personal is: it
	// decides whether a sign-in that needs no password typed exists, and that is
	// not a decision to leave to a variable that can be got wrong in a deploy
	// file. `sag server` cannot turn it on however the environment is set.
	//
	// What it turns on: the local sign-in route, and a server that ADMITS to
	// being one, so both pages can say so where somebody will see it. What it
	// does NOT change is any of the machinery: the sign-in is the ordinary
	// Login with a real credential, and every session, rotation, revocation and
	// permission check behaves exactly as it does in production. A development
	// server that authenticated differently would be proving nothing.
	Dev bool
	// DevSignInEmail and DevSignInPassword are the account `sag dev` signs in
	// as, from SAG_DEV_SIGN_IN_EMAIL and SAG_DEV_SIGN_IN_PASSWORD.
	//
	// The environment is the right place for these and the wrong place for Dev
	// itself: this is WHICH account, which is a working preference, while Dev is
	// WHETHER the door exists. Read only in dev mode, and `sag dev` refuses to
	// start without them rather than serving a button that fails.
	DevSignInEmail    string
	DevSignInPassword string
	// PersonalAccelerated and PersonalAcceleratorReason say which inference build
	// this computer is actually running and why, so the Machines screen can show
	// the reality rather than the intention. A model that answers a hundred
	// times slower than expected should not be a mystery.
	PersonalAccelerated       bool
	PersonalAcceleratorReason string
	// PersonalStateDir is where a desktop installation keeps its data
	// (SAG_PERSONAL_STATE): the database, its key, its logs. Empty means the
	// user's own application-data directory, and never inside the application.
	PersonalStateDir string
	// LogLevel is how loud the logs are (SAG_LOG_LEVEL): error, warn, info
	// (default), debug, or trace. A level shows itself and everything more
	// severe.
	LogLevel string
	// LogFormat is how the logs read (SAG_LOG_FORMAT): "console" (default,
	// human-readable, coloured on a terminal) or "json" (for a collector).
	LogFormat string
}

// How long a restart waits for work under way. A minute covers an answer being
// written and most background agents; past that a deploy that never returns is
// worse than a turn that says it was interrupted.
const (
	defaultShutdownGrace = time.Minute
	maxShutdownGrace     = 10 * time.Minute
)

// How long a spent refresh token still answers for the client that spent it.
// Long enough to cover a restart and the reload that follows (a development
// rebuild is about twenty seconds of no server); short enough that a stolen
// token is useful for under a minute. Capped, because this is the one setting
// here that trades security for reliability and it should not be set to an hour
// by somebody who has not read what it does.
const (
	defaultRefreshReuseGrace = time.Minute
	maxRefreshReuseGrace     = 5 * time.Minute
)

func Load() (*Config, error) {
	// A .env beside the binary is read first, and the real environment always
	// wins over it. That order matters: a container sets its variables for a
	// reason, and a file left in an image must never quietly outrank them.
	// Its absence is the normal case in production, and not an error.
	_ = godotenv.Load()

	cfg := &Config{
		HTTPAddr:           envOr("SAG_HTTP_ADDR", ":8080"),
		BaseURL:            envOr("SAG_BASE_URL", "http://localhost:8080"),
		InferenceDistDir:   os.Getenv("SAG_INFERENCE_DIST_DIR"),
		DBDSN:              envOr("SAG_DB_DSN", "sag:sag@tcp(127.0.0.1:3306)/sag?parseTime=true"),
		UploadDir:          envOr("SAG_UPLOAD_DIR", "./uploads"),
		NATSURL:            envOr("SAG_NATS_URL", "nats://127.0.0.1:4222"),
		WorkerCapabilities: envOr("SAG_WORKER_CAPABILITIES", "default"),
		WorkerConcurrency:  envInt("SAG_WORKER_CONCURRENCY", 8),
		VerboseEvents:      envOr("SAG_VERBOSE_EVENTS", "") != "",
		ApprovalTTL:        model.DefaultApprovalTTL,
		ShutdownGrace:      defaultShutdownGrace,
		RefreshReuseGrace:  defaultRefreshReuseGrace,
		LogLevel:           envOr("SAG_LOG_LEVEL", "info"),
		LogFormat:          envOr("SAG_LOG_FORMAT", "console"),
	}

	if raw := os.Getenv("SAG_APPROVAL_TTL"); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("SAG_APPROVAL_TTL is not a duration (try 30m, 24h): %w", err)
		}
		if !model.ValidApprovalTTL(ttl) {
			return nil, fmt.Errorf("SAG_APPROVAL_TTL must be between %s and %s",
				model.MinApprovalTTL, model.MaxApprovalTTL)
		}
		cfg.ApprovalTTL = ttl
	}

	if raw := os.Getenv("SAG_SHUTDOWN_GRACE"); raw != "" {
		grace, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("SAG_SHUTDOWN_GRACE is not a duration (try 30s, 2m): %w", err)
		}
		if grace < 0 || grace > maxShutdownGrace {
			return nil, fmt.Errorf("SAG_SHUTDOWN_GRACE must be between 0 and %s", maxShutdownGrace)
		}
		cfg.ShutdownGrace = grace
	}

	if raw := os.Getenv("SAG_REFRESH_REUSE_GRACE"); raw != "" {
		grace, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("SAG_REFRESH_REUSE_GRACE is not a duration (try 0, 30s): %w", err)
		}
		if grace < 0 || grace > maxRefreshReuseGrace {
			return nil, fmt.Errorf("SAG_REFRESH_REUSE_GRACE must be between 0 and %s", maxRefreshReuseGrace)
		}
		cfg.RefreshReuseGrace = grace
	}

	if raw := os.Getenv("SAG_SESSION_SECRET"); raw != "" {
		secret, err := hex.DecodeString(raw)
		if err != nil || len(secret) != 32 {
			return nil, fmt.Errorf("SAG_SESSION_SECRET must be 32 bytes hex-encoded")
		}
		cfg.SessionSecret = secret
	} else {
		cfg.SessionSecret = make([]byte, 32)
		if _, err := rand.Read(cfg.SessionSecret); err != nil {
			return nil, fmt.Errorf("generate session secret: %w", err)
		}
		cfg.SessionSecretGenerated = true
	}

	origins, err := parseOrigins(os.Getenv("SAG_ALLOWED_ORIGINS"))
	if err != nil {
		return nil, err
	}
	cfg.AllowedOrigins = origins
	cfg.ConsoleDir = os.Getenv("SAG_CONSOLE_DIR")
	cfg.ChatDir = os.Getenv("SAG_CHAT_DIR")
	cfg.PersonalBundleDir = os.Getenv("SAG_PERSONAL_BUNDLE")
	cfg.PersonalStateDir = os.Getenv("SAG_PERSONAL_STATE")
	cfg.MCPClientMetadataURL = strings.TrimSpace(os.Getenv("SAG_MCP_CLIENT_METADATA_URL"))
	cfg.DevSignInEmail = os.Getenv("SAG_DEV_SIGN_IN_EMAIL")
	cfg.DevSignInPassword = os.Getenv("SAG_DEV_SIGN_IN_PASSWORD")

	cfg.EncryptionKeys = os.Getenv("SAG_ENCRYPTION_KEYS")
	cfg.EncryptionPrimaryKeyID = envOr("SAG_ENCRYPTION_PRIMARY_KEY_ID", "1")
	if cfg.EncryptionKeys == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate encryption key: %w", err)
		}
		cfg.EncryptionKeys = cfg.EncryptionPrimaryKeyID + ":" + hex.EncodeToString(key)
		cfg.EncryptionKeysGenerated = true
	}
	return cfg, nil
}

// parseOrigins splits SAG_ALLOWED_ORIGINS and insists every entry is a real
// origin. "localhost:5174" without a scheme would never match what a browser
// sends, and an allowlist entry that can never match is a misconfiguration to
// report at boot, not a mystery to debug in a network tab.
func parseOrigins(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var origins []string
	for _, entry := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(entry)
		if origin == "" {
			continue
		}
		if !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
			return nil, fmt.Errorf("SAG_ALLOWED_ORIGINS entry %q is not an origin (scheme://host[:port])", origin)
		}
		origins = append(origins, origin)
	}
	return origins, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt reads a whole-number setting. A value that is not a number, or is not
// positive, falls back rather than starting the process on a nonsense one: zero
// goroutines is a worker that never works, and saying so at boot is better than
// a queue nobody drains.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// Local reports that this installation runs on somebody's own machine: a working
// copy or the personal edition.
//
// It answers the one question the OAuth client keeps needing, and it answers it
// from the SUBCOMMAND rather than by reading an address. Deciding it from the
// base URL meant sniffing for loopback and private ranges, which is a guess
// about a string; `sag dev` and `sag personal` already say it outright, and they
// are set where somebody typed them.
//
// What it decides: whether this install can publish a client document a remote
// authorization server could fetch. It cannot, so it borrows a published one.
func (c *Config) Local() bool { return c.Dev || c.Personal }
