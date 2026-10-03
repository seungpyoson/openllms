package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/goodtekxyz/openllms/internal/secrets"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/google/uuid"
)

type unreadableCredentials struct{ secrets.Client }

func (unreadableCredentials) Get(context.Context, string, string) (string, error) {
	return "", errors.New("injected credential read failure")
}

func TestRefreshRejectsCredentialReadFailure(t *testing.T) {
	eng := &Engine{Secrets: unreadableCredentials{}}
	// Unsupported vendor ensures a stale-token refresh would return a different
	// error, without making any external request.
	acc := &store.Account{ID: uuid.New(), Vendor: "unsupported", InfisicalPath: "/old"}
	_, err := eng.RefreshOAuthCredential(context.Background(), acc, secrets.CredentialJSON{AccessToken: "synthetic-old", RefreshToken: "synthetic-stale"})
	if err == nil || err.Error() != "secret_fetch" {
		t.Fatalf("want credential read failure, got %v", err)
	}
}

func TestLoadCredentialWithoutSecretStore(t *testing.T) {
	eng := &Engine{}
	_, _, err := eng.LoadCredential(context.Background(), &store.Account{ID: uuid.New()})
	if err == nil || err.Error() != "secrets_unavailable" {
		t.Fatalf("want missing-store error, got %v", err)
	}
}
