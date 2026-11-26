package users

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/store"
)

// QA: deleting a user takes the count of failed sign-ins on their name with it.
func TestDeleteAccountRemovesTheFailureCounter(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, name := range []string{"dave", "erin"} {
		if _, err := db.CreateAdmin(ctx, name, "hash", "secret"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.RegisterFailure(ctx, store.LoginFailedKey+name, 20, time.Hour, time.Hour, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		acc, err := store.AccountTx(ctx, tx, "dave")
		if err != nil {
			return err
		}
		return db.DeleteAccount(ctx, tx, acc)
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys string
	if err := db.QueryRow(`SELECT IFNULL(group_concat(key), '') FROM login_attempts`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != store.LoginFailedKey+"erin" {
		t.Fatalf("counters left: %q", keys)
	}
}
