package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

func openDuckDB(path string, opts Options) (*sql.DB, error) {
	if opts.MemoryLimit == "" {
		opts.MemoryLimit = "1GB"
	}
	if !memoryLimitPattern.MatchString(opts.MemoryLimit) {
		return nil, fmt.Errorf("invalid memory limit %q (use e.g. 512MB or 2GB)", opts.MemoryLimit)
	}
	// Every connection works in UTC so time.Time parameters and TIMESTAMPTZ
	// columns compare without surprises; local time is applied in queries.
	// Insertion order is irrelevant for us and keeping it costs memory.
	init := []string{
		"SET TimeZone = 'UTC'",
		"SET memory_limit = '" + opts.MemoryLimit + "'",
		"SET preserve_insertion_order = false",
	}
	connector, err := duckdb.NewConnector(path, func(execer driver.ExecerContext) error {
		for _, stmt := range init {
			if _, err := execer.ExecContext(context.Background(), stmt, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}
	db := sql.OpenDB(connector)
	if err := migrateDuckDB(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Each migration is a list of statements, applied once and in order.
var migrations = [][]string{
	{
		`CREATE SEQUENCE sites_id_seq START 1`,
		`CREATE TABLE sites (
			id         INTEGER PRIMARY KEY DEFAULT nextval('sites_id_seq'),
			domain     VARCHAR NOT NULL UNIQUE,
			timezone   VARCHAR NOT NULL DEFAULT 'UTC',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		// Append-only, one row per event. Empty strings instead of NULLs keep
		// grouping and scanning simple.
		`CREATE TABLE events (
			ts              TIMESTAMPTZ NOT NULL,
			site_id         INTEGER NOT NULL,
			name            VARCHAR NOT NULL,
			hostname        VARCHAR NOT NULL,
			pathname        VARCHAR NOT NULL,
			visitor_id      UBIGINT NOT NULL,
			session_id      UBIGINT NOT NULL,
			referrer        VARCHAR NOT NULL,
			referrer_source VARCHAR NOT NULL,
			utm_source      VARCHAR NOT NULL,
			utm_medium      VARCHAR NOT NULL,
			utm_campaign    VARCHAR NOT NULL,
			utm_content     VARCHAR NOT NULL,
			utm_term        VARCHAR NOT NULL,
			country         VARCHAR NOT NULL,
			region          VARCHAR NOT NULL,
			city            VARCHAR NOT NULL,
			browser         VARCHAR NOT NULL,
			browser_version VARCHAR NOT NULL,
			os              VARCHAR NOT NULL,
			os_version      VARCHAR NOT NULL,
			device          VARCHAR NOT NULL,
			screen_width    USMALLINT NOT NULL,
			props           VARCHAR NOT NULL
		)`,
		`CREATE TABLE salts (
			day  DATE PRIMARY KEY,
			salt BLOB NOT NULL
		)`,
		`CREATE TABLE settings (
			key   VARCHAR PRIMARY KEY,
			value VARCHAR NOT NULL
		)`,
		// API keys for the public API. Only a SHA-256 hash of each key is
		// stored; prefix is the start of the key, shown to tell keys apart.
		// scope is "read" (stats only) or "write" (also manage sites and send
		// trusted events).
		`CREATE SEQUENCE api_keys_id_seq START 1`,
		`CREATE TABLE api_keys (
			id           INTEGER PRIMARY KEY DEFAULT nextval('api_keys_id_seq'),
			name         VARCHAR NOT NULL,
			prefix       VARCHAR NOT NULL,
			scope        VARCHAR NOT NULL,
			hash         VARCHAR NOT NULL UNIQUE,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			last_used_at TIMESTAMPTZ
		)`,
	},
	{
		// Previous domains of renamed sites.
		`CREATE TABLE site_aliases (
			domain  VARCHAR PRIMARY KEY,
			site_id INTEGER NOT NULL
		)`,
	},
	{
		// City coordinates from GeoIP (rounded to ~1 km), for the globe.
		// NULL when unknown, including for events recorded before this.
		// (Some development databases already have them.)
		`ALTER TABLE events ADD COLUMN IF NOT EXISTS latitude FLOAT`,
		`ALTER TABLE events ADD COLUMN IF NOT EXISTS longitude FLOAT`,
	},
	{
		// Public sites' dashboards can be viewed without signing in.
		`ALTER TABLE sites ADD COLUMN public BOOLEAN DEFAULT false`,
	}, {
		// App analytics: version, build, device model and locale of the app
		// that sent the event. Empty for web events.
		`ALTER TABLE events ADD COLUMN IF NOT EXISTS app_version VARCHAR DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN IF NOT EXISTS app_build VARCHAR DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN IF NOT EXISTS device_model VARCHAR DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN IF NOT EXISTS locale VARCHAR DEFAULT ''`,
	},
}

func migrateDuckDB(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var version int
	if err := db.QueryRow(`SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, stmt := range migrations[i] {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// duckMeta keeps sites, API keys, settings and salts in DuckDB.
type duckMeta struct{ db *sql.DB }

func (m *duckMeta) close() error { return m.db.Close() }

func (m *duckMeta) loadSites() (map[int32]Site, error) {
	rows, err := m.db.Query(`SELECT id, domain, timezone, coalesce(public, false), created_at FROM sites`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sites := map[int32]Site{}
	for rows.Next() {
		var site Site
		if err := rows.Scan(&site.ID, &site.Domain, &site.Timezone, &site.Public, &site.CreatedAt); err != nil {
			return nil, err
		}
		site.Aliases = []string{}
		sites[site.ID] = site
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	aliases, err := m.db.Query(`SELECT domain, site_id FROM site_aliases ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer aliases.Close()
	return sites, scanAliases(aliases, sites)
}

// scanAliases adds (domain, site_id) rows to their sites.
func scanAliases(rows *sql.Rows, sites map[int32]Site) error {
	for rows.Next() {
		var domain string
		var id int32
		if err := rows.Scan(&domain, &id); err != nil {
			return err
		}
		if site, ok := sites[id]; ok {
			site.Aliases = append(site.Aliases, domain)
			slices.Sort(site.Aliases)
			sites[id] = site
		}
	}
	return rows.Err()
}

func (m *duckMeta) createSite(domain, timezone string) (Site, error) {
	site := Site{Domain: domain, Timezone: timezone, Aliases: []string{}}
	err := m.db.QueryRow(
		`INSERT INTO sites (domain, timezone) VALUES (?, ?) RETURNING id, created_at`,
		domain, timezone,
	).Scan(&site.ID, &site.CreatedAt)
	return site, err
}

func (m *duckMeta) updateSite(old, next Site) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if next.Domain != old.Domain {
		if _, err := tx.Exec(`DELETE FROM site_aliases WHERE domain = ?`, next.Domain); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO site_aliases (domain, site_id) VALUES (?, ?)`, old.Domain, old.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE sites SET domain = ?, timezone = ?, public = ? WHERE id = ?`, next.Domain, next.Timezone, next.Public, old.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *duckMeta) removeAlias(id int32, domain string) error {
	_, err := m.db.Exec(`DELETE FROM site_aliases WHERE domain = ? AND site_id = ?`, domain, id)
	return err
}

func (m *duckMeta) deleteSite(site Site) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM events WHERE site_id = ?`,
		`DELETE FROM site_aliases WHERE site_id = ?`,
		`DELETE FROM sites WHERE id = ?`,
	} {
		if _, err := tx.Exec(stmt, site.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *duckMeta) loadAPIKeys() (map[string]*APIKey, error) {
	rows, err := m.db.Query(`SELECT id, name, prefix, scope, hash, created_at, last_used_at FROM api_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAPIKeys(rows)
}

// scanAPIKeys reads (id, name, prefix, scope, hash, created_at, last_used_at)
// rows.
func scanAPIKeys(rows *sql.Rows) (map[string]*APIKey, error) {
	keys := map[string]*APIKey{}
	for rows.Next() {
		k := &APIKey{}
		var last sql.NullTime
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Scope, &k.hash, &k.CreatedAt, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			k.LastUsedAt = &last.Time
		}
		keys[k.hash] = k
	}
	return keys, rows.Err()
}

func (m *duckMeta) createAPIKey(k *APIKey) error {
	return m.db.QueryRow(
		`INSERT INTO api_keys (name, prefix, scope, hash) VALUES (?, ?, ?, ?) RETURNING id, created_at`,
		k.Name, k.Prefix, k.Scope, k.hash,
	).Scan(&k.ID, &k.CreatedAt)
}

func (m *duckMeta) deleteAPIKey(id int32) error {
	_, err := m.db.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

func (m *duckMeta) touchAPIKey(id int32, t time.Time) error {
	_, err := m.db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, t, id)
	return err
}

func (m *duckMeta) setting(key string) (string, error) {
	var v string
	err := m.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (m *duckMeta) setSetting(key, value string) error {
	_, err := m.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, key, value)
	return err
}

// settings returns every setting, for copying them to ClickHouse.
func (m *duckMeta) settings() (map[string]string, error) {
	rows, err := m.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (m *duckMeta) salt(day string) ([]byte, error) {
	var salt []byte
	err := m.db.QueryRow(`SELECT salt FROM salts WHERE day = ?`, day).Scan(&salt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return salt, err
}

func (m *duckMeta) addSalt(day string, salt []byte) ([]byte, error) {
	if _, err := m.db.Exec(`INSERT OR IGNORE INTO salts (day, salt) VALUES (?, ?)`, day, salt); err != nil {
		return nil, err
	}
	return m.salt(day)
}

func (m *duckMeta) deleteSaltsBefore(day string) error {
	_, err := m.db.Exec(`DELETE FROM salts WHERE day < ?`, day)
	return err
}

// salts returns every stored salt by day, for copying them to ClickHouse.
func (m *duckMeta) salts() (map[string][]byte, error) {
	rows, err := m.db.Query(`SELECT strftime(day, '%Y-%m-%d'), salt FROM salts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var day string
		var salt []byte
		if err := rows.Scan(&day, &salt); err != nil {
			return nil, err
		}
		out[day] = salt
	}
	return out, rows.Err()
}
