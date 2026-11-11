// Package stats runs dashboard queries over the events table.
package stats

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/egoist/tinytraffic/internal/store"
)

// Query is a parsed dashboard request: a site, a time range in the site's
// timezone and a set of filters.
type Query struct {
	Site     store.Site
	Loc      *time.Location
	Period   string
	Start    time.Time // inclusive
	End      time.Time // exclusive
	Interval string    // hour | day | month
	Filters  []Filter
}

type Filter struct {
	Dim   string `json:"dim"`
	Value string `json:"value"`
}

// columns maps filter/breakdown dimensions to event columns.
var columns = map[string]string{
	"page":     "pathname",
	"hostname": "hostname",
	"source":   "referrer_source",
	// Referrers are stored as host + path; "referrer" groups them by host and
	// "referrer_url" lists the individual links.
	"referrer":     "split_part(referrer, '/', 1)",
	"referrer_url": "referrer",
	"utm_source":   "utm_source",
	"utm_medium":   "utm_medium",
	"utm_campaign": "utm_campaign",
	"utm_content":  "utm_content",
	"utm_term":     "utm_term",
	"country":      "country",
	"region":       "region",
	"city":         "city",
	"browser":      "browser",
	"os":           "os",
	"device":       "device",
	// App analytics.
	"app_version":  "app_version",
	"app_build":    "app_build",
	"device_model": "device_model",
	"locale":       "locale",
}

// column returns the SQL for a filter/breakdown dimension.
func column(b store.Backend, dim string) (string, bool) {
	if dim == "referrer" && b == store.ClickHouse {
		return "splitByChar('/', referrer)[1]", true
	}
	col, ok := columns[dim]
	return col, ok
}

// Session-level dimensions, resolved with subqueries.
var sessionDims = map[string]bool{"entry_page": true, "exit_page": true, "event": true}

var intervals = map[string]bool{"hour": true, "day": true, "month": true}

// ParseQuery reads period, from, to, interval and f (repeated "dim:value")
// parameters. firstEvent is used for the "all" period.
func ParseQuery(site store.Site, v url.Values, now time.Time, firstEvent func() (time.Time, bool)) (Query, error) {
	loc, err := time.LoadLocation(site.Timezone)
	if err != nil {
		loc = time.UTC
	}
	q := Query{Site: site, Loc: loc, Period: v.Get("period")}
	if q.Period == "" {
		q.Period = "30d"
	}
	n := now.In(loc)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	month := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)

	switch q.Period {
	case "today":
		q.Start, q.End, q.Interval = today, today.AddDate(0, 0, 1), "hour"
	case "yesterday":
		q.Start, q.End, q.Interval = today.AddDate(0, 0, -1), today, "hour"
	case "24h":
		hour := time.Date(n.Year(), n.Month(), n.Day(), n.Hour(), 0, 0, 0, loc)
		q.Start, q.End, q.Interval = hour.Add(-23*time.Hour), hour.Add(time.Hour), "hour"
	case "7d":
		q.Start, q.End, q.Interval = today.AddDate(0, 0, -6), today.AddDate(0, 0, 1), "day"
	case "30d":
		q.Start, q.End, q.Interval = today.AddDate(0, 0, -29), today.AddDate(0, 0, 1), "day"
	case "90d":
		q.Start, q.End, q.Interval = today.AddDate(0, 0, -89), today.AddDate(0, 0, 1), "day"
	case "month":
		q.Start, q.End, q.Interval = month, month.AddDate(0, 1, 0), "day"
	case "lastmonth":
		q.Start, q.End, q.Interval = month.AddDate(0, -1, 0), month, "day"
	case "12mo":
		q.Start, q.End, q.Interval = month.AddDate(0, -11, 0), month.AddDate(0, 1, 0), "month"
	case "all":
		start := today
		if t, ok := firstEvent(); ok {
			t = t.In(loc)
			start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		}
		q.Start, q.End = start, today.AddDate(0, 0, 1)
		q.Interval = autoInterval(q.Start, q.End)
		if q.Interval == "month" {
			q.Start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, loc)
		}
	case "custom":
		from, err1 := time.ParseInLocation(time.DateOnly, v.Get("from"), loc)
		to, err2 := time.ParseInLocation(time.DateOnly, v.Get("to"), loc)
		if err1 != nil || err2 != nil || to.Before(from) {
			return q, errors.New("custom period needs from and to as YYYY-MM-DD")
		}
		q.Start, q.End = from, to.AddDate(0, 0, 1)
		q.Interval = autoInterval(q.Start, q.End)
		if q.Interval == "month" {
			q.Start = time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, loc)
		}
	default:
		return q, fmt.Errorf("unknown period %q", q.Period)
	}
	if iv := v.Get("interval"); intervals[iv] {
		q.Interval = iv
	}

	for _, f := range v["f"] {
		dim, value, ok := strings.Cut(f, ":")
		if !ok {
			continue
		}
		if _, known := columns[dim]; !known && !sessionDims[dim] {
			return q, fmt.Errorf("unknown filter %q", dim)
		}
		q.Filters = append(q.Filters, Filter{Dim: dim, Value: value})
	}
	return q, nil
}

func autoInterval(start, end time.Time) string {
	switch d := end.Sub(start); {
	case d <= 48*time.Hour:
		return "hour"
	case d <= 93*24*time.Hour:
		return "day"
	default:
		return "month"
	}
}

// clampedEnd is the end of the range, capped at now, so comparisons use
// equally long periods ("today so far" vs. the same hours yesterday).
func (q Query) clampedEnd(now time.Time) time.Time {
	if now.Before(q.End) && now.After(q.Start) {
		return now
	}
	return q.End
}

// where builds the WHERE clause for the query's range and filters.
func (q Query) where(b store.Backend, start, end time.Time) (string, []any) {
	conds := []string{"site_id = ?", "ts >= ?", "ts < ?"}
	args := []any{q.Site.ID, start.UTC(), end.UTC()}
	for _, f := range q.Filters {
		match, arg := matcher(b, f.Value)
		switch f.Dim {
		case "entry_page", "exit_page":
			// Both databases know argMin/argMax.
			fn := "argMin"
			if f.Dim == "exit_page" {
				fn = "argMax"
			}
			conds = append(conds, fmt.Sprintf(
				`session_id IN (SELECT session_id FROM events WHERE site_id = ? AND ts >= ? AND ts < ? AND name = 'pageview' GROUP BY session_id HAVING %s(pathname, ts) %s)`,
				fn, match))
			args = append(args, q.Site.ID, start.UTC(), end.UTC(), arg)
		case "event":
			conds = append(conds, fmt.Sprintf(
				`session_id IN (SELECT session_id FROM events WHERE site_id = ? AND ts >= ? AND ts < ? AND name %s)`, match))
			args = append(args, q.Site.ID, start.UTC(), end.UTC(), arg)
		default:
			col, _ := column(b, f.Dim)
			conds = append(conds, col+" "+match)
			args = append(args, arg)
		}
	}
	return strings.Join(conds, " AND "), args
}

// matcher supports exact values and * wildcards (e.g. /blog/*).
func matcher(b store.Backend, value string) (string, any) {
	if !strings.Contains(value, "*") {
		return "= ?", value
	}
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `*`, `%`)
	if b == store.ClickHouse {
		// ClickHouse has no ESCAPE clause; backslash is its escape character.
		return `LIKE ?`, r.Replace(value)
	}
	return `LIKE ? ESCAPE '\'`, r.Replace(value)
}
