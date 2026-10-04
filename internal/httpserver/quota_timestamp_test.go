package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRouteListingRejectsUnreadableQuota(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	account, err := f.st.CreateAccount(ctx, f.project, "codex", "seat", "oauth", "/synthetic", "https://unused.invalid")
	if err != nil {
		t.Fatal(err)
	}
	route, err := f.st.CreateRoute(ctx, f.project, "quota", "sequential", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.AttachAccount(ctx, route.ID, account.ID, 0, 1); err != nil {
		t.Fatal(err)
	}
	reset := time.Date(2026, 10, 9, 21, 13, 25, 0, time.UTC)
	remaining := 75.0
	if err := f.st.SetAccountQuotaWindowsID(ctx, account.ID, &remaining, &reset, &remaining, &reset, &remaining, &reset); err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+f.key)
		w := httptest.NewRecorder()
		f.srv.Router().ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"/control/v1/accounts", "/control/v1/routes"} {
		w := get(path)
		if w.Code != http.StatusOK {
			t.Errorf("after provider quota write: %s status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
	var listed struct {
		Routes []struct {
			AccountIDs []string `json:"account_ids"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(get("/control/v1/routes").Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Routes) != 1 || len(listed.Routes[0].AccountIDs) != 1 || listed.Routes[0].AccountIDs[0] != account.ID.String() {
		t.Error("valid quota timestamp hid route membership")
	}
	// A malformed existing row must fail explicitly, never look like an empty pool.
	if _, err := f.db.Exec(`UPDATE accounts SET quota_reset_at = 'invalid' WHERE id = ?`, account.ID.String()); err != nil {
		t.Fatal(err)
	}
	if w := get("/control/v1/routes"); w.Code != http.StatusInternalServerError {
		t.Fatalf("unreadable quota status=%d, want 500; body=%s", w.Code, w.Body.String())
	}
}
