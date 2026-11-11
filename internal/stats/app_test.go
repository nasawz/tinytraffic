package stats

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/egoist/tinytraffic/internal/store"
)

func TestAppSummaryAndRetention(t *testing.T) {
	st := openTestStore(t)
	site, err := st.CreateSite("com.example.app", "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) // 20:00 local
	day := func(daysAgo int, hour int) time.Time {
		return time.Date(2026, 11, 10-daysAgo, hour, 0, 0, 0, time.UTC)
	}
	var events []store.Event
	use := func(visitor uint64, daysAgo int, version string) {
		events = append(events, store.Event{
			Timestamp: day(daysAgo, 3), SiteID: site.ID, Name: "pageview", Pathname: "/home",
			VisitorID: visitor, SessionID: visitor*100 + uint64(daysAgo), AppVersion: version,
		})
	}
	// A: installed 5 days ago, back after 1 and 5 days.
	use(1, 5, "1.5.0")
	use(1, 4, "1.5.0")
	use(1, 0, "1.6.0")
	// B: installed 5 days ago, never came back.
	use(2, 5, "1.5.0")
	// C: installed today.
	use(3, 0, "1.6.0")
	// D: installed long ago, active today (returning, not new).
	use(4, 40, "1.4.0")
	use(4, 0, "1.6.0")
	if err := st.Events.Insert(context.Background(), events); err != nil {
		t.Fatal(err)
	}

	q, err := ParseQuery(site, url.Values{"period": {"7d"}}, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := App(context.Background(), st.Events, q, now)
	if err != nil {
		t.Fatal(err)
	}
	// DAU: A, C, D today. WAU/MAU: A, B, C, D (D's first visit was 40 days ago).
	want := AppSummary{IsApp: true, DAU: 3, WAU: 4, MAU: 4, NewUsers: 3, ReturningUsers: 1, TotalUsers: 4}
	if sum != want {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}

	cohorts, err := Retention(context.Background(), st.Events, q, now, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(cohorts) != 7 {
		t.Fatalf("%d cohorts", len(cohorts))
	}
	// Local dates: events at 03:00 UTC are 11:00 in Shanghai, same day.
	byDate := map[string]Cohort{}
	for _, c := range cohorts {
		byDate[c.Date] = c
	}
	c5 := byDate["2026-11-05"]
	if c5.Size != 2 {
		t.Fatalf("cohort 11-05 size %d, want 2 (A and B)", c5.Size)
	}
	get := func(c Cohort, n int) int64 {
		if c.Retained[n] == nil {
			return -1
		}
		return *c.Retained[n]
	}
	if get(c5, 1) != 1 || get(c5, 2) != 0 || get(c5, 5) != 1 || get(c5, 6) != -1 {
		t.Errorf("cohort 11-05 retention: D1=%d D2=%d D5=%d D6=%d", get(c5, 1), get(c5, 2), get(c5, 5), get(c5, 6))
	}
	c0 := byDate["2026-11-10"]
	if c0.Size != 1 || get(c0, 1) != -1 {
		t.Errorf("cohort 11-10: size %d, D1=%d", c0.Size, get(c0, 1))
	}
	// c0.Size == 1 above also shows that user D, first seen 40 days ago,
	// does not form a cohort today.

	// Filters narrow everything: only users seen on 1.6.0 events.
	q, _ = ParseQuery(site, url.Values{"period": {"7d"}, "f": {"app_version:1.6.0"}}, now, nil)
	sum, _ = App(context.Background(), st.Events, q, now)
	if sum.TotalUsers != 3 || sum.NewUsers != 3 {
		t.Errorf("filtered summary: %+v", sum)
	}
}
