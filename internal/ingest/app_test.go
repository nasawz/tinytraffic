package ingest

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/egoist/tinytraffic/internal/store"
)

func TestAppEvents(t *testing.T) {
	st, err := store.Open("", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	site, _ := st.CreateSite("com.example.app", "UTC")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := store.NewWriter(st.Events, log)
	h := AppHandler{NewHandler(Handler{Store: st, Writer: w, Salts: NewSalts(st), Log: log})}

	send := func(body string) int {
		// An app's user agent would be dropped as a bot on the web endpoint.
		req := httptest.NewRequest(http.MethodPost, "/api/app/event", strings.NewReader(body))
		req.Header.Set("User-Agent", "okhttp/4.12.0")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	old := time.Now().Add(-2 * time.Hour).UnixMilli()
	ancient := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	ctx := `"app_id":"com.example.app","device_id":"dev-1","session_id":"s-1","os":"iOS","os_version":"26.1",
		"app_version":"1.6.0","build":"13","device_model":"iPhone17,1","device_type":"phone","locale":"zh-Hans-CN","channel":"appstore"`
	if c := send(fmt.Sprintf(`{%s,"events":[
		{"name":"app_open"},
		{"name":"screen_view","screen":"closet","ts":%d},
		{"name":"outfit_saved","screen":"/studio","props":{"items":4}},
		{"name":"stale","ts":%d}]}`, ctx, old, ancient)); c != http.StatusAccepted {
		t.Fatalf("status %d", c)
	}
	// Second launch: same install, new session.
	send(strings.Replace(fmt.Sprintf(`{%s,"events":[{"name":"app_open"}]}`, ctx), `"s-1"`, `"s-2"`, 1))
	// Same device ID in another app is another visitor; unknown apps, debug
	// builds and malformed payloads record nothing.
	send(`{"app_id":"com.other.app","device_id":"dev-1","events":[{"name":"app_open"}]}`)
	send(strings.Replace(fmt.Sprintf(`{%s,"events":[{"name":"app_open"}]}`, ctx), `"os"`, `"debug":true,"os"`, 1))
	if c := send(`{"app_id":"com.example.app","events":[{"name":"x"}]}`); c != http.StatusBadRequest {
		t.Errorf("missing device_id: status %d", c)
	}
	w.Close()

	rows, err := st.DB.Query(`SELECT name, pathname, hostname, device, os, app_version, app_build, device_model, locale, referrer_source, props, visitor_id, session_id, ts
		FROM events WHERE site_id = ? ORDER BY ts`, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		name, path, host, device, os, ver, build, model, locale, source, props string
		visitor, session                                                       uint64
		ts                                                                     time.Time
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.path, &r.host, &r.device, &r.os, &r.ver, &r.build, &r.model, &r.locale, &r.source, &r.props, &r.visitor, &r.session, &r.ts); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(got), got)
	}
	// The screen view carries the client's timestamp and is a pageview.
	var view, custom, second *row
	for i := range got {
		switch {
		case got[i].name == "pageview":
			view = &got[i]
		case got[i].name == "outfit_saved":
			custom = &got[i]
		case got[i].name == "app_open" && got[i].session != got[0].session:
			second = &got[i]
		}
	}
	if view == nil || view.path != "/closet" || view.host != "com.example.app" || view.device != "Mobile" || view.os != "iOS" ||
		view.ver != "1.6.0" || view.build != "13" || view.model != "iPhone17,1" || view.locale != "zh-Hans-CN" || view.source != "appstore" {
		t.Errorf("screen view: %+v", view)
	}
	if view != nil && time.Since(view.ts) < time.Hour {
		t.Errorf("client timestamp ignored: %v", view.ts)
	}
	if custom == nil || custom.path != "/studio" || custom.props != `{"items":"4"}` {
		t.Errorf("custom event: %+v", custom)
	}
	// Stale client timestamps fall back to now.
	for _, r := range got {
		if r.name == "stale" && time.Since(r.ts) > time.Minute {
			t.Errorf("stale event kept its old time: %v", r.ts)
		}
	}
	if second == nil {
		t.Fatalf("no second session: %+v", got)
	}
	for _, r := range got {
		if r.visitor != got[0].visitor {
			t.Errorf("one install should be one visitor across sessions")
		}
	}
	if second.session == view.session {
		t.Errorf("a new session id should start a new session")
	}
}
