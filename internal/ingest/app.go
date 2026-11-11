package ingest

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/cespare/xxhash/v2"

	"github.com/egoist/tinytraffic/internal/store"
)

// AppHandler records events sent by mobile and desktop apps, in batches.
//
// A site stands for an app: its domain is the app ID (e.g. a bundle ID).
// Unlike web visitors, who are hashed from IP and user agent, an app sends a
// random per-install device ID, so the same install keeps the same visitor ID
// across days. That is what makes retention and monthly active users
// possible. The device ID is hashed with the site ID before it is stored, and
// the IP is only used for the location, then discarded.
type AppHandler struct {
	*Handler
}

type appPayload struct {
	AppID       string `json:"app_id"`
	DeviceID    string `json:"device_id"`
	SessionID   string `json:"session_id"`
	OS          string `json:"os"`
	OSVersion   string `json:"os_version"`
	AppVersion  string `json:"app_version"`
	Build       string `json:"build"`
	DeviceModel string `json:"device_model"`
	// DeviceType is phone, tablet or desktop.
	DeviceType  string `json:"device_type"`
	Locale      string `json:"locale"`
	ScreenWidth int    `json:"screen_width"`
	// Channel is the distribution channel, e.g. appstore or huawei.
	Channel string `json:"channel"`
	// Debug marks development builds, which are not recorded.
	Debug  bool       `json:"debug"`
	Events []appEvent `json:"events"`
}

type appEvent struct {
	// Name is the event name. "screen_view" is recorded as a pageview of the
	// screen; everything else is a custom event.
	Name string `json:"name"`
	// Screen is the screen the event happened on.
	Screen string `json:"screen"`
	// Time is when the event happened, in epoch milliseconds. Apps queue
	// events while offline; the server's time is used when it is missing or
	// implausible.
	Time  int64           `json:"ts"`
	Props json.RawMessage `json:"props"`
}

const (
	maxAppEvents   = 100
	maxAppEventAge = 7 * 24 * time.Hour
)

func (h AppHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var p appPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	p.AppID = strings.ToLower(strings.TrimSpace(p.AppID))
	p.DeviceID = strings.TrimSpace(p.DeviceID)
	if p.AppID == "" || p.DeviceID == "" || len(p.Events) == 0 {
		http.Error(w, "missing app_id, device_id or events", http.StatusBadRequest)
		return
	}
	if len(p.Events) > maxAppEvents {
		http.Error(w, "too many events", http.StatusRequestEntityTooLarge)
		return
	}

	// As on the web endpoint, everything past validation answers 202, so the
	// endpoint never reveals which app IDs are registered.
	defer w.WriteHeader(http.StatusAccepted)
	if p.Debug {
		return
	}
	site, ok := h.Store.SiteByDomain(p.AppID)
	if !ok {
		return
	}

	now := time.Now().UTC()
	ip := ClientIP(r, h.TrustProxy || (h.TrustRequest != nil && h.TrustRequest(r)))
	loc := h.Geo.Lookup(ip)
	visitor := appVisitorHash(site.ID, p.DeviceID)

	channel := clip(p.Channel, 255)
	base := store.Event{
		SiteID:         site.ID,
		Hostname:       clip(p.AppID, 255),
		ReferrerSource: channel,
		UTMSource:      channel,
		Country:        loc.Country,
		Region:         loc.Region,
		City:           loc.City,
		OS:             clip(p.OS, 64),
		OSVersion:      clip(p.OSVersion, 64),
		Device:         appDeviceType(p.DeviceType),
		ScreenWidth:    uint16(min(max(p.ScreenWidth, 0), 65535)),
		HasLocation:    loc.HasCoords,
		Latitude:       float32(loc.Lat),
		Longitude:      float32(loc.Lon),
		AppVersion:     clip(p.AppVersion, 64),
		AppBuild:       clip(p.Build, 64),
		DeviceModel:    clip(p.DeviceModel, 128),
		Locale:         clip(p.Locale, 32),
	}

	for _, ev := range p.Events {
		name := strings.TrimSpace(ev.Name)
		if name == "" {
			continue
		}
		ts := now
		if ev.Time > 0 {
			if t := time.UnixMilli(ev.Time).UTC(); t.After(now.Add(-maxAppEventAge)) && t.Before(now.Add(5*time.Minute)) {
				ts = t
			}
		}
		e := base
		e.Timestamp = ts
		e.VisitorID = visitor
		if p.SessionID != "" {
			e.SessionID = appSessionHash(visitor, p.SessionID)
		} else {
			// No session from the app: 30 minutes without events end one.
			e.VisitorID, e.SessionID, _ = h.sessions.touch(visitor, 0, ts, attribution{})
		}
		screen := strings.Trim(strings.TrimSpace(ev.Screen), "/")
		if name == "screen_view" || name == "pageview" {
			e.Name = "pageview"
			e.Pathname = clip("/"+screen, 2000)
		} else {
			e.Name = clip(name, 120)
			if screen != "" {
				e.Pathname = clip("/"+screen, 2000)
			}
		}
		e.Props = encodeProps(ev.Props)
		h.Writer.Write(e)
	}
}

// appVisitorHash identifies an install. It has no daily salt, so it stays
// the same for as long as the app keeps its device ID.
func appVisitorHash(siteID int32, deviceID string) uint64 {
	d := xxhash.New()
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(siteID))
	d.Write(b[:])
	d.WriteString(deviceID)
	return d.Sum64()
}

func appSessionHash(visitor uint64, session string) uint64 {
	d := xxhash.New()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], visitor)
	d.Write(b[:])
	d.WriteString(session)
	return d.Sum64()
}

func appDeviceType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "phone", "mobile":
		return "Mobile"
	case "tablet", "foldable", "2in1":
		return "Tablet"
	case "desktop", "pc", "mac", "laptop":
		return "Desktop"
	}
	return ""
}
