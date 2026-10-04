package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/goodtekxyz/openllms/internal/db"
	"github.com/goodtekxyz/openllms/internal/store"
)

func sqliteTimeDB(t *testing.T) *sql.DB {
	t.Helper()
	url := "sqlite:" + filepath.Join(t.TempDir(), "time.db")
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	database, err := db.ConnectSQLite(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// Exercise the actual driver's time.Time binding into TEXT, including the
// monotonic suffix it writes when handed time.Now(). Existing databases contain
// these values, so the reader must work without rewriting stored rows.
func TestSQLiteTimeDriverRoundTrip(t *testing.T) {
	database := sqliteTimeDB(t)
	adapter := store.NewSQLiteDB(database)
	ctx := context.Background()
	if _, err := database.Exec(`CREATE TABLE timestamp_roundtrip (value TEXT)`); err != nil {
		t.Fatal(err)
	}
	values := []time.Time{
		time.Date(2026, 10, 9, 21, 13, 25, 0, time.UTC),
		time.Date(2026, 10, 9, 21, 13, 25, 123456789, time.FixedZone("KST", 9*3600)),
		time.Date(2026, 10, 9, 21, 13, 25, 500, time.FixedZone("", -5*3600-1800)),
		time.Now(),
	}
	for _, want := range values {
		t.Run(want.String(), func(t *testing.T) {
			if _, err := adapter.Exec(ctx, `DELETE FROM timestamp_roundtrip`); err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Exec(ctx, `INSERT INTO timestamp_roundtrip VALUES ($1)`, &want); err != nil {
				t.Fatal(err)
			}
			var got *time.Time
			if err := adapter.QueryRow(ctx, `SELECT value FROM timestamp_roundtrip`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got == nil || !got.Equal(want) {
				t.Fatalf("got %v, want instant %v", got, want)
			}
			rows, err := adapter.Query(ctx, `SELECT value FROM timestamp_roundtrip`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("missing timestamp: %v", rows.Err())
			}
			var value time.Time
			if err := rows.Scan(&value); err != nil || !value.Equal(want) {
				t.Fatalf("rows scan got %v, want %v: %v", value, want, err)
			}
		})
	}
}

func TestSQLiteTimeNullAndInvalid(t *testing.T) {
	adapter := store.NewSQLiteDB(sqliteTimeDB(t))
	ctx := context.Background()
	var nullable *time.Time
	value := time.Now()
	if err := adapter.QueryRow(ctx, `SELECT NULL, NULL`).Scan(&nullable, &value); err != nil {
		t.Fatal(err)
	}
	if nullable != nil || !value.IsZero() {
		t.Fatal("NULL did not remain absent")
	}
	for _, invalid := range []string{
		"not-a-time", "2026-10-09 21:13:25 +0000 UTC garbage",
		"2026-10-09 21:13:25 +0000 UTC m=garbage",
		"2026-10-09 21:13:25 +0000 UTC m=+1.000000000 trailing",
	} {
		if err := adapter.QueryRow(ctx, `SELECT $1`, invalid).Scan(&value); err == nil {
			t.Errorf("accepted malformed timestamp %q", invalid)
		}
	}
}

func TestSQLiteProviderQuotaKeepsAccountReadable(t *testing.T) {
	database := sqliteTimeDB(t)
	st := store.NewSQLite(database)
	ctx := context.Background()
	project, _, err := st.Bootstrap(ctx, "quota", "quota", "quota")
	if err != nil {
		t.Fatal(err)
	}
	account, err := st.CreateAccount(ctx, project, "codex", "seat", "oauth", "/synthetic", "https://unused.invalid")
	if err != nil {
		t.Fatal(err)
	}
	route, err := st.CreateRoute(ctx, project, "quota", "sequential", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachAccount(ctx, route.ID, account.ID, 0, 1); err != nil {
		t.Fatal(err)
	}
	reset := time.Date(2026, 10, 9, 21, 13, 25, 0, time.UTC)
	reset5h, reset7d := reset.Add(5*time.Hour), reset.Add(7*24*time.Hour)
	remaining := 75.0
	if err := st.SetAccountQuotaWindowsID(ctx, account.ID, &remaining, &reset, &remaining, &reset5h, &remaining, &reset7d); err != nil {
		t.Fatal(err)
	}
	readers := map[string]func() ([]store.Account, error){
		"accounts":        func() ([]store.Account, error) { return st.ListAccounts(ctx, project) },
		"all accounts":    func() ([]store.Account, error) { return st.ListAllAccounts(ctx) },
		"oauth":           func() ([]store.Account, error) { return st.ListOAuthAccounts(ctx) },
		"project oauth":   func() ([]store.Account, error) { return st.ListOAuthAccountsByProject(ctx, project) },
		"route pool":      func() ([]store.Account, error) { return st.ListRoutePool(ctx, route.ID) },
		"usable accounts": func() ([]store.Account, error) { return st.ListRouteAccounts(ctx, route.ID) },
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			accounts, err := read()
			if err != nil {
				t.Fatal(err)
			}
			if len(accounts) != 1 || accounts[0].ID != account.ID {
				t.Fatalf("quota update lost the account: %v", accounts)
			}
			a := accounts[0]
			for i, pair := range [][2]*time.Time{{a.QuotaResetAt, &reset}, {a.Quota5hResetAt, &reset5h}, {a.Quota7dResetAt, &reset7d}} {
				if pair[0] == nil || !pair[0].Equal(*pair[1]) {
					t.Errorf("reset %d = %v, want %v", i, pair[0], pair[1])
				}
			}
		})
	}
}
