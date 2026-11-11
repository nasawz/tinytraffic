package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// MigrateProgress reports a site's copy: events copied so far and the month
// being copied.
type MigrateProgress struct {
	SiteID int32
	Copied int64
	Month  time.Time
	Done   bool
}

// migrateBatch is the most events per insert (a variable for tests).
var migrateBatch = 50_000

// MigrateToClickHouse copies a DuckDB store to a ClickHouse one: sites and
// their aliases, API keys, settings (such as the password), salts and events.
//
// It is safe to run again. Sites, keys, settings and salts already in
// ClickHouse are kept; it fails if a site or key ID means something else
// there. For each site it copies only events older than the site's earliest
// event in ClickHouse, so it can run after the server already started
// writing to ClickHouse, and nothing is copied twice. Events are copied
// newest first, a month at a time, and each insert stays within one month
// (one ClickHouse partition) so it lands whole or not at all: if the copy is
// interrupted, running it again resumes where it stopped.
//
// Nothing is removed from DuckDB.
func MigrateToClickHouse(ctx context.Context, from, to *Store, progress func(MigrateProgress)) (int64, error) {
	duck, ok := from.meta.(*duckMeta)
	ch, ok2 := to.meta.(*clickhouseMeta)
	if !ok || !ok2 {
		return 0, errors.New("migrate: need a DuckDB store and a ClickHouse store")
	}
	if err := copyMeta(from, duck, to, ch); err != nil {
		return 0, err
	}

	rows, err := from.DB.QueryContext(ctx, `SELECT site_id, min(ts), max(ts) FROM events GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return 0, err
	}
	type siteRange struct {
		id          int32
		first, last time.Time
	}
	var sites []siteRange
	for rows.Next() {
		var r siteRange
		if err := rows.Scan(&r.id, &r.first, &r.last); err != nil {
			rows.Close()
			return 0, err
		}
		sites = append(sites, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var total int64
	for _, site := range sites {
		// Copy everything before the first event already in ClickHouse.
		var cutoff sql.NullTime
		if err := to.Events.QueryRowContext(ctx,
			`SELECT minOrNull(ts) FROM events WHERE site_id = ?`, site.id).Scan(&cutoff); err != nil {
			return total, err
		}
		end := site.last.Add(time.Microsecond)
		if cutoff.Valid && cutoff.Time.Before(end) {
			end = cutoff.Time
		}
		var copied int64
		// Timestamps have microsecond precision, so the newest event left
		// is at most end-1µs; copy its month, then the month before.
		for site.first.Before(end) {
			month := monthStart(end.Add(-time.Microsecond))
			n, err := copyRange(ctx, from.Events, to.Events, site.id, month, end)
			copied += n
			total += n
			if err != nil {
				return total, fmt.Errorf("site %d, %s: %w", site.id, month.Format("2006-01"), err)
			}
			if progress != nil {
				progress(MigrateProgress{SiteID: site.id, Copied: copied, Month: month})
			}
			end = month
		}
		if progress != nil {
			progress(MigrateProgress{SiteID: site.id, Copied: copied, Done: true})
		}
	}
	return total, nil
}

// copyMeta copies sites, aliases, API keys, settings and salts that
// ClickHouse doesn't have yet.
func copyMeta(from *Store, duck *duckMeta, to *Store, ch *clickhouseMeta) error {
	settings, err := duck.settings()
	if err != nil {
		return err
	}
	for k, v := range settings {
		if have, err := ch.setting(k); err != nil {
			return err
		} else if have == "" {
			if err := ch.setSetting(k, v); err != nil {
				return err
			}
		}
	}

	sites := from.Sites()
	slices.SortFunc(sites, func(a, b Site) int { return int(a.ID - b.ID) })
	for _, site := range sites {
		if have, ok := to.SiteByID(site.ID); ok {
			if have.Domain != site.Domain {
				return fmt.Errorf("site %d is %s in DuckDB but %s in ClickHouse", site.ID, site.Domain, have.Domain)
			}
		} else if other, ok := to.SiteByDomain(site.Domain); ok {
			return fmt.Errorf("%s is site %d in DuckDB but site %d in ClickHouse", site.Domain, site.ID, other.ID)
		} else if err := ch.putSite(site, false); err != nil {
			return err
		}
		for _, a := range site.Aliases {
			if _, ok := to.SiteByDomain(a); !ok {
				if err := ch.putAlias(a, site.ID, false); err != nil {
					return err
				}
			}
		}
	}

	from.keysMu.RLock()
	keys := make([]APIKey, 0, len(from.keys))
	for _, k := range from.keys {
		keys = append(keys, *k)
	}
	from.keysMu.RUnlock()
	have := map[int32]string{}
	to.keysMu.RLock()
	for h, k := range to.keys {
		have[k.ID] = h
	}
	to.keysMu.RUnlock()
	for _, k := range keys {
		if h, ok := have[k.ID]; ok {
			if h != k.hash {
				return fmt.Errorf("API key %d (%s) is a different key in ClickHouse", k.ID, k.Name)
			}
			continue
		}
		if err := ch.putAPIKey(&k, false); err != nil {
			return err
		}
		if k.LastUsedAt != nil {
			if err := ch.touchAPIKey(k.ID, *k.LastUsedAt); err != nil {
				return err
			}
		}
	}

	// Today's salt keeps visitors counted once across the switch.
	salts, err := duck.salts()
	if err != nil {
		return err
	}
	for day, salt := range salts {
		if have, err := ch.salt(day); err != nil {
			return err
		} else if have == nil {
			if _, err := ch.addSalt(day, salt); err != nil {
				return err
			}
		}
	}
	return to.reload()
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// copyRange copies a site's events in [start, end), which lies within one
// month, newest first.
func copyRange(ctx context.Context, from, to *Events, siteID int32, start, end time.Time) (int64, error) {
	rows, err := from.QueryContext(ctx, `SELECT `+strings.Join(eventColumns, ", ")+`
FROM events WHERE site_id = ? AND ts >= ? AND ts < ? ORDER BY ts DESC`, siteID, start, end)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var copied int64
	batch := make([]Event, 0, migrateBatch)
	flush := func() error {
		if err := to.Insert(ctx, batch); err != nil {
			return err
		}
		copied += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var e Event
		var lat, lon sql.NullFloat64
		if err := rows.Scan(
			&e.Timestamp, &e.SiteID, &e.Name, &e.Hostname, &e.Pathname,
			&e.VisitorID, &e.SessionID, &e.Referrer, &e.ReferrerSource,
			&e.UTMSource, &e.UTMMedium, &e.UTMCampaign, &e.UTMContent, &e.UTMTerm,
			&e.Country, &e.Region, &e.City,
			&e.Browser, &e.BrowserVersion, &e.OS, &e.OSVersion, &e.Device,
			&e.ScreenWidth, &e.Props, &lat, &lon,
			&e.AppVersion, &e.AppBuild, &e.DeviceModel, &e.Locale,
		); err != nil {
			return copied, err
		}
		if lat.Valid && lon.Valid {
			e.HasLocation, e.Latitude, e.Longitude = true, float32(lat.Float64), float32(lon.Float64)
		}
		// Never split events with the same timestamp across inserts: a
		// resumed copy starts strictly before the oldest copied event.
		if len(batch) >= migrateBatch && !e.Timestamp.Equal(batch[len(batch)-1].Timestamp) {
			if err := flush(); err != nil {
				return copied, err
			}
		}
		batch = append(batch, e)
	}
	if err := rows.Err(); err != nil {
		return copied, err
	}
	if len(batch) > 0 {
		return copied, flush()
	}
	return copied, nil
}
