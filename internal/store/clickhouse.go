package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
)

// ClickHouse has no transactions, unique keys or in-place updates, so
// everything but events and salts is stored as versioned rows: every change
// inserts the whole row with a higher version, deletes insert a row marked
// deleted, and reads take each key's latest version (argMax). The
// ReplacingMergeTree engine drops old versions in the background.
//
// IDs are the highest ID so far plus one. Two processes creating a site in
// the same instant could pick the same ID, which a single-user dashboard
// doesn't do.
var clickHouseSchema = []string{
	// The events table has the same columns as DuckDB's. Rows are sorted by
	// site and day, so a dashboard query reads only its site's range;
	// visitor_id last keeps a visitor's events together, which compresses
	// well and speeds up uniqExact.
	`CREATE TABLE IF NOT EXISTS events (
		ts              DateTime64(6, 'UTC') CODEC(Delta, ZSTD),
		site_id         Int32,
		name            LowCardinality(String),
		hostname        LowCardinality(String),
		pathname        String,
		visitor_id      UInt64,
		session_id      UInt64,
		referrer        String,
		referrer_source LowCardinality(String),
		utm_source      LowCardinality(String),
		utm_medium      LowCardinality(String),
		utm_campaign    LowCardinality(String),
		utm_content     String,
		utm_term        String,
		country         LowCardinality(String),
		region          LowCardinality(String),
		city            LowCardinality(String),
		browser         LowCardinality(String),
		browser_version LowCardinality(String),
		os              LowCardinality(String),
		os_version      LowCardinality(String),
		device          LowCardinality(String),
		screen_width    UInt16,
		props           String,
		latitude        Nullable(Float32),
		longitude       Nullable(Float32),
		app_version     LowCardinality(String),
		app_build       LowCardinality(String),
		device_model    LowCardinality(String),
		locale          LowCardinality(String)
	)
	ENGINE = MergeTree
	PARTITION BY toYYYYMM(ts)
	ORDER BY (site_id, toDate(ts), visitor_id)`,
	`CREATE TABLE IF NOT EXISTS sites (
		id         Int32,
		domain     String,
		timezone   String,
		created_at DateTime64(6, 'UTC'),
		deleted    UInt8,
		version    UInt64
	) ENGINE = ReplacingMergeTree(version) ORDER BY id`,
	`CREATE TABLE IF NOT EXISTS site_aliases (
		domain  String,
		site_id Int32,
		deleted UInt8,
		version UInt64
	) ENGINE = ReplacingMergeTree(version) ORDER BY domain`,
	`CREATE TABLE IF NOT EXISTS api_keys (
		id         Int32,
		name       String,
		prefix     String,
		scope      String,
		hash       String,
		created_at DateTime64(6, 'UTC'),
		deleted    UInt8,
		version    UInt64
	) ENGINE = ReplacingMergeTree(version) ORDER BY id`,
	// Kept apart from api_keys so recording a use can never bring back a key
	// that was just deleted.
	`CREATE TABLE IF NOT EXISTS api_key_usage (
		id           Int32,
		last_used_at DateTime64(6, 'UTC')
	) ENGINE = ReplacingMergeTree(last_used_at) ORDER BY id`,
	`CREATE TABLE IF NOT EXISTS settings (
		key     String,
		value   String,
		version UInt64
	) ENGINE = ReplacingMergeTree(version) ORDER BY key`,
	// Salts are hex. A day's first salt wins (see addSalt). Deleted days are
	// also dropped by the TTL, in case a deletion was missed.
	`CREATE TABLE IF NOT EXISTS salts (
		day     Date,
		salt    String,
		version UInt64
	) ENGINE = MergeTree ORDER BY day TTL day + INTERVAL 3 DAY`,
	// Columns added later go here as ADD COLUMN IF NOT EXISTS, so this list
	// can run on every start.
	`ALTER TABLE events ADD COLUMN IF NOT EXISTS app_version LowCardinality(String)`,
	`ALTER TABLE events ADD COLUMN IF NOT EXISTS app_build LowCardinality(String)`,
	`ALTER TABLE events ADD COLUMN IF NOT EXISTS device_model LowCardinality(String)`,
	`ALTER TABLE events ADD COLUMN IF NOT EXISTS locale LowCardinality(String)`,
	`ALTER TABLE sites ADD COLUMN IF NOT EXISTS public UInt8 DEFAULT 0 AFTER timezone`,
}

// openClickHouse connects to the database in dsn, e.g.
// clickhouse://user:password@host:9000/tinytraffic or http://host:8123/db,
// and creates the tables that are missing.
func openClickHouse(dsn string) (*sql.DB, error) {
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to clickhouse: %w", err)
	}
	for _, stmt := range clickHouseSchema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("clickhouse schema: %w", err)
		}
	}
	return db, nil
}

// chTimeLayout is how times are sent to ClickHouse. The driver would bind a
// time.Time with whole seconds only; a string compares against a
// DateTime64(6, 'UTC') column at full precision.
const chTimeLayout = "2006-01-02 15:04:05.000000"

func clickHouseArgs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		if t, ok := a.(time.Time); ok {
			a = t.UTC().Format(chTimeLayout)
		}
		out[i] = a
	}
	return out
}

var versionMu sync.Mutex
var lastVersion uint64

// nextVersion returns an increasing row version: the time in nanoseconds,
// so versions written by different processes order by time too.
func nextVersion() uint64 {
	versionMu.Lock()
	defer versionMu.Unlock()
	v := uint64(time.Now().UnixNano())
	if v <= lastVersion {
		v = lastVersion + 1
	}
	lastVersion = v
	return v
}

// clickhouseMeta keeps sites, API keys, settings and salts in ClickHouse.
type clickhouseMeta struct{ db *sql.DB }

func (m *clickhouseMeta) close() error { return m.db.Close() }

func (m *clickhouseMeta) exec(query string, args ...any) error {
	_, err := m.db.Exec(query, clickHouseArgs(args)...)
	return err
}

func (m *clickhouseMeta) loadSites() (map[int32]Site, error) {
	rows, err := m.db.Query(`
SELECT id, argMax(domain, version), argMax(timezone, version), argMax(public, version) = 1, argMax(created_at, version)
FROM sites GROUP BY id HAVING argMax(deleted, version) = 0`)
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
	aliases, err := m.db.Query(`
SELECT domain, argMax(site_id, version) FROM site_aliases
GROUP BY domain HAVING argMax(deleted, version) = 0 ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer aliases.Close()
	return sites, scanAliases(aliases, sites)
}

func (m *clickhouseMeta) putSite(site Site, deleted bool) error {
	return m.exec(`INSERT INTO sites (id, domain, timezone, public, created_at, deleted, version) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		site.ID, site.Domain, site.Timezone, site.Public, site.CreatedAt, deleted, nextVersion())
}

func (m *clickhouseMeta) putAlias(domain string, siteID int32, deleted bool) error {
	return m.exec(`INSERT INTO site_aliases (domain, site_id, deleted, version) VALUES (?, ?, ?, ?)`,
		domain, siteID, deleted, nextVersion())
}

func (m *clickhouseMeta) createSite(domain, timezone string) (Site, error) {
	var maxID int32
	// Deleted sites count too, so their IDs aren't reused.
	if err := m.db.QueryRow(`SELECT max(id) FROM sites`).Scan(&maxID); err != nil {
		return Site{}, err
	}
	site := Site{ID: maxID + 1, Domain: domain, Timezone: timezone, Aliases: []string{},
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	return site, m.putSite(site, false)
}

func (m *clickhouseMeta) updateSite(old, next Site) error {
	if next.Domain != old.Domain {
		if err := m.putAlias(next.Domain, old.ID, true); err != nil {
			return err
		}
		if err := m.putAlias(old.Domain, old.ID, false); err != nil {
			return err
		}
	}
	return m.putSite(next, false)
}

func (m *clickhouseMeta) removeAlias(id int32, domain string) error {
	return m.putAlias(domain, id, true)
}

func (m *clickhouseMeta) deleteSite(site Site) error {
	// Events first: if that fails, the site is still there to retry.
	if err := m.exec(`DELETE FROM events WHERE site_id = ?`, site.ID); err != nil {
		return err
	}
	for _, a := range site.Aliases {
		if err := m.putAlias(a, site.ID, true); err != nil {
			return err
		}
	}
	return m.putSite(site, true)
}

func (m *clickhouseMeta) loadAPIKeys() (map[string]*APIKey, error) {
	rows, err := m.db.Query(`
SELECT k.id, k.key_name, k.key_prefix, k.key_scope, k.key_hash, k.key_created_at, u.last_used
FROM (
	SELECT id, argMax(name, version) AS key_name, argMax(prefix, version) AS key_prefix,
	       argMax(scope, version) AS key_scope, argMax(hash, version) AS key_hash,
	       argMax(created_at, version) AS key_created_at
	FROM api_keys GROUP BY id HAVING argMax(deleted, version) = 0
) AS k
LEFT JOIN (SELECT id, max(last_used_at) AS last_used FROM api_key_usage GROUP BY id) AS u ON k.id = u.id
SETTINGS join_use_nulls = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAPIKeys(rows)
}

func (m *clickhouseMeta) putAPIKey(k *APIKey, deleted bool) error {
	return m.exec(`INSERT INTO api_keys (id, name, prefix, scope, hash, created_at, deleted, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.Prefix, k.Scope, k.hash, k.CreatedAt, deleted, nextVersion())
}

func (m *clickhouseMeta) createAPIKey(k *APIKey) error {
	var maxID int32
	if err := m.db.QueryRow(`SELECT max(id) FROM api_keys`).Scan(&maxID); err != nil {
		return err
	}
	k.ID, k.CreatedAt = maxID+1, time.Now().UTC().Truncate(time.Microsecond)
	return m.putAPIKey(k, false)
}

func (m *clickhouseMeta) deleteAPIKey(id int32) error {
	return m.putAPIKey(&APIKey{ID: id}, true)
}

func (m *clickhouseMeta) touchAPIKey(id int32, t time.Time) error {
	return m.exec(`INSERT INTO api_key_usage (id, last_used_at) VALUES (?, ?)`, id, t)
}

func (m *clickhouseMeta) setting(key string) (string, error) {
	var v string
	err := m.db.QueryRow(`SELECT argMax(value, version) FROM settings WHERE key = ? GROUP BY key`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (m *clickhouseMeta) setSetting(key, value string) error {
	return m.exec(`INSERT INTO settings (key, value, version) VALUES (?, ?, ?)`, key, value, nextVersion())
}

func (m *clickhouseMeta) salt(day string) ([]byte, error) {
	var h string
	err := m.db.QueryRow(`SELECT argMin(salt, (version, salt)) FROM salts WHERE day = ? GROUP BY day`, day).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(h)
}

// addSalt inserts a salt and returns the day's earliest one. Processes that
// add a salt for the same day at once all end up with the same one.
func (m *clickhouseMeta) addSalt(day string, salt []byte) ([]byte, error) {
	if err := m.exec(`INSERT INTO salts (day, salt, version) VALUES (?, ?, ?)`, day, hex.EncodeToString(salt), nextVersion()); err != nil {
		return nil, err
	}
	return m.salt(day)
}

func (m *clickhouseMeta) deleteSaltsBefore(day string) error {
	return m.exec(`DELETE FROM salts WHERE day < ?`, day)
}

func insertClickHouse(ctx context.Context, db *sql.DB, events []Event) error {
	// The driver sends the rows of a prepared INSERT in one block when the
	// transaction commits.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO events ("+strings.Join(eventColumns, ", ")+")")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		var lat, lon *float32
		if e.HasLocation {
			lat, lon = &e.Latitude, &e.Longitude
		}
		if _, err := stmt.ExecContext(ctx,
			e.Timestamp.UTC(), e.SiteID, e.Name, e.Hostname, e.Pathname,
			e.VisitorID, e.SessionID, e.Referrer, e.ReferrerSource,
			e.UTMSource, e.UTMMedium, e.UTMCampaign, e.UTMContent, e.UTMTerm,
			e.Country, e.Region, e.City,
			e.Browser, e.BrowserVersion, e.OS, e.OSVersion, e.Device,
			e.ScreenWidth, e.Props, lat, lon,
			e.AppVersion, e.AppBuild, e.DeviceModel, e.Locale,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}
