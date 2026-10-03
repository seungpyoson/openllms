package httpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goodtekxyz/openllms/internal/secrets/file"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/goodtekxyz/openllms/internal/vendor"
)

// Model a committed INSERT whose result is lost to the caller. The SQL and
// persisted row are real; only the ambiguous response failure is injected.
type ambiguousAccountDB struct {
	store.DB
	commit bool
}

func (d ambiguousAccountDB) QueryRow(ctx context.Context, query string, args ...any) store.Row {
	if strings.Contains(query, "INSERT INTO accounts") {
		if !d.commit {
			return ambiguousAccountRow{}
		}
		return ambiguousAccountRow{d.DB.QueryRow(ctx, query, args...)}
	}
	return d.DB.QueryRow(ctx, query, args...)
}

func (d ambiguousAccountDB) Begin(ctx context.Context) (store.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return ambiguousAccountTx{Tx: tx, commit: d.commit}, nil
}

type ambiguousAccountTx struct {
	store.Tx
	commit bool
}

func (tx ambiguousAccountTx) Commit(ctx context.Context) error {
	if tx.commit {
		if err := tx.Tx.Commit(ctx); err != nil {
			return err
		}
	}
	return context.DeadlineExceeded
}

type ambiguousAccountRow struct{ store.Row }

func (r ambiguousAccountRow) Scan(dest ...any) error {
	if r.Row != nil {
		if err := r.Row.Scan(dest...); err != nil {
			return err
		}
	}
	return context.DeadlineExceeded
}

func TestAccountCreationAmbiguousInsertKeepsCredential(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed", false: "not_visible"}[commit], func(t *testing.T) {
			f := newAccountFixture(t)
			f.srv.store = store.NewDB(ambiguousAccountDB{DB: store.NewSQLiteDB(f.db), commit: commit}, store.DefaultAuthCacheTTL, store.DefaultRouteCacheTTL)
			code, _ := f.create(context.Background(), false, "ambiguous", "synthetic-review")
			accounts, err := f.st.ListAccounts(context.Background(), f.project)
			if err != nil {
				t.Fatal(err)
			}
			wantAccounts := 0
			if commit {
				wantAccounts = 1
			}
			if code != 500 || len(accounts) != wantAccounts {
				t.Fatalf("status=%d committed accounts=%d", code, len(accounts))
			}
			for _, acc := range accounts {
				_, err := f.vault.Get(context.Background(), acc.InfisicalPath, vendor.SecretName)
				if err != nil {
					t.Errorf("committed account lost its credential after ambiguous insert response: %v", err)
				}
			}
			if f.secretFiles(t) != 1 {
				t.Error("uncertain commit must preserve the credential even if no row is yet visible")
			}
		})
	}
}

// Make the real filesystem rename fail after file.Client writes the temp file.
// No credential content is included in logs or assertions.
type renameFailureSecrets struct{ *file.Client }

func (s renameFailureSecrets) Put(ctx context.Context, path, name, value string) error {
	destination := filepath.Join(s.Root, strings.TrimPrefix(path, "/"), name+".secret")
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	return s.Client.Put(ctx, path, name, value)
}

func TestAccountCreationRealRenameFailureCleansStagingFile(t *testing.T) {
	f := newAccountFixture(t)
	f.srv.secret = renameFailureSecrets{f.vault}
	code, body := f.create(context.Background(), false, "rename", "synthetic-review")
	_, cleanupError := body["cleanup_error"]
	t.Logf("create status=%d; cleanup_error present=%v", code, cleanupError)
	if code < 400 {
		t.Fatalf("expected rename failure, status=%d", code)
	}
	if n := f.secretFiles(t); n != 0 {
		t.Errorf("failed create left %d orphan temp credential file(s)", n)
	}
}
