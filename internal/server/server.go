// Package server wires HTTP routes: event ingest, the tracker script, the
// dashboard API and the embedded dashboard.
package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/tinytraffic/internal/ingest"
	"github.com/egoist/tinytraffic/internal/stats"
	"github.com/egoist/tinytraffic/internal/store"
)

type Server struct {
	Store   *store.Store
	Auth    *Auth
	Ingest  *ingest.Handler
	Log     *slog.Logger
	Geo     *ingest.Geo // nil when no GeoIP database is configured
	Dist    fs.FS       // built dashboard
	Tracker []byte      // tracker script
	// TrackerPath, when set, serves the tracker from disk on every request
	// instead of Tracker. Used by `bun dev`, where the script is rebuilt live.
	TrackerPath string
	TrustProxy  bool

	limits  rateLimiter[int32]      // per API key
	viewers rateLimiter[netip.Addr] // signed-out viewers of public dashboards
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/api/event", s.Ingest)
	mux.Handle("/api/app/event", ingest.AppHandler{Handler: s.Ingest})
	mux.HandleFunc("GET /js/script.js", s.serveTracker)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /api/auth", s.authStatus)
	mux.HandleFunc("POST /api/auth/setup", s.setup)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("POST /api/auth/password", s.private(s.changePassword))

	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("GET /api/sites", s.private(s.listSites))
	mux.HandleFunc("POST /api/sites", s.private(s.createSite))
	// {site} is the site's ID or one of its domains.
	mux.HandleFunc("GET /api/sites/{site}", s.viewable(s.getSite))
	mux.HandleFunc("PATCH /api/sites/{site}", s.private(s.updateSite))
	mux.HandleFunc("DELETE /api/sites/{site}", s.private(s.deleteSite))
	mux.HandleFunc("DELETE /api/sites/{site}/aliases/{alias}", s.private(s.removeAlias))
	// The stats on a public site's dashboard are open to anyone.
	mux.HandleFunc("GET /api/sites/{site}/aggregate", s.viewable(s.aggregate))
	mux.HandleFunc("GET /api/sites/{site}/timeseries", s.viewable(s.timeseries))
	mux.HandleFunc("GET /api/sites/{site}/breakdown/{dim}", s.viewable(s.breakdown))
	mux.HandleFunc("GET /api/sites/{site}/app", s.viewable(s.appSummary))
	mux.HandleFunc("GET /api/sites/{site}/retention", s.viewable(s.retention))
	mux.HandleFunc("GET /api/sites/{site}/realtime", s.viewable(s.realtime))
	mux.HandleFunc("GET /api/sites/{site}/locations", s.private(s.locations))
	mux.HandleFunc("GET /api/sites/{site}/live", s.private(s.live))
	s.publicRoutes(mux)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})

	mux.HandleFunc("/", s.serveApp)
	return s.logRequests(mux)
}

func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(rw, r)
		if rw.status >= 500 {
			s.Log.Error("request", "method", r.Method, "path", r.URL.Path, "status", rw.status, "duration", time.Since(start))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// private requires a valid session. Mutating requests must be JSON, which a
// cross-site form cannot send without a CORS preflight.
func (s *Server) private(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Auth.Valid(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodDelete && !isJSON(r) {
			writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
			return
		}
		h(w, r)
	}
}

// viewable lets anyone read a public site's stats, and requires a session
// for the rest. A signed-out request for a site that isn't public is
// unauthorized whether the site exists or not, so it reveals nothing.
// Signed-out viewers are rate limited per IP.
func (s *Server) viewable(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Auth.Valid(r) {
			site, ok := s.Store.LookupSite(r.PathValue("site"))
			if !ok || !site.Public {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			if !limited(w, &s.viewers, viewerKey(ingest.ClientIP(r, s.TrustProxy)), viewerRateLimit) {
				return
			}
		}
		h(w, r)
	}
}

// viewerKey is the rate limit key for an IP. An IPv6 user usually has a
// whole /64, so it counts as one.
func viewerKey(ip netip.Addr) netip.Addr {
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.Addr()
	}
	return ip
}

func isJSON(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "application/json"
}

// --- auth ---

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"needs_setup":   !s.Auth.HasPassword(),
		"authenticated": s.Auth.Valid(r),
	})
}

type passwordBody struct {
	Password    string `json:"password"`
	NewPassword string `json:"new_password"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var body passwordBody
	if !readJSON(w, r, &body) {
		return
	}
	if s.Auth.HasPassword() {
		writeError(w, http.StatusConflict, "already set up")
		return
	}
	if err := s.Auth.SetPassword(body.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Auth.IssueCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body passwordBody
	if !readJSON(w, r, &body) {
		return
	}
	if err := s.Auth.CheckPassword(ingest.ClientIP(r, s.TrustProxy).String(), body.Password); err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, errRateLimited) {
			status = http.StatusTooManyRequests
		}
		writeError(w, status, err.Error())
		return
	}
	s.Auth.IssueCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Auth.ClearCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body passwordBody
	if !readJSON(w, r, &body) {
		return
	}
	if err := s.Auth.CheckPassword(ingest.ClientIP(r, s.TrustProxy).String(), body.Password); err != nil {
		writeError(w, http.StatusUnauthorized, "current password is wrong")
		return
	}
	if err := s.Auth.SetPassword(body.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Auth.IssueCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// info describes the server setup the dashboard adapts to. Public
// dashboards need it too, and it holds nothing private.
func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	attr := s.Geo.Attribution()
	writeJSON(w, http.StatusOK, map[string]any{
		"geoip": map[string]any{
			"enabled":     s.Geo != nil,
			"attribution": attr,
		},
	})
}

// --- sites ---

type siteResponse struct {
	store.Site
	Visitors24h int64   `json:"visitors_24h"`
	Sparkline   []int64 `json:"sparkline"`
}

func (s *Server) listSites(w http.ResponseWriter, r *http.Request) {
	overview, err := stats.Overview(r.Context(), s.Store.Events, time.Now())
	if err != nil {
		s.serverError(w, err)
		return
	}
	sites := s.Store.Sites()
	out := make([]siteResponse, 0, len(sites))
	for _, site := range sites {
		res := siteResponse{Site: site, Sparkline: make([]int64, 24)}
		if o := overview[site.ID]; o != nil {
			res.Visitors24h, res.Sparkline = o.Visitors, o.Sparkline
		}
		out = append(out, res)
	}
	// Busiest first, then alphabetical.
	sortSites(out)
	writeJSON(w, http.StatusOK, out)
}

type siteBody struct {
	Domain   string `json:"domain"`
	Timezone string `json:"timezone"`
	Public   *bool  `json:"public"`
}

func (s *Server) createSite(w http.ResponseWriter, r *http.Request) {
	var body siteBody
	if !readJSON(w, r, &body) {
		return
	}
	domain := store.NormalizeDomain(body.Domain)
	if !validDomain(domain) {
		writeError(w, http.StatusBadRequest, "enter a domain like example.com")
		return
	}
	tz := body.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		writeError(w, http.StatusBadRequest, "unknown timezone")
		return
	}
	site, err := s.Store.CreateSite(domain, tz)
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, "a site already uses this domain")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, site)
}

// site resolves the {site} path value, or answers 404.
func (s *Server) site(w http.ResponseWriter, r *http.Request) (store.Site, bool) {
	site, ok := s.Store.LookupSite(r.PathValue("site"))
	if !ok {
		writeError(w, http.StatusNotFound, "site not found")
	}
	return site, ok
}

func (s *Server) getSite(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	// Visitors of a public dashboard don't need the site's old domains.
	if !s.Auth.Valid(r) {
		site.Aliases = []string{}
	}
	writeJSON(w, http.StatusOK, site)
}

func (s *Server) updateSite(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	var body siteBody
	if !readJSON(w, r, &body) {
		return
	}
	if body.Domain == "" && body.Timezone == "" && body.Public == nil {
		writeError(w, http.StatusBadRequest, "nothing to change: send a domain, timezone or public")
		return
	}
	domain := store.NormalizeDomain(body.Domain)
	if body.Domain != "" && !validDomain(domain) {
		writeError(w, http.StatusBadRequest, "enter a domain like example.com")
		return
	}
	if body.Timezone != "" {
		if _, err := time.LoadLocation(body.Timezone); err != nil {
			writeError(w, http.StatusBadRequest, "unknown timezone")
			return
		}
	}
	site, err := s.Store.UpdateSite(site.ID, store.SiteUpdate{Domain: domain, Timezone: body.Timezone, Public: body.Public})
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, "another site uses this domain")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "site not found")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, site)
}

func (s *Server) removeAlias(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	site, err := s.Store.RemoveAlias(site.ID, r.PathValue("alias"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "alias not found")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, site)
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	err := s.Store.DeleteSite(site.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "site not found")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- stats ---

func (s *Server) query(w http.ResponseWriter, r *http.Request) (stats.Query, bool) {
	site, ok := s.site(w, r)
	if !ok {
		return stats.Query{}, false
	}
	q, err := stats.ParseQuery(site, r.URL.Query(), time.Now(), func() (time.Time, bool) {
		return stats.FirstEvent(r.Context(), s.Store.Events, site.ID)
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return q, false
	}
	return q, true
}

type queryMeta struct {
	Period   string         `json:"period"`
	From     string         `json:"from"`
	To       string         `json:"to"` // inclusive, last day of the range
	Interval string         `json:"interval"`
	Timezone string         `json:"timezone"`
	Filters  []stats.Filter `json:"filters"`
}

func meta(q stats.Query) queryMeta {
	filters := q.Filters
	if filters == nil {
		filters = []stats.Filter{}
	}
	return queryMeta{
		Period: q.Period, Interval: q.Interval, Timezone: q.Loc.String(), Filters: filters,
		From: q.Start.Format(time.DateOnly), To: q.End.Add(-time.Nanosecond).Format(time.DateOnly),
	}
}

func (s *Server) aggregate(w http.ResponseWriter, r *http.Request) {
	q, ok := s.query(w, r)
	if !ok {
		return
	}
	res, err := stats.Aggregate(r.Context(), s.Store.Events, q, time.Now())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": meta(q), "current": res.Current, "previous": res.Previous})
}

func (s *Server) timeseries(w http.ResponseWriter, r *http.Request) {
	q, ok := s.query(w, r)
	if !ok {
		return
	}
	points, err := stats.Timeseries(r.Context(), s.Store.Events, q)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": meta(q), "points": points})
}

func (s *Server) breakdown(w http.ResponseWriter, r *http.Request) {
	q, ok := s.query(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 10
	}
	res, err := stats.Breakdown(r.Context(), s.Store.Events, q, r.PathValue("dim"), limit)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown dimension") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// appSummary serves active and new users for app sites.
func (s *Server) appSummary(w http.ResponseWriter, r *http.Request) {
	q, ok := s.query(w, r)
	if !ok {
		return
	}
	res, err := stats.App(r.Context(), s.Store.Events, q, time.Now())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// retention serves day cohorts; ?days= sets how many cohorts (default 14).
func (s *Server) retention(w http.ResponseWriter, r *http.Request) {
	q, ok := s.query(w, r)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 14
	}
	cohorts, err := stats.Retention(r.Context(), s.Store.Events, q, time.Now(), days)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cohorts": cohorts})
}

func (s *Server) realtime(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	n, err := stats.Realtime(r.Context(), s.Store.Events, site.ID, time.Now())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"visitors": n})
}

func (s *Server) locations(w http.ResponseWriter, r *http.Request) {
	q, ok := s.query(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	points, err := stats.Locations(r.Context(), s.Store.Events, q, limit)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": points})
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	site, ok := s.site(w, r)
	if !ok {
		return
	}
	res, err := stats.Live(r.Context(), s.Store.Events, site.ID, time.Now(), 300)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- static ---

func (s *Server) serveTracker(w http.ResponseWriter, r *http.Request) {
	script, cache := s.Tracker, "public, max-age=3600"
	if s.TrackerPath != "" {
		script, _ = os.ReadFile(s.TrackerPath)
		cache = "no-cache"
	}
	if len(script) == 0 {
		http.Error(w, "tracker not built: run `bun run build`", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(script)
}

// serveApp serves built assets and falls back to the SPA shell for client
// routes.
func (s *Server) serveApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")

	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p != "" && p != "_shell.html" {
		if f, err := fs.Stat(s.Dist, p); err == nil && !f.IsDir() {
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, s.Dist, p)
			return
		}
		if strings.HasPrefix(p, "assets/") {
			http.NotFound(w, r)
			return
		}
	}
	// Signed-out visitors get the sign-in page before any dashboard HTML, and
	// signed-in visitors who open it are sent on.
	signedIn := s.Auth.Valid(r)
	if p == "login" {
		if signedIn {
			http.Redirect(w, r, nextPath(r.URL.Query().Get("redirect")), http.StatusFound)
			return
		}
	} else if !signedIn && !s.publicPage(p) {
		target := "/login"
		if r.URL.Path != "/" {
			target += "?redirect=" + url.QueryEscape(r.URL.RequestURI())
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}

	shell, err := fs.ReadFile(s.Dist, "_shell.html")
	if err != nil {
		http.Error(w, "dashboard not built: run `bun run build`", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(shell)
}

// publicPage reports whether p (without its leading slash) is the dashboard
// of a public site, which signed-out visitors may open.
func (s *Server) publicPage(p string) bool {
	ref, ok := strings.CutPrefix(p, "sites/")
	if !ok || ref == "" || strings.Contains(ref, "/") {
		return false
	}
	site, ok := s.Store.LookupSite(ref)
	return ok && site.Public
}

// nextPath returns next if it is a path on this site, otherwise "/".
func nextPath(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.HasPrefix(next, "/\\") {
		return next
	}
	return "/"
}

// --- helpers ---

func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.Log.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !isJSON(r) {
		writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func validDomain(d string) bool {
	if len(d) < 3 || len(d) > 253 || !strings.Contains(d, ".") && d != "localhost" {
		return false
	}
	for _, c := range d {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

func sortSites(sites []siteResponse) {
	slices.SortFunc(sites, func(a, b siteResponse) int {
		if c := cmp.Compare(b.Visitors24h, a.Visitors24h); c != 0 {
			return c
		}
		return cmp.Compare(a.Domain, b.Domain)
	})
}
