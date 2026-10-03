package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goodtekxyz/openllms/internal/config"
	"github.com/goodtekxyz/openllms/internal/db"
	"github.com/goodtekxyz/openllms/internal/secrets"
	"github.com/goodtekxyz/openllms/internal/secrets/file"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/goodtekxyz/openllms/internal/vendor"
	"github.com/google/uuid"
)

type accountFixture struct {
	srv     *Server
	st      *store.Store
	db      *sql.DB
	vault   *file.Client
	project uuid.UUID
	key     string
}

func newAccountFixture(t *testing.T) accountFixture {
	t.Helper()
	dir := t.TempDir()
	url := "sqlite:" + filepath.Join(dir, "test.db")
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	database, err := db.ConnectSQLite(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	vault, err := file.New(filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewSQLite(database)
	project, key, err := st.Bootstrap(context.Background(), "review", "review", "review")
	if err != nil {
		t.Fatal(err)
	}
	return accountFixture{New(st, config.Config{RateLimitPerMin: 1000}, vault), st, database, vault, project, key}
}

func (f accountFixture) create(ctx context.Context, api bool, name, token string) (int, map[string]any) {
	in := accountCreateInput{Vendor: "codex", Name: name, AuthType: "oauth", AccessToken: token, RefreshToken: "synthetic-refresh"}
	endpoint := "/console/api/accounts"
	if api {
		endpoint = "/control/v1/accounts"
	}
	raw, _ := json.Marshal(in)
	req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.key)
	w := httptest.NewRecorder()
	f.srv.Router().ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestAccountCreationPreservesLegacyCredential(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	path := "/llms/" + f.project.String() + "/accounts/codex/legacy"
	if err := f.vault.Put(ctx, path, vendor.SecretName, `{"access_token":"synthetic-legacy","refresh_token":"synthetic-refresh"}`); err != nil {
		t.Fatal(err)
	}
	acc, err := f.st.CreateAccount(ctx, f.project, "codex", "legacy", "oauth", path, "https://unused.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.create(ctx, true, "legacy", "synthetic-replacement"); code != http.StatusConflict {
		t.Fatalf("duplicate=%d", code)
	}
	_, token, err := f.srv.proxy.LoadCredential(ctx, acc)
	if err != nil || token != "synthetic-legacy" {
		t.Fatal("legacy credential changed or became unreadable")
	}
	if f.secretFiles(t) != 1 {
		t.Error("duplicate left an orphan secret")
	}
}

func (f accountFixture) credentials(t *testing.T) map[string]string {
	t.Helper()
	accounts, err := f.st.ListAccounts(context.Background(), f.project)
	if err != nil {
		t.Fatal(err)
	}
	// Reopen the durable file store: assertions must not pass through a cache.
	vault, err := file.New(f.vault.Root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, acc := range accounts {
		raw, err := vault.Get(context.Background(), acc.InfisicalPath, vendor.SecretName)
		if err != nil {
			t.Fatal(err)
		}
		var cred secrets.CredentialJSON
		if err := json.Unmarshal([]byte(raw), &cred); err != nil {
			t.Fatal(err)
		}
		out[acc.Name] = cred.AccessToken
	}
	return out
}

func (f accountFixture) secretFiles(t *testing.T) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(f.vault.Root, func(_ string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAccountCreationPreservesCredentials(t *testing.T) {
	for _, api := range []bool{false, true} {
		t.Run(fmt.Sprintf("api=%t", api), func(t *testing.T) {
			f := newAccountFixture(t)
			if code, _ := f.create(context.Background(), api, "default", "synthetic-first"); code != 201 {
				t.Fatalf("first status=%d", code)
			}
			code, _ := f.create(context.Background(), api, "default", "synthetic-second")
			got := f.credentials(t)
			if got["default"] != "synthetic-first" {
				t.Error("rejected duplicate overwrote existing credentials")
			}
			if code != http.StatusConflict {
				t.Errorf("duplicate status=%d want 409", code)
			}
			if len(got) != 1 || f.secretFiles(t) != 1 {
				t.Error("duplicate left an extra account or secret")
			}
		})
	}
}

func TestAccountCreationPathIsolation(t *testing.T) {
	f := newAccountFixture(t)
	names := []string{"work", "personal", "a/b", "a//b", "a/./b", " a/b "}
	for i, name := range names {
		if code, _ := f.create(context.Background(), true, name, fmt.Sprintf("synthetic-%d", i)); code != 201 {
			t.Fatalf("create %d status=%d", i, code)
		}
	}
	got := f.credentials(t)
	for i, name := range names {
		if got[name] != fmt.Sprintf("synthetic-%d", i) {
			t.Errorf("account %d lost its own credential", i)
		}
	}
	if f.secretFiles(t) != len(names) {
		t.Error("distinct accounts share a credential file")
	}
}

func TestAccountCreationConcurrentDuplicate(t *testing.T) {
	f := newAccountFixture(t)
	const count = 12
	start := make(chan struct{})
	type result struct {
		code  int
		token string
	}
	results := make(chan result, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			token := fmt.Sprintf("synthetic-%d", i)
			code, _ := f.create(context.Background(), i%2 == 0, "shared", token)
			results <- result{code, token}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	winningToken := ""
	for r := range results {
		if r.code == 201 {
			winners++
			winningToken = r.token
		} else if r.code != 409 {
			t.Errorf("losing create status=%d want 409", r.code)
		}
	}
	got := f.credentials(t)
	if winners != 1 || len(got) != 1 || got["shared"] != winningToken {
		t.Error("concurrent rejected create changed the winning credential")
	}
	if f.secretFiles(t) != 1 {
		t.Error("concurrent create left orphan credentials")
	}
}

type createFaultSecrets struct {
	secrets.Client
	afterPut    func()
	putError    bool
	deleteError bool
}

func (s createFaultSecrets) Put(ctx context.Context, path, name, value string) error {
	if err := s.Client.Put(ctx, path, name, value); err != nil {
		return err
	}
	if s.afterPut != nil {
		s.afterPut()
	}
	if s.putError {
		return errors.New("injected put failure after write")
	}
	return nil
}

func (s createFaultSecrets) Delete(ctx context.Context, path, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.deleteError {
		return errors.New("injected cleanup failure")
	}
	return s.Client.Delete(ctx, path, name)
}

func TestAccountCreationFailureCleanup(t *testing.T) {
	for _, kind := range []string{"put", "database", "unavailable_database", "canceled", "cleanup"} {
		t.Run(kind, func(t *testing.T) {
			f := newAccountFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fault := createFaultSecrets{Client: f.vault}
			switch kind {
			case "put":
				fault.putError = true
			case "database":
				fault.afterPut = func() {
					if _, err := f.db.Exec("DELETE FROM projects WHERE id = ?", f.project.String()); err != nil {
						t.Error(err)
					}
				}
			case "unavailable_database":
				fault.afterPut = func() { _ = f.db.Close() }
			case "canceled":
				fault.afterPut = cancel
			case "cleanup":
				fault.putError = true
				fault.deleteError = true
			}
			f.srv.secret = fault
			code, body := f.create(ctx, false, "failure", "synthetic-token")
			if code < 400 {
				t.Fatalf("failed operation returned success %d", code)
			}
			if kind == "cleanup" {
				if !strings.Contains(fmt.Sprint(body), "cleanup") {
					t.Error("cleanup failure was hidden")
				}
				if f.secretFiles(t) != 1 {
					t.Error("unresolved ownership or cleanup must preserve the credential")
				}
			} else if f.secretFiles(t) != 0 {
				t.Error("failed create left a secret on disk")
			}
		})
	}
}
