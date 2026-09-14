package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// migrate brings an existing database up to the current schema.
//
// `CREATE TABLE IF NOT EXISTS` in schema.sql creates new databases correctly
// but does nothing for one that already exists, so columns added later need
// an explicit step. Each entry is idempotent.
func migrate(db *sql.DB) error {
	adds := []struct{ table, column, def string }{
		{"network", "isp_hop", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, a := range adds {
		has, err := hasColumn(db, a.table, a.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", a.table, a.column, a.def)
		if _, err := db.Exec(stmt); err != nil {
			// A concurrent process may have added it between the check and here.
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			typ       string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
