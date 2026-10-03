package httpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goodtekxyz/openllms/internal/secrets/file"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/goodtekxyz/openllms/internal/vendor"
	"github.com/jackc/pgx/v5"
)

// Model an uncertain COMMIT response with both possible durable outcomes.
// The transaction and row are real; only the response failure is injected.
type ambiguousAccountDB struct {
	store.DB
	commit bool
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

func TestAccountCreationAmbiguousInsertKeepsCredential(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed", false: "not_visible"}[commit], func(t *testing.T) {
			f := newAccountFixture(t)
			f.srv.store = store.NewDB(ambiguousAccountDB{DB: store.NewSQLiteDB(f.db), commit: commit}, store.DefaultAuthCacheTTL, store.DefaultRouteCacheTTL)
			code, body := f.create(context.Background(), false, "ambiguous", "synthetic-review")
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
			path, ok := body["credential_path"].(string)
			if !ok || body["credential_preserved"] != true {
				t.Error("uncertain response must identify the retained credential for reconciliation")
			} else if _, err := f.vault.Get(context.Background(), path, vendor.SecretName); err != nil {
				t.Error("reported credential path does not identify the preserved secret")
			}
		})
	}
}

type transactionFailureDB struct {
	store.DB
	commitErr   error
	rollbackErr error
	afterScan   func()
}

func (d transactionFailureDB) Begin(ctx context.Context) (store.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return transactionFailureTx{Tx: tx, fault: d}, nil
}

type transactionFailureTx struct {
	store.Tx
	fault transactionFailureDB
}

func (tx transactionFailureTx) Commit(ctx context.Context) error {
	if tx.fault.commitErr != nil {
		return tx.fault.commitErr
	}
	return tx.Tx.Commit(ctx)
}

func (tx transactionFailureTx) Rollback(ctx context.Context) error {
	return errors.Join(tx.Tx.Rollback(ctx), tx.fault.rollbackErr)
}

func (tx transactionFailureTx) QueryRow(ctx context.Context, query string, args ...any) store.Row {
	return afterScanRow{Row: tx.Tx.QueryRow(ctx, query, args...), afterScan: tx.fault.afterScan}
}

type afterScanRow struct {
	store.Row
	afterScan func()
}

func (r afterScanRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	if r.afterScan != nil {
		r.afterScan()
	}
	return err
}

type beforeWriteError struct{}

func (beforeWriteError) Error() string     { return "synthetic failure before sending COMMIT" }
func (beforeWriteError) SafeToRetry() bool { return true }

func TestAccountCreationKnownCommitFailuresCleanCredential(t *testing.T) {
	for _, kind := range []string{"before_write", "server_rollback", "canceled_before_commit"} {
		t.Run(kind, func(t *testing.T) {
			f := newAccountFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fault := transactionFailureDB{DB: store.NewSQLiteDB(f.db)}
			switch kind {
			case "before_write":
				fault.commitErr = beforeWriteError{}
			case "server_rollback":
				fault.commitErr = pgx.ErrTxCommitRollback
			case "canceled_before_commit":
				fault.afterScan = cancel
			}
			f.srv.store = store.NewDB(fault, store.DefaultAuthCacheTTL, store.DefaultRouteCacheTTL)
			code, _ := f.create(ctx, false, "known_failure", "synthetic")
			accounts, err := f.st.ListAccounts(context.Background(), f.project)
			if code != 500 || err != nil || len(accounts) != 0 || f.secretFiles(t) != 0 {
				t.Fatalf("known failure did not clean up: status=%d accounts=%d err=%v", code, len(accounts), err)
			}
		})
	}
}

func TestAccountCreationRollbackFailureIsReported(t *testing.T) {
	f := newAccountFixture(t)
	if code, _ := f.create(context.Background(), false, "duplicate", "original"); code != 201 {
		t.Fatalf("initial create=%d", code)
	}
	fault := transactionFailureDB{DB: store.NewSQLiteDB(f.db), rollbackErr: errors.New("synthetic rollback failure")}
	f.srv.store = store.NewDB(fault, store.DefaultAuthCacheTTL, store.DefaultRouteCacheTTL)
	code, body := f.create(context.Background(), false, "duplicate", "rejected")
	detail, _ := body["error"].(string)
	if code != 500 || !strings.Contains(detail, "account rollback failed") {
		t.Fatal("rollback failure was hidden behind a routine conflict response")
	}
	if f.credentials(t)["duplicate"] != "original" || f.secretFiles(t) != 1 {
		t.Fatal("rollback failure corrupted the original account or left the rejected secret")
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
