package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

// Event is one row of the events table. Field order matches eventColumns.
type Event struct {
	Timestamp      time.Time
	SiteID         int32
	Name           string
	Hostname       string
	Pathname       string
	VisitorID      uint64
	SessionID      uint64
	Referrer       string
	ReferrerSource string
	UTMSource      string
	UTMMedium      string
	UTMCampaign    string
	UTMContent     string
	UTMTerm        string
	Country        string
	Region         string
	City           string
	Browser        string
	BrowserVersion string
	OS             string
	OSVersion      string
	Device         string
	ScreenWidth    uint16
	Props          string
	// City coordinates from GeoIP, stored as NULL unless HasLocation.
	HasLocation bool
	Latitude    float32
	Longitude   float32
	// App analytics: empty for web events.
	AppVersion  string
	AppBuild    string
	DeviceModel string
	Locale      string
}

const (
	bufferSize    = 100_000
	maxBatch      = 5_000
	flushInterval = time.Second
)

// Writer buffers events in memory and bulk-inserts them, so a traffic spike
// costs one insert per second instead of one per request.
type Writer struct {
	ev  *Events
	log *slog.Logger
	ch  chan Event

	mu      sync.RWMutex
	closed  bool
	done    chan struct{}
	dropped atomic.Uint64
}

func NewWriter(ev *Events, log *slog.Logger) *Writer {
	w := &Writer{ev: ev, log: log, ch: make(chan Event, bufferSize), done: make(chan struct{})}
	go w.run()
	return w
}

// Write queues an event. It never blocks; when the buffer is full the event
// is dropped and counted.
func (w *Writer) Write(e Event) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return false
	}
	select {
	case w.ch <- e:
		return true
	default:
		if n := w.dropped.Add(1); n%1000 == 1 {
			w.log.Warn("event buffer full, dropping events", "dropped_total", n)
		}
		return false
	}
}

// Close flushes everything still buffered.
func (w *Writer) Close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	w.mu.Unlock()
	<-w.done
}

func (w *Writer) run() {
	defer close(w.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	batch := make([]Event, 0, maxBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.ev.Insert(context.Background(), batch); err != nil {
			w.log.Error("insert events", "count", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case e, ok := <-w.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// eventColumns are named for the appender, since columns added by later
// migrations sit at the end of older tables but inline in newer ones.
var eventColumns = []string{
	"ts", "site_id", "name", "hostname", "pathname",
	"visitor_id", "session_id", "referrer", "referrer_source",
	"utm_source", "utm_medium", "utm_campaign", "utm_content", "utm_term",
	"country", "region", "city",
	"browser", "browser_version", "os", "os_version", "device",
	"screen_width", "props", "latitude", "longitude",
	"app_version", "app_build", "device_model", "locale",
}

// insertDuckDB appends events with the DuckDB appender.
func insertDuckDB(ctx context.Context, db *sql.DB, events []Event) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		a, err := duckdb.NewAppenderWithColumns(dc.(driver.Conn), "", "", "events", eventColumns)
		if err != nil {
			return err
		}
		for _, e := range events {
			var lat, lon driver.Value
			if e.HasLocation {
				lat, lon = e.Latitude, e.Longitude
			}
			if err := a.AppendRow(
				e.Timestamp, e.SiteID, e.Name, e.Hostname, e.Pathname,
				e.VisitorID, e.SessionID, e.Referrer, e.ReferrerSource,
				e.UTMSource, e.UTMMedium, e.UTMCampaign, e.UTMContent, e.UTMTerm,
				e.Country, e.Region, e.City,
				e.Browser, e.BrowserVersion, e.OS, e.OSVersion, e.Device,
				e.ScreenWidth, e.Props, lat, lon,
				e.AppVersion, e.AppBuild, e.DeviceModel, e.Locale,
			); err != nil {
				a.Close()
				return err
			}
		}
		return a.Close()
	})
}
