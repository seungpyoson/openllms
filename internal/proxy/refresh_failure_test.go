package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goodtekxyz/openllms/internal/router"
	"github.com/goodtekxyz/openllms/internal/secrets"
	"github.com/goodtekxyz/openllms/internal/secrets/memory"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/goodtekxyz/openllms/internal/vendor"
	"github.com/goodtekxyz/openllms/internal/vendorauth"
	"github.com/google/uuid"
)

func TestOAuthRefreshFailureClassification(t *testing.T) {
	for _, mode := range []string{"claude", "codex", "codex_images"} {
		for _, tokenStatus := range []int{400, 401, 403, 429, 500, 200} {
			t.Run(fmt.Sprintf("%s/%d", mode, tokenStatus), func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusUnauthorized)
				}))
				defer up.Close()
				token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tokenStatus)
					if tokenStatus == 200 {
						_, _ = w.Write([]byte(`{"access_token":"new","refresh_token":"new-refresh"}`))
					} else {
						_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
					}
				}))
				defer token.Close()
				claudeURL, codexURL := vendorauth.ClaudeTokenURL, vendorauth.CodexTokenURL
				vendorauth.ClaudeTokenURL, vendorauth.CodexTokenURL = token.URL, token.URL
				t.Cleanup(func() { vendorauth.ClaudeTokenURL, vendorauth.CodexTokenURL = claudeURL, codexURL })
				inner := memory.New()
				const path = "/synthetic/failure"
				if err := inner.Put(context.Background(), path, vendor.SecretName, `{"access_token":"old","refresh_token":"old-refresh"}`); err != nil {
					t.Fatal(err)
				}
				var sec secrets.Client = inner
				if tokenStatus == 200 {
					sec = &flakyPut{inner: inner, failLeft: 100}
				}
				vendorName := mode
				if mode == "codex_images" {
					vendorName = "codex"
				}
				acc := &store.Account{ID: uuid.New(), Vendor: vendorName, AuthType: "oauth", InfisicalPath: path, BaseURL: up.URL}
				e := &Engine{Secrets: sec, HTTP: up.Client()}
				var res *Result
				var err error
				if mode == "codex_images" {
					res, err = e.callResponsesImages(context.Background(), acc, []byte(`{"model":"gpt-image-1","prompt":"synthetic"}`))
				} else {
					res, err = e.doUpstream(context.Background(), acc, []byte(`{"model":"synthetic"}`), up.URL, "Authorization", "Bearer ", true)
				}
				want := http.StatusBadGateway
				if tokenStatus == 400 || tokenStatus == 401 || tokenStatus == 403 {
					want = http.StatusUnauthorized
				}
				if err != nil || res == nil || res.StatusCode != want || res.Error != "oauth_refresh_failed" {
					t.Fatalf("status/result=%+v err=%v; want status=%d", res, err, want)
				}
				if want == 401 && router.CooldownForStatus(res.StatusCode, 0) != 5*time.Minute {
					t.Fatal("revoked credential lost its authentication cooldown")
				}
			})
		}
	}
}
