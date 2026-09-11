package app

import (
	"testing"
	"time"

	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/config"
)

// A personal installation must not expire its own session.
//
// Nothing here is protected by an expiry: the credential is possession of this
// computer's files, and the sign-in it would force asks for nothing. What an
// expiry does produce is somebody opening the application a month after
// installing it and being shown a login box for an account they never made.
func TestAPersonalSessionOutlivesAnybodyWhoWouldNotice(t *testing.T) {
	personal := &App{Config: &config.Config{Personal: true}}
	if got := personal.refreshTTL(); got < 10*365*24*time.Hour {
		t.Fatalf("a personal session lasts %s; it should outlast the installation", got)
	}

	// And a deployment keeps the expiry it needs. A laptop left on a train
	// should eventually stop being a way in.
	server := &App{Config: &config.Config{}}
	if got := server.refreshTTL(); got != auth.RefreshTokenTTL {
		t.Fatalf("a deployment session lasts %s, want %s", got, auth.RefreshTokenTTL)
	}
}
