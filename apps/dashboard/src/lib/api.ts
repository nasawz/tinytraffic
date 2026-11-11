export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

async function request<T>(path: string, init?: RequestInit & { json?: unknown }): Promise<T> {
  const { json, ...rest } = init ?? {};
  const res = await fetch(path, {
    ...rest,
    credentials: "same-origin",
    headers: json !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: json !== undefined ? JSON.stringify(json) : rest.body,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(data.error ?? res.statusText, res.status);
  return data as T;
}

export type Site = {
  id: number;
  domain: string;
  timezone: string;
  /** Previous domains, still accepted from the tracker. */
  aliases: string[];
  /** Anyone can view a public site's dashboard, without signing in. */
  public: boolean;
  created_at: string;
};

export type SiteWithStats = Site & { visitors_24h: number; sparkline: number[] };

export type Metrics = {
  visitors: number;
  visits: number;
  pageviews: number;
  views_per_visit: number;
  bounce_rate: number;
  visit_duration: number;
};

export type Filter = { dim: string; value: string };

export type QueryMeta = {
  period: string;
  from: string;
  to: string;
  interval: "hour" | "day" | "month";
  timezone: string;
  filters: Filter[];
};

export type Aggregate = { query: QueryMeta; current: Metrics; previous: Metrics | null };
export type Point = Metrics & { date: string };
export type Timeseries = { query: QueryMeta; points: Point[] };
export type BreakdownRow = {
  value: string;
  visitors: number;
  count: number;
  /** Referrer domains: number of distinct links (with a path) under it. */
  links?: number;
};
export type Breakdown = { rows: BreakdownRow[]; total_visitors: number };

/** Active and new users of an app site. */
export type AppSummary = {
  is_app: boolean;
  dau: number;
  wau: number;
  mau: number;
  new_users: number;
  returning_users: number;
  total_users: number;
};

/** Users first seen on `date`; retained[n] is how many were active n days later (null: not yet). */
export type Cohort = { date: string; size: number; retained: (number | null)[] };

export type ServerInfo = {
  geoip: { enabled: boolean; attribution: { text: string; url: string } };
};

export type APIKeyScope = "read" | "write";

export type APIKey = {
  id: number;
  name: string;
  prefix: string;
  scope: APIKeyScope;
  created_at: string;
  last_used_at: string | null;
};

export type LocationPoint = { country: string; city: string; lat: number; lon: number; visitors: number };

/** A visit with activity in the last few minutes. */
export type LiveSession = {
  id: string;
  country: string;
  region: string;
  city: string;
  lat: number | null;
  lon: number | null;
  /** The last page viewed. */
  page: string;
  source: string;
  referrer: string;
  browser: string;
  os: string;
  device: string;
  pageviews: number;
  /** Custom events. */
  events: number;
  started_at: string;
  last_seen: string;
};

export type LiveEvent = { ts: string; session: string; name: string; page: string };

export type Live = { visitors: number; sessions: LiveSession[]; events: LiveEvent[] };

export type StatsParams = { period: string; from?: string; to?: string; f?: string[] };

function qs(p: StatsParams, extra?: Record<string, string>) {
  const s = new URLSearchParams({ period: p.period, ...extra });
  if (p.period === "custom" && p.from && p.to) {
    s.set("from", p.from);
    s.set("to", p.to);
  }
  for (const f of p.f ?? []) s.append("f", f);
  return s.toString();
}

/** A site's API path. `ref` is its ID (or a domain). */
const site = (ref: string | number) => `/api/sites/${encodeURIComponent(ref)}`;

export const api = {
  auth: () => request<{ needs_setup: boolean; authenticated: boolean }>("/api/auth"),
  setup: (password: string) => request("/api/auth/setup", { method: "POST", json: { password } }),
  login: (password: string) => request("/api/auth/login", { method: "POST", json: { password } }),
  logout: () => request("/api/auth/logout", { method: "POST" }),
  changePassword: (password: string, new_password: string) =>
    request("/api/auth/password", { method: "POST", json: { password, new_password } }),

  sites: () => request<SiteWithStats[]>("/api/sites"),
  createSite: (domain: string, timezone: string) =>
    request<Site>("/api/sites", { method: "POST", json: { domain, timezone } }),
  site: (ref: string | number) => request<Site>(site(ref)),
  updateSite: (ref: string | number, patch: { domain?: string; timezone?: string; public?: boolean }) =>
    request<Site>(site(ref), { method: "PATCH", json: patch }),
  removeAlias: (ref: string | number, alias: string) =>
    request<Site>(`${site(ref)}/aliases/${encodeURIComponent(alias)}`, { method: "DELETE" }),
  deleteSite: (ref: string | number) => request(site(ref), { method: "DELETE" }),

  aggregate: (ref: string, p: StatsParams) => request<Aggregate>(`${site(ref)}/aggregate?${qs(p)}`),
  timeseries: (ref: string, p: StatsParams) => request<Timeseries>(`${site(ref)}/timeseries?${qs(p)}`),
  breakdown: (ref: string, dim: string, p: StatsParams, limit = 10) =>
    request<Breakdown>(`${site(ref)}/breakdown/${dim}?${qs(p, { limit: String(limit) })}`),
  app: (ref: string, p: StatsParams) => request<AppSummary>(`${site(ref)}/app?${qs(p)}`),
  retention: (ref: string, p: StatsParams, days = 14) =>
    request<{ cohorts: Cohort[] }>(`${site(ref)}/retention?${qs(p, { days: String(days) })}`),
  realtime: (ref: string) => request<{ visitors: number }>(`${site(ref)}/realtime`),
  locations: (ref: string, p: StatsParams, limit = 200) =>
    request<{ points: LocationPoint[] }>(`${site(ref)}/locations?${qs(p, { limit: String(limit) })}`),
  live: (ref: string) => request<Live>(`${site(ref)}/live`),
  info: () => request<ServerInfo>("/api/info"),

  keys: () => request<APIKey[]>("/api/keys"),
  createKey: (name: string, scope: APIKeyScope) =>
    request<{ key: APIKey; token: string }>("/api/keys", { method: "POST", json: { name, scope } }),
  deleteKey: (id: number) => request(`/api/keys/${id}`, { method: "DELETE" }),
};
