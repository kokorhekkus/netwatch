// Package store persists samples and rolls them up.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type DB struct {
	w *sql.DB // single writer
	r *sql.DB // read-only, for the API
}

// dsn builds a connection string carrying the pragmas.
//
// modernc applies pragmas per connection, so setting them with a one-off Exec
// leaves every other pooled connection unconfigured. Putting them in the DSN
// is the only way they reliably apply.
func dsn(path string, readOnly bool) string {
	pragmas := []string{
		"journal_mode(WAL)",
		"busy_timeout(5000)",
		"synchronous(1)",
		"foreign_keys(1)",
		"temp_store(2)",
		"cache_size(-32000)",
	}
	v := url.Values{}
	for _, p := range pragmas {
		v.Add("_pragma", p)
	}
	if readOnly {
		v.Set("mode", "ro")
	}
	return "file:" + path + "?" + v.Encode()
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	fresh := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fresh = true
	}

	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	// Exactly one writer. SQLite serialises writes anyway; forcing the pool to
	// one connection turns lock contention into a queue we control.
	w.SetMaxOpenConns(1)

	if err := w.Ping(); err != nil {
		w.Close()
		return nil, err
	}

	if fresh {
		// auto_vacuum can only be chosen before the first table exists;
		// changing it later needs a full VACUUM of the whole database.
		if _, err := w.Exec(`PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
			w.Close()
			return nil, fmt.Errorf("set auto_vacuum: %w", err)
		}
	}

	if _, err := w.Exec(schemaSQL); err != nil {
		w.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)

	return &DB{w: w, r: r}, nil
}

func (d *DB) Close() error {
	var errs []string
	if err := d.w.Close(); err != nil {
		errs = append(errs, err.Error())
	}
	if err := d.r.Close(); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("close: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Reader returns the read-only handle for queries.
func (d *DB) Reader() *sql.DB { return d.r }

// DefaultPath is where the daemon keeps its database.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "netwatch.db"
	}
	return filepath.Join(home, "Library", "Application Support", "netwatch", "netwatch.db")
}
