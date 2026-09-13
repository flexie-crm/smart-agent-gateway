package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The identity a desktop installation is seeded with. One person, one
// workspace, and an address that is deliberately not a real one: nothing is ever
// sent to it, and a real address here would look like an account somebody could
// sign in to from somewhere else.
const (
	personalWorkspaceSlug = "default"
	personalWorkspaceName = "Default"
	// It has to be a VALID address, because the ordinary user endpoint validates
	// one and "owner@localhost" is not (no domain): renaming yourself came back
	// "a valid email is required". `localhost.fx` satisfies that and says whose
	// it is, which a person reading their own row can make sense of.
	//
	// The trade against `.invalid`, which RFC 2606 guarantees will never resolve:
	// `.fx` is merely undelegated today rather than reserved forever. Nothing is
	// ever sent here and nothing looks it up, so what the name buys is being
	// legible, and that is worth more than a guarantee against a lookup that does
	// not happen. Migration 52 renames installations seeded with the old one.
	personalOwnerEmail = "owner@localhost.fx"
	personalOwnerName  = "Owner"
	// ownerKeyFile holds the seeded owner's password. Generated once, never
	// shown, and never typed: the Enter button is the server reading this file
	// on the caller's behalf.
	ownerKeyFile = "owner.key"
)

// loadPersonalSecrets gives this installation keys that outlive a restart.
//
// A server is handed SAG_SESSION_SECRET and SAG_ENCRYPTION_KEYS by whoever
// deploys it. Nobody deploys a desktop, so without this the config generates a
// fresh pair on every boot and logs a warning nobody sees, and the second launch
// finds every sealed value unreadable: vendor keys, machine keys, the authority
// every machine's certificate chains to. It is the same mistake as losing the
// deployment's key file, made automatically, once a day.
//
// So they are made once and kept beside the database they protect. It has to run
// before the app is built, because that is where the keyring is assembled.
func loadPersonalSecrets(cfg *config.Config, stateDir string) error {
	session, err := keepSecret(filepath.Join(stateDir, "session.key"), 32)
	if err != nil {
		return err
	}
	raw, err := hex.DecodeString(session)
	if err != nil {
		return fmt.Errorf("the session key is unreadable: %w", err)
	}
	cfg.SessionSecret, cfg.SessionSecretGenerated = raw, false

	sealing, err := keepSecret(filepath.Join(stateDir, "encryption.key"), 32)
	if err != nil {
		return err
	}
	// The same "id:key" shape a deployment writes, so the keyring, its rotation
	// and its rewrap behave here exactly as they do on a server.
	cfg.EncryptionKeys = "1:" + sealing
	cfg.EncryptionPrimaryKeyID = "1"
	cfg.EncryptionKeysGenerated = false
	return nil
}

// keepSecret reads a hex secret, making it the first time.
func keepSecret(path string, size int) (string, error) {
	existing, err := os.ReadFile(path) //nolint:gosec // a path this process built
	if err == nil {
		if value := strings.TrimSpace(string(existing)); value != "" {
			return value, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}

	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate %s: %w", filepath.Base(path), err)
	}
	value := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return "", fmt.Errorf("store %s: %w", filepath.Base(path), err)
	}
	return value, nil
}

// setupMarker records that a first run finished: the database created, every
// migration applied, the owner seeded.
const setupMarker = "setup-complete"

// firstRunFinished reports whether this installation has ever completed a start.
func firstRunFinished(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, setupMarker))
	return err == nil
}

func markFirstRunFinished(stateDir string) {
	_ = os.WriteFile(filepath.Join(stateDir, setupMarker), []byte("1\n"), 0o600)
}

// discardUnfinishedFirstRun throws away a database whose first run never
// finished, so the next start begins from nothing.
//
// This exists because of what a migration IS in MySQL: DDL auto-commits, so
// goose cannot roll an ALTER TABLE back. Quit the application while migration 8
// is running and the table is half-altered with nothing recording it, and every
// later start fails on the same migration forever. A marker that says only "the
// server was bootstrapped" does not catch that, because the server was.
//
// Wiping is safe ONLY here, and the marker is what makes it safe: it is written
// when a start has fully succeeded, so its absence means nothing has ever worked
// and there is nothing anybody could have put in it. Once it exists this never
// runs again, and a later failure is reported rather than deleted.
func discardUnfinishedFirstRun(stateDir string, logger zerolog.Logger) error {
	if firstRunFinished(stateDir) {
		return nil
	}
	database := filepath.Join(stateDir, "database")
	if _, err := os.Stat(database); err != nil {
		return nil // nothing there yet, which is the ordinary first run
	}
	logger.Warn().Msg("the previous setup did not finish, starting it again from the beginning")
	if err := os.RemoveAll(database); err != nil {
		return fmt.Errorf("clear the unfinished database: %w", err)
	}
	return nil
}

// requireLoopback refuses to serve a desktop on an address other machines can
// reach.
//
// This is the condition the passwordless sign-in rests on, and it is checked
// rather than documented. A desktop seeds an owner who holds every permission
// and offers a sign-in with no password, which is safe exactly while the only
// thing that can reach the port is somebody already on the machine. Bound to
// 0.0.0.0 it is an open administrative console, and that must not be one
// mistyped environment variable away.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("desktop mode cannot understand the address %q: %w", addr, err)
	}
	// An empty host is what ":8080" means, and it means every address.
	if host == "" {
		return fmt.Errorf(
			"desktop mode will not listen on %q, which is every address on this machine. "+
				"It signs in without a password, so it must only be reachable from here: "+
				"use 127.0.0.1:<port>", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf(
				"desktop mode will not listen on %s, which other machines can reach. "+
					"It signs in without a password: use 127.0.0.1:<port>", host)
		}
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	return fmt.Errorf(
		"desktop mode will not listen on %q, because it cannot tell that only this "+
			"machine can reach it: use 127.0.0.1:<port>", host)
}

// seedPersonalOwner makes the person this installation belongs to, once.
//
// Nothing about the permission system is bypassed or simplified. The owner is
// seeded into the same admin role and group `sag bootstrap` uses, so every check
// runs exactly as it does on a server and passes because this identity holds
// everything. A desktop that hid the permission system would be a second system
// to keep correct; this is the same one with one member.
func seedPersonalOwner(ctx context.Context, cfg *config.Config, st store.Store, stateDir string, logger zerolog.Logger) error {
	if _, err := st.Users().GetByEmail(ctx, personalOwnerEmail); err == nil {
		// Already seeded. The password file must still be readable, or the
		// Enter button has nothing to present.
		if _, err := ownerPassword(stateDir); err != nil {
			return err
		}
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("look for the owner: %w", err)
	}

	password, err := ownerPassword(stateDir)
	if err != nil {
		return err
	}

	// The built-in tools this build ships, filled in when the workspace is new.
	// Empty on an installation that already exists, and seedGateway then leaves
	// an existing Gateway exactly as somebody configured it.
	var builtins []string

	ws, err := st.Workspaces().GetBySlug(ctx, personalWorkspaceSlug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		ws = &model.Workspace{Slug: personalWorkspaceSlug, Name: personalWorkspaceName}
		if err := st.Workspaces().Create(ctx, ws); err != nil {
			return fmt.Errorf("create the workspace: %w", err)
		}
		a, err := app.New(cfg, zerolog.Nop(), st)
		if err != nil {
			return fmt.Errorf("offer the abilities: %w", err)
		}
		// The tools this build ships, offered to the new workspace now rather
		// than at some later restart nobody would think to perform.
		if err := a.SyncTools(ctx, ws.ID); err != nil {
			return fmt.Errorf("offer the abilities: %w", err)
		}
		// Kept, so the Gateway below can be given them. Read from the registry
		// rather than from the rows just written, because the rows include the
		// tools that are deliberately not in the catalogue and a person would
		// not recognise a grant they cannot see.
		builtins = builtinNames(a)
	case err != nil:
		return fmt.Errorf("load the workspace: %w", err)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash the password: %w", err)
	}
	user := &model.User{Email: personalOwnerEmail, Name: personalOwnerName, PasswordHash: hash}
	if err := st.Users().Create(ctx, user); err != nil {
		return fmt.Errorf("create the owner: %w", err)
	}
	if err := st.Workspaces().SetMembers(ctx, user.ID, []int64{ws.ID}); err != nil {
		return fmt.Errorf("add the owner to the workspace: %w", err)
	}

	role, err := ensureAdminRole(ctx, st, ws.ID)
	if err != nil {
		return err
	}
	group, err := ensureAdminGroup(ctx, st, ws.ID)
	if err != nil {
		return err
	}
	if err := st.Groups().AssignRole(ctx, group.ID, role.ID); err != nil {
		return fmt.Errorf("assign the administrator role: %w", err)
	}
	if err := st.Groups().AddMember(ctx, group.ID, user.ID); err != nil {
		return fmt.Errorf("add the owner to the administrators: %w", err)
	}

	brainID, err := seedWorkingMemory(ctx, st, ws.ID)
	if err != nil {
		return err
	}
	if err := seedGateway(ctx, st, ws.ID, brainID, builtins); err != nil {
		return err
	}

	logger.Info().Str("workspace", ws.Slug).Msg("this installation is ready")
	return nil
}

// seedWorkingMemory makes the brain the Gateway remembers into.
//
// Created here rather than asked for, because "would you like your assistant to
// remember things" is not a question with two reasonable answers. It is the
// working memory: the Gateway reads it and writes back to it, so what it learns
// in one conversation is there in the next.
//
// Unlocked, because the whole point is that the agent writes to it. A locked
// brain is for something an administrator maintains by hand.
func seedWorkingMemory(ctx context.Context, st store.Store, workspaceID int64) (int64, error) {
	brains, err := st.Brains().Brains(ctx, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("look for a working memory: %w", err)
	}
	for _, b := range brains {
		if b.Slug == workingMemorySlug {
			return b.ID, nil
		}
	}
	brain := &model.Brain{
		WorkspaceID: workspaceID,
		Name:        "Working memory",
		Slug:        workingMemorySlug,
		Description: "What the assistant has learned and should remember between conversations.",
	}
	if err := st.Brains().CreateBrain(ctx, brain); err != nil {
		return 0, fmt.Errorf("create the working memory: %w", err)
	}
	return brain.ID, nil
}

// workingMemorySlug identifies it, so a second start finds the one that exists
// rather than making another.
const workingMemorySlug = "working-memory"

// seedGateway makes the agent every turn goes through.
//
// Nothing in this codebase creates it: eight places read it and none writes it,
// because on a deployment an administrator makes it on the Agents screen. Nobody
// is going to do that on their own laptop, and without it setup cannot finish at
// all: the model gets created, there is no Gateway to point at it, and the
// screen sits there having done half the job.
//
// It is made with no model on purpose. Choosing one is what setup is for, and a
// Gateway pointed at a model nobody chose would make the first screen a lie.
// builtinNames is every built-in this build ships that a person would see in
// the catalogue, in a stable order.
//
// From the registry and not from the tools table, because the table also holds
// the ones deliberately kept out of the catalogue (set_model_status,
// list_models): granting a tool nobody can see is a setting nobody can undo.
func builtinNames(a *app.App) []string {
	var names []string
	for _, t := range a.Tools.All() {
		if t.Schema.Kind == tool.KindBuiltin && !t.Schema.Hidden {
			names = append(names, t.Schema.Name)
		}
	}
	sort.Strings(names)
	return names
}

func seedGateway(ctx context.Context, st store.Store, workspaceID, brainID int64, builtins []string) error {
	if _, err := st.Agents().GetByKey(ctx, workspaceID, model.DefaultAgentKey); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("look for the Gateway: %w", err)
	}
	if err := st.Agents().Create(ctx, newGatewayAgent(workspaceID, brainID, builtins)); err != nil {
		return fmt.Errorf("create the Gateway: %w", err)
	}
	return nil
}

// newGatewayAgent is what a fresh installation's Gateway IS, apart from a
// database, so the defaults can be read and tested in one place.
func newGatewayAgent(workspaceID, brainID int64, builtins []string) *model.Agent {
	return &model.Agent{
		WorkspaceID: workspaceID,
		Key:         model.DefaultAgentKey,
		Name:        "Gateway",
		Status:      model.StatusActive,
		// It can read this one and it writes back to it, which is what makes it
		// remember anything between conversations. Nobody is asked to set this
		// up: an assistant that forgets everything is not a lesser assistant,
		// it is a different and worse product.
		Brains:        []int64{brainID},
		MemoryBrainID: &brainID,

		// Everything this build ships, on.
		//
		// A deployment starts an assistant with nothing and an administrator
		// decides what it may do. Nobody is the administrator of their own
		// laptop: an assistant that can read no file and run no command is not a
		// cautious product, it is one that does nothing until somebody finds the
		// screen that turns it on.
		Tools: builtins,

		// The three that change something on the person's own disk. Reading,
		// searching and asking the time are answers; writing a file and running
		// a command are ACTIONS, and an action on somebody's own machine is
		// worth a look before it happens. This only ever ADDS friction: a tool
		// the code already declares dangerous stays that way regardless.
		ConfirmTools: []string{machine.TerminalName, machine.WriteFileName, machine.EditFileName},

		// Think as hard as the model will.
		//
		// The cost of thinking is the person's own and they chose the model; the
		// cost of a shallow answer is a wrong one about their own data. A model
		// with no such setting ignores it.
		Reasoning: true,
		Settings:  model.Settings{"reasoning_effort": "max"},
	}
}

// ownerPassword reads the seeded owner's password, making it on first call.
//
// It is a real password against a real hash: the local sign-in calls the same
// Login every other client calls, so there is no second authentication path to
// keep correct and nothing about sessions, rotation or revocation is special
// here. What the Enter button removes is the typing, not the check.
//
// The credential is therefore possession of this file, which is the same trust
// boundary as the database key sitting beside it: whoever can read this
// directory is already this person.
func ownerPassword(stateDir string) (string, error) {
	path := filepath.Join(stateDir, ownerKeyFile)
	existing, err := os.ReadFile(path) //nolint:gosec // a path this process built
	if err == nil {
		if password := strings.TrimSpace(string(existing)); password != "" {
			return password, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read the owner key: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate the owner key: %w", err)
	}
	password := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(password), 0o600); err != nil {
		return "", fmt.Errorf("store the owner key: %w", err)
	}
	return password, nil
}

// personalBaseURL is the address this installation calls itself by.
//
// Everything absolute we hand to somebody ELSE is built from it, and the one
// that decides whether a feature works is the OAuth redirect: the address a
// remote service sends a person's browser back to when they have approved. A
// deployment has a name somebody configured and typed into DNS. A personal
// installation has whatever port was free when it started, so the only honest
// answer is the address it has just been told to bind.
//
// Left to the default it said `http://localhost:8080`, which is a deployment's
// answer given by an application. On the machine where this was found there
// happened to BE a gateway on 8080, which received a consent it had never
// issued and refused it ("the state is not ours"); on any other machine it is a
// browser failing to connect, after the person has already approved. Either way
// the approval is spent and the connection is not made.
//
// An explicit SAG_BASE_URL still wins, on the same reasoning as the listen
// address: unset means we choose, set means somebody meant it.
func personalBaseURL(explicit, httpAddr string) string {
	if explicit != "" {
		return explicit
	}
	// A listen address may name no host at all (":8080" means every interface),
	// and "http://:8080" is not an address anybody can be sent back to. For a
	// callback the person's own browser follows, loopback is the right reading
	// of "wherever this is running".
	if strings.HasPrefix(httpAddr, ":") {
		return "http://localhost" + httpAddr
	}
	// Otherwise safe to build by hand: requireLoopback has already established
	// that the address has a host and a port, and that the host is one of ours.
	return "http://" + httpAddr
}
