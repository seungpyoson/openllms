package modelcatalog_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goodtekxyz/openllms/internal/modelcatalog"
	"github.com/goodtekxyz/openllms/internal/proxy"
	"github.com/goodtekxyz/openllms/internal/secrets/memory"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/google/uuid"
)

func TestClientCancellationDoesNotPoisonSharedDiscovery(t *testing.T) {
	for _, expiredDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[expiredDeadline], func(t *testing.T) {
			var hits atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-synthetic"}]}`))
			}))
			defer up.Close()
			sec := memory.New()
			const path = "/synthetic/cancellation"
			if err := sec.Put(context.Background(), path, "credential", `{"access_token":"synthetic-only"}`); err != nil {
				t.Fatal(err)
			}
			engine := &proxy.Engine{Secrets: sec}
			discovery := modelcatalog.New(engine, up.Client(), time.Minute)
			acc := store.Account{ID: uuid.New(), Vendor: "claude", Name: "synthetic", AuthType: "oauth", InfisicalPath: path, BaseURL: up.URL}
			route := &store.Route{Slug: "synthetic"}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if expiredDeadline {
				canceled, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
			}
			first := discovery.ForRoute(canceled, route, []store.Account{acc})
			if first.Accounts[0].Status != "error" {
				t.Fatal("canceled request unexpectedly succeeded")
			}
			second := discovery.ForRoute(context.Background(), route, []store.Account{acc})
			t.Logf("fresh caller after cancellation: status=%s upstream_calls=%d", second.Accounts[0].Status, hits.Load())
			// Independent oracle: identical unchanged account/provider succeeds when
			// queried without the poisoned entry, so neither credentials nor provider
			// need repair to recover.
			beforeOracle := hits.Load()
			oracle := modelcatalog.New(engine, up.Client(), time.Minute).ForRoute(context.Background(), route, []store.Account{acc})
			if oracle.Accounts[0].Status != "ok" || hits.Load() != beforeOracle+1 {
				t.Fatal("healthy provider oracle did not succeed")
			}
			if second.Accounts[0].Status != "ok" {
				t.Error("one canceled caller poisoned other callers until cache expiry")
			}
		})
	}
}
