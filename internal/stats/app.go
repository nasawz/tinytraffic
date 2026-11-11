package stats

import (
	"context"
	"fmt"
	"time"

	"github.com/egoist/tinytraffic/internal/store"
)

// AppSummary answers the questions an app owner asks first: how many people
// use it today, this week and this month, and how many of the people in the
// range are new.
type AppSummary struct {
	// IsApp is true when the site has received events from an app.
	IsApp bool `json:"is_app"`
	// Active users in the 1, 7 and 30 days up to the end of the range
	// (or now, if the range is still running).
	DAU int64 `json:"dau"`
	WAU int64 `json:"wau"`
	MAU int64 `json:"mau"`
	// NewUsers were first seen inside the range; Returning users were seen
	// in it and before it.
	NewUsers       int64 `json:"new_users"`
	ReturningUsers int64 `json:"returning_users"`
	// TotalUsers is every install ever seen, up to the end of the range.
	TotalUsers int64 `json:"total_users"`
}

// epoch is the lower bound for "all time" scans.
var epoch = time.Unix(0, 0)

// App computes the AppSummary. Filters apply to every number, so filtering
// by version shows who is on that version.
func App(ctx context.Context, ev *store.Events, q Query, now time.Time) (AppSummary, error) {
	var res AppSummary
	end := q.clampedEnd(now)
	count := func(dst *int64, start time.Time) error {
		where, args := q.where(ev.Backend, start, end)
		return ev.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT count(DISTINCT visitor_id) FROM events WHERE %s`, where), args...).Scan(dst)
	}
	var apps int64
	if err := ev.QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT 1 FROM events WHERE site_id = ? AND app_version <> '' LIMIT 1)`,
		q.Site.ID).Scan(&apps); err != nil {
		return res, err
	}
	res.IsApp = apps > 0

	// Day boundaries are rolling windows ending at the range's end, so "DAU"
	// is the last 24 hours rather than a calendar day that may be half over.
	for dst, start := range map[*int64]time.Time{
		&res.DAU:        end.Add(-24 * time.Hour),
		&res.WAU:        end.Add(-7 * 24 * time.Hour),
		&res.MAU:        end.Add(-30 * 24 * time.Hour),
		&res.TotalUsers: epoch,
	} {
		if err := count(dst, start); err != nil {
			return res, err
		}
	}

	// A visitor is new when the first event in the (filtered) history lies in
	// the range.
	where, args := q.where(ev.Backend, epoch, q.End)
	err := ev.QueryRowContext(ctx, fmt.Sprintf(`
SELECT count(*) FROM (
	SELECT visitor_id FROM events WHERE %s
	GROUP BY visitor_id HAVING min(ts) >= ?
)`, where), append(args, q.Start.UTC())...).Scan(&res.NewUsers)
	if err != nil {
		return res, err
	}
	var inRange int64
	if err := count(&inRange, q.Start); err != nil {
		return res, err
	}
	res.ReturningUsers = max(inRange-res.NewUsers, 0)
	return res, nil
}

// Cohort is the users first seen on Date, and how many of them came back on
// each following day. Retained[n] is the number active n days after their
// first day (Retained[0] is the cohort size); it is nil for days that have
// not happened yet.
type Cohort struct {
	Date     string   `json:"date"`
	Size     int64    `json:"size"`
	Retained []*int64 `json:"retained"`
}

// maxRetentionDays is how far each cohort is followed (D0..D30).
const maxRetentionDays = 30

// Retention builds day cohorts for the last `days` days up to the end of the
// range, in the site's time zone. Filters apply to all events, so a cohort
// filtered by version only has users whose first event was on that version.
func Retention(ctx context.Context, ev *store.Events, q Query, now time.Time, days int) ([]Cohort, error) {
	days = min(max(days, 1), 90)
	end := q.clampedEnd(now)
	endLocal := end.In(q.Loc)
	today := time.Date(endLocal.Year(), endLocal.Month(), endLocal.Day(), 0, 0, 0, 0, q.Loc)
	first := today.AddDate(0, 0, -(days - 1))

	// Offsets are baked into the SQL for the span from the first cohort day
	// to the end of the range.
	b := newBucketer(q.Loc, first, today.AddDate(0, 0, 1), "day")
	dayKey := b.keySQL(ev.Backend, epochUs(ev.Backend, "ts"))
	firstKey, todayKey := b.key(first), b.key(today)

	where, args := q.where(ev.Backend, epoch, today.AddDate(0, 0, 1))
	rows, err := ev.QueryContext(ctx, fmt.Sprintf(`
WITH d AS (
	SELECT visitor_id, %[1]s AS day FROM events WHERE %[2]s GROUP BY visitor_id, day
),
f AS (
	SELECT visitor_id, min(day) AS first FROM d GROUP BY visitor_id
)
SELECT f.first, d.day - f.first AS n, count(DISTINCT d.visitor_id)
FROM d JOIN f ON d.visitor_id = f.visitor_id
WHERE f.first >= ? AND d.day - f.first <= %[3]d
GROUP BY f.first, n ORDER BY f.first, n`, dayKey, where, maxRetentionDays),
		append(args, firstKey)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[int64]map[int64]int64{}
	for rows.Next() {
		var day, n, c int64
		if err := rows.Scan(&day, &n, &c); err != nil {
			return nil, err
		}
		if counts[day] == nil {
			counts[day] = map[int64]int64{}
		}
		counts[day][n] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	cohorts := make([]Cohort, 0, days)
	t := first
	for key := firstKey; key <= todayKey; key++ {
		c := Cohort{Date: t.Format(time.DateOnly), Size: counts[key][0], Retained: make([]*int64, maxRetentionDays+1)}
		for n := int64(0); n <= maxRetentionDays && key+n <= todayKey; n++ {
			v := counts[key][n]
			c.Retained[n] = &v
		}
		cohorts = append(cohorts, c)
		t = t.AddDate(0, 0, 1)
	}
	return cohorts, nil
}
