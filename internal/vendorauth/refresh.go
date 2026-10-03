package vendorauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// RefreshError preserves the token endpoint status without retaining a response
// body that could contain credentials.
type RefreshError struct {
	StatusCode int
}

func (e *RefreshError) Error() string {
	return fmt.Sprintf("oauth token endpoint returned HTTP %d", e.StatusCode)
}

// ClaudeRefresh exchanges a stored refresh_token for a new Claude access token.
func ClaudeRefresh(ctx context.Context, refreshToken string) (Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", ClaudeClientID)
	form.Set("refresh_token", refreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ClaudeTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := httpClient().Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return Tokens{}, &RefreshError{StatusCode: res.StatusCode}
	}
	return tokensFromRefreshJSON(b, refreshToken)
}

// CodexRefresh exchanges a stored refresh_token for a new Codex/ChatGPT access token.
func CodexRefresh(ctx context.Context, refreshToken string) (Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", CodexClientID)
	form.Set("refresh_token", refreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, CodexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := httpClient().Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return Tokens{}, &RefreshError{StatusCode: res.StatusCode}
	}
	return tokensFromRefreshJSON(b, refreshToken)
}

// Refresh dispatches to the vendor-specific refresh flow. Vendor is matched
// case-insensitively; both the display name (claude/codex) and the upstream
// name (anthropic/openai) are accepted.
func Refresh(ctx context.Context, vendor, refreshToken string) (Tokens, error) {
	switch strings.ToLower(strings.TrimSpace(vendor)) {
	case "claude", "anthropic":
		return ClaudeRefresh(ctx, refreshToken)
	case "codex", "openai":
		return CodexRefresh(ctx, refreshToken)
	default:
		return Tokens{}, fmt.Errorf("no oauth refresh for vendor %q", vendor)
	}
}

// tokensFromRefreshJSON parses an oauth refresh response, falling back to the
// prior refresh_token when the upstream omits rotation (common for refresh
// grants) and preserving ChatGPTAccountID extraction from the new tokens.
func tokensFromRefreshJSON(b []byte, prevRefreshToken string) (Tokens, error) {
	tok, err := tokensFromOAuthJSON(b)
	if err != nil {
		return Tokens{}, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = prevRefreshToken
	}
	return tok, nil
}
