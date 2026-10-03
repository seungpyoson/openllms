package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/goodtekxyz/openllms/internal/secrets"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/goodtekxyz/openllms/internal/vendor"
	"github.com/goodtekxyz/openllms/internal/vendorauth"
	"github.com/google/uuid"
)

type accountCreateInput struct {
	Vendor       string `json:"vendor"`
	Name         string `json:"name"`
	APIKey       string `json:"api_key"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	IDToken      string `json:"id_token"`
	ChatGPTAcct  string `json:"chatgpt_account_id"`
	AuthType     string `json:"auth_type"`
	BaseURL      string `json:"base_url"`
	DefaultModel string `json:"default_model"`
}

// createAccountForProjectInner creates an upstream account; caller must gate plan capacity.
func (s *Server) createAccountForProjectInner(ctx context.Context, projectID uuid.UUID, in accountCreateInput) (account *store.Account, status int, errBody map[string]any) {
	if s.secret == nil {
		return nil, http.StatusServiceUnavailable, map[string]any{
			"error": "infisical_not_configured",
			"hint":  "See docs/ops/HUMAN-SETUP.md",
		}
	}
	if in.Vendor == "" {
		return nil, http.StatusBadRequest, map[string]any{"error": "vendor required"}
	}
	if in.Name == "" {
		in.Name = "default"
	}
	if in.AuthType == "" {
		if in.AccessToken != "" {
			in.AuthType = "oauth"
		} else {
			in.AuthType = "api_key"
		}
	}
	if in.AuthType == "api_key" && in.APIKey == "" {
		return nil, http.StatusBadRequest, map[string]any{"error": "api_key required for auth_type=api_key"}
	}
	if in.AuthType == "oauth" && in.AccessToken == "" {
		return nil, http.StatusBadRequest, map[string]any{"error": "access_token required for auth_type=oauth"}
	}
	if in.BaseURL == "" {
		in.BaseURL = vendor.DefaultBaseURLFor(in.Vendor, in.AuthType)
	}
	// Names are user-controlled and may collide after file-path normalization.
	// Every create owns a fresh secret; the database arbitrates name uniqueness.
	path := vendor.CredentialPath(projectID, uuid.New())
	var createErr error
	defer func() {
		if account != nil {
			return
		}
		// A canceled request must still clean up its own partially written secret.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if errors.Is(createErr, store.ErrAccountCommitUnknown) {
			// A follow-up read cannot rule out a commit still completing elsewhere.
			errBody["error"] = "account_creation_result_unknown"
			errBody["detail"] = createErr.Error()
			errBody["credential_preserved"] = true
			errBody["credential_path"] = path
			return
		}
		if err := s.secret.Delete(cleanupCtx, path, vendor.SecretName); err != nil {
			status = http.StatusInternalServerError
			errBody["cleanup_error"] = err.Error()
		}
	}()
	cred := secrets.CredentialJSON{
		APIKey: in.APIKey, AccessToken: in.AccessToken, RefreshToken: in.RefreshToken,
		ExpiresAt: in.ExpiresAt, IDToken: in.IDToken, ChatGPTAccountID: in.ChatGPTAcct,
	}
	raw, _ := json.Marshal(cred)
	if err := s.secret.Put(ctx, path, vendor.SecretName, string(raw)); err != nil {
		return nil, http.StatusBadGateway, map[string]any{"error": "infisical_put_failed", "detail": err.Error()}
	}
	var acc *store.Account
	acc, createErr = s.store.CreateAccount(ctx, projectID, in.Vendor, in.Name, in.AuthType, path, in.BaseURL, in.DefaultModel)
	// Only an unwrapped conflict means the required rollback also succeeded.
	if createErr == store.ErrAccountExists {
		return nil, http.StatusConflict, map[string]any{"error": "account_already_exists"}
	}
	if createErr != nil {
		return nil, http.StatusInternalServerError, map[string]any{"error": createErr.Error()}
	}
	return acc, http.StatusCreated, nil
}

func tokensToAccountInput(vendor string, name string, t vendorauth.Tokens) accountCreateInput {
	return accountCreateInput{
		Vendor:       vendor,
		Name:         name,
		AuthType:     "oauth",
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    t.ExpiresAt,
		IDToken:      t.IDToken,
		ChatGPTAcct:  t.ChatGPTAccountID,
	}
}
