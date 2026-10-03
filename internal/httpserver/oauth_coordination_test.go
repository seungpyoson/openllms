package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goodtekxyz/openllms/internal/config"
	"github.com/goodtekxyz/openllms/internal/secrets"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/goodtekxyz/openllms/internal/vendorauth"
	"github.com/google/uuid"
)

func TestOAuthRefreshSharedAcrossConsumers(t *testing.T) {
	f := newAccountFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldRequests := make(chan struct{}, 2)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer synthetic-old" {
			oldRequests <- struct{}{}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-new" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-test"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"id":"message","content":[{"type":"text","text":"ok"}]}`))
		}
	}))
	defer up.Close()
	var refreshes atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-new", "refresh_token": "synthetic-rotated", "expires_in": 7200})
	}))
	defer token.Close()
	defer unblock()
	prior := vendorauth.ClaudeTokenURL
	vendorauth.ClaudeTokenURL = token.URL
	t.Cleanup(func() { vendorauth.ClaudeTokenURL = prior })
	acc, code, _ := f.srv.createAccountForProjectInner(ctx, f.project, accountCreateInput{
		Vendor: "claude", Name: "shared", AuthType: "oauth", BaseURL: up.URL,
		AccessToken: "synthetic-old", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	if code != http.StatusCreated {
		t.Fatalf("create=%d", code)
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		res, err := f.srv.proxy.CallAnthropicAccountForTest(ctx, acc, []byte(`{"model":"claude-test","messages":[]}`))
		if err != nil || res == nil || res.StatusCode != 200 {
			t.Error("inference refresh failed")
		}
	}()
	go func() {
		defer wg.Done()
		cat := f.srv.models.ForRoute(ctx, &store.Route{ID: uuid.New()}, []store.Account{*acc})
		if len(cat.Accounts) != 1 || cat.Accounts[0].Status != "ok" || cat.Accounts[0].Error != "" {
			t.Error("model discovery refresh failed")
		}
	}()
	go func() {
		defer wg.Done()
		updated, failed, _ := f.srv.proxy.RefreshExpiringOAuth(ctx, []store.Account{*acc}, time.Hour)
		if updated != 1 || failed != 0 {
			t.Error("proactive refresh failed")
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-oldRequests:
		case <-ctx.Done():
			t.Error("consumers did not both reach the expired credential")
		}
	}
	unblock()
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Errorf("refreshes=%d want 1 across all consumers", refreshes.Load())
	}
	if f.credentials(t)["shared"] != "synthetic-new" {
		t.Error("rotated credential was not persisted")
	}
}

type refreshFaultSecrets struct {
	secrets.Client
	failing atomic.Bool
}

func (s *refreshFaultSecrets) Put(ctx context.Context, path, name, value string) error {
	if s.failing.Load() {
		return context.DeadlineExceeded
	}
	return s.Client.Put(ctx, path, name, value)
}

func TestDiscoveryRefreshPersistenceFailureRecoversThroughInference(t *testing.T) {
	f := newAccountFixture(t)
	sec := &refreshFaultSecrets{Client: f.vault}
	f.srv = New(f.st, config.Config{}, sec)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-new" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-test"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"id":"message"}`))
		}
	}))
	defer up.Close()
	var rotations atomic.Int32
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rotations.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"synthetic-new","refresh_token":"synthetic-rotated","expires_in":7200}`))
	}))
	defer token.Close()
	prior := vendorauth.ClaudeTokenURL
	vendorauth.ClaudeTokenURL = token.URL
	t.Cleanup(func() { vendorauth.ClaudeTokenURL = prior })
	ctx := context.Background()
	acc, code, _ := f.srv.createAccountForProjectInner(ctx, f.project, accountCreateInput{
		Vendor: "claude", Name: "storage-failure", AuthType: "oauth", BaseURL: up.URL,
		AccessToken: "synthetic-old", RefreshToken: "synthetic-refresh",
	})
	if code != 201 {
		t.Fatalf("create=%d", code)
	}
	sec.failing.Store(true)
	route := &store.Route{ID: uuid.New()}
	cat := f.srv.models.ForRoute(ctx, route, []store.Account{*acc})
	if cat.Accounts[0].Status != "error" || len(cat.Models) != 0 {
		t.Fatal("failed credential persistence reported successful discovery")
	}
	sec.failing.Store(false)
	res, err := f.srv.proxy.CallAnthropicAccountForTest(ctx, acc, []byte(`{"model":"claude-test","messages":[]}`))
	if err != nil || res == nil || res.StatusCode != 200 {
		t.Fatal("inference could not persist discovery's pending credential")
	}
	cat = f.srv.models.ForRoute(ctx, route, []store.Account{*acc})
	if cat.Accounts[0].Status != "ok" || cat.Accounts[0].Error != "" {
		t.Fatal("discovery did not recover after persistence")
	}
	if rotations.Load() != 1 {
		t.Errorf("rotations=%d want 1", rotations.Load())
	}
	if f.credentials(t)["storage-failure"] != "synthetic-new" {
		t.Fatal("new credential not durable")
	}
}
