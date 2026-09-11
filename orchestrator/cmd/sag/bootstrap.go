package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// runBootstrap creates a workspace (if missing) and its first user. It is
// the dev/on-prem entry into an empty database:
//
//	sag bootstrap -workspace acme -workspace-name "Acme Inc" \
//	    -email admin@acme.com -user-name "Admin" -password secret
func runBootstrap(ctx context.Context, cfg *config.Config, st store.Store, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	slug := fs.String("workspace", "", "workspace slug (required)")
	wsName := fs.String("workspace-name", "", "workspace display name (defaults to slug)")
	email := fs.String("email", "", "user email (required)")
	userName := fs.String("user-name", "Admin", "user display name")
	password := fs.String("password", "", "user password (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" || *email == "" || *password == "" {
		return errors.New("bootstrap requires -workspace, -email, and -password")
	}
	if *wsName == "" {
		*wsName = *slug
	}

	ws, err := st.Workspaces().GetBySlug(ctx, *slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		ws = &model.Workspace{Slug: *slug, Name: *wsName}
		if err := st.Workspaces().Create(ctx, ws); err != nil {
			return fmt.Errorf("create workspace: %w", err)
		}
		fmt.Printf("workspace %q created (id %d)\n", ws.Slug, ws.ID)
		// Offered what this build can do, here rather than at the next boot.
		// Bootstrapping against a server that is ALREADY running is the normal
		// case for a deployment, and that server synced its tools before this
		// workspace existed: without this it would have none until somebody
		// restarted it, which is a thing nobody would think to do.
		a, err := app.New(cfg, zerolog.Nop(), st)
		if err != nil {
			return fmt.Errorf("offer the abilities: %w", err)
		}
		if err := a.SyncTools(ctx, ws.ID); err != nil {
			return fmt.Errorf("offer the abilities: %w", err)
		}
		fmt.Printf("abilities offered to %q\n", ws.Slug)
	case err != nil:
		return fmt.Errorf("load workspace: %w", err)
	default:
		fmt.Printf("workspace %q exists (id %d)\n", ws.Slug, ws.ID)
	}

	if _, err := st.Users().GetByEmail(ctx, *email); err == nil {
		return fmt.Errorf("user %s already exists", *email)
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("check user: %w", err)
	}

	hash, err := auth.HashPassword(*password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	user := &model.User{
		Email:        *email,
		Name:         *userName,
		PasswordHash: hash,
	}
	if err := st.Users().Create(ctx, user); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	// The person exists in the tenant; the membership is what lets them in.
	if err := st.Workspaces().SetMembers(ctx, user.ID, []int64{ws.ID}); err != nil {
		return fmt.Errorf("add user to workspace: %w", err)
	}
	fmt.Printf("user %s created (id %d)\n", user.Email, user.ID)

	// Without an administrator role the first user could not manage
	// anything, so bootstrap grants it: superuser role, administrators
	// group, user in the group.
	role, err := ensureAdminRole(ctx, st, ws.ID)
	if err != nil {
		return err
	}
	group, err := ensureAdminGroup(ctx, st, ws.ID)
	if err != nil {
		return err
	}
	if err := st.Groups().AssignRole(ctx, group.ID, role.ID); err != nil {
		return fmt.Errorf("assign admin role: %w", err)
	}
	if err := st.Groups().AddMember(ctx, group.ID, user.ID); err != nil {
		return fmt.Errorf("add user to admin group: %w", err)
	}
	fmt.Printf("user granted %q via group %q\n", model.BootstrapAdminRole, model.BootstrapAdminGroup)
	return nil
}

func ensureAdminRole(ctx context.Context, st store.Store, workspaceID int64) (*model.Role, error) {
	roles, err := st.Roles().List(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	for _, r := range roles {
		if r.Name == model.BootstrapAdminRole {
			return r, nil
		}
	}
	role := &model.Role{
		WorkspaceID: workspaceID,
		Name:        model.BootstrapAdminRole,
		Permissions: []string{model.PermSuperuser},
	}
	if err := st.Roles().Create(ctx, role); err != nil {
		return nil, fmt.Errorf("create admin role: %w", err)
	}
	return role, nil
}

func ensureAdminGroup(ctx context.Context, st store.Store, workspaceID int64) (*model.Group, error) {
	groups, err := st.Groups().List(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	for _, g := range groups {
		if g.Name == model.BootstrapAdminGroup {
			return g, nil
		}
	}
	group := &model.Group{WorkspaceID: workspaceID, Name: model.BootstrapAdminGroup}
	if err := st.Groups().Create(ctx, group); err != nil {
		return nil, fmt.Errorf("create admin group: %w", err)
	}
	return group, nil
}
