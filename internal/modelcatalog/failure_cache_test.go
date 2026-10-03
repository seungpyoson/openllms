package modelcatalog_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goodtekxyz/openllms/internal/modelcatalog"
	"github.com/goodtekxyz/openllms/internal/proxy"
	"github.com/goodtekxyz/openllms/internal/secrets/memory"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/goodtekxyz/openllms/internal/vendorauth"
	"github.com/google/uuid"
)

func TestDiscoveryCachesFailuresUntilCredentialChanges(t *testing.T) {
	for _, status := range []int{401, 403, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var modelsHits, rotations atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				modelsHits.Add(1)
				if r.Header.Get("Authorization") != "Bearer repaired" {
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-test"}]}`))
			}))
			defer up.Close()
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rotations.Add(1)
				_, _ = w.Write([]byte(`{"access_token":"rotated","refresh_token":"rotated-refresh","expires_in":7200}`))
			}))
			defer token.Close()
			prior := vendorauth.ClaudeTokenURL
			vendorauth.ClaudeTokenURL = token.URL
			t.Cleanup(func() { vendorauth.ClaudeTokenURL = prior })
			ctx := context.Background()
			sec := memory.New()
			const path = "/synthetic/cache"
			if err := sec.Put(ctx, path, "credential", `{"access_token":"old","refresh_token":"refresh"}`); err != nil {
				t.Fatal(err)
			}
			d := modelcatalog.New(&proxy.Engine{Secrets: sec}, up.Client(), time.Minute)
			acc := store.Account{ID: uuid.New(), Vendor: "claude", Name: "cache", AuthType: "oauth", InfisicalPath: path, BaseURL: up.URL}
			route := &store.Route{Slug: "cache"}
			for i := 0; i < 3; i++ {
				cat := d.ForRoute(ctx, route, []store.Account{acc})
				if cat.Accounts[0].Status != "error" || len(cat.Models) != 0 {
					t.Fatal("failed discovery reported models")
				}
			}
			wantHits, wantRotations := int32(1), int32(0)
			if status == 401 {
				wantHits, wantRotations = 2, 1
			}
			if modelsHits.Load() != wantHits || rotations.Load() != wantRotations {
				t.Errorf("requests=%d rotations=%d; want %d and %d", modelsHits.Load(), rotations.Load(), wantHits, wantRotations)
			}
			if err := sec.Put(ctx, path, "credential", `{"access_token":"repaired","refresh_token":"new-refresh"}`); err != nil {
				t.Fatal(err)
			}
			cat := d.ForRoute(ctx, route, []store.Account{acc})
			if cat.Accounts[0].Status != "ok" || len(cat.Models) != 1 {
				t.Fatal("a changed credential remained stuck behind the cached error")
			}
		})
	}
}
