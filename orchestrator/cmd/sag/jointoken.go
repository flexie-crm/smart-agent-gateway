package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/store/sqlstore"
)

// runJoinToken prints a token to start the next machine with.
//
//	sag join-token                        # for the deployment's first account
//	sag join-token -user someone@x.com    # for a particular person
//
// It exists on the command line as well as in the console for the case the
// console cannot serve: the FIRST machine, on a deployment where nobody has
// signed in yet. That is exactly when somebody is racking hardware, and sending
// them to create a user first would be sending them the long way round. Docker
// puts `swarm join-token` in the same place for the same reason.
//
// There is no `-rotate` any more, and nothing to rotate: every token is minted
// fresh, replaces the one that person had, lasts an hour, and is destroyed by
// the machine that uses it. Running this command IS the rotation.
//
// It prints the token and nothing else, so it can be read by a person or piped
// into a script without either having to strip a banner off it.
func runJoinToken(ctx context.Context, cfg *config.Config, st *sqlstore.SQLStore, args []string) error {
	fs := flag.NewFlagSet("join-token", flag.ContinueOnError)
	email := fs.String("user", "", "whose token this is; the deployment's first account by default")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// A token has an owner now, so this has to name one. Not a formality: it is
	// what makes "this token turned up somewhere it should not have" a question
	// with an answer.
	owner, err := joinTokenOwner(ctx, st, *email)
	if err != nil {
		return err
	}

	// Silent, because the only output of this command is the token.
	a, err := app.New(cfg, zerolog.Nop(), st)
	if err != nil {
		return err
	}

	token, err := a.MintJoinToken(ctx, owner)
	if err != nil {
		if errors.Is(err, app.ErrCredentialsUnavailable) {
			return errors.New("the join token is sealed with a key this server does not have")
		}
		return err
	}
	fmt.Println(token)
	return nil
}

// joinTokenOwner resolves whose token to mint.
//
// The default is the deployment's FIRST account, which on any deployment that
// has been set up at all is the person who set it up. It is a default and not a
// guess: `-user` says who, and a deployment with nobody in it is told so plainly
// rather than being handed a token attributed to nobody.
func joinTokenOwner(ctx context.Context, st *sqlstore.SQLStore, email string) (int64, error) {
	if email != "" {
		user, err := st.Users().GetByEmail(ctx, email)
		if err != nil {
			return 0, fmt.Errorf("there is no account here for %s", email)
		}
		return user.ID, nil
	}

	users, err := st.Users().List(ctx)
	if err != nil {
		return 0, err
	}
	if len(users) == 0 {
		return 0, errors.New("there is nobody on this deployment yet, so there is nobody to give a token to")
	}
	return users[0].ID, nil
}
