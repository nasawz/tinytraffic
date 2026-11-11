import { keepPreviousData, useQuery } from "@tanstack/solid-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/solid-router";
import { Earth } from "lucide-solid";
import { createMemo, onCleanup, onMount, Show } from "solid-js";
import { AppSummaryTiles } from "~/components/app-summary";
import { BreakdownPanel } from "~/components/breakdown-panel";
import { FilterBar, parseFilter } from "~/components/filter-bar";
import { metricDefs, MetricTiles, type MetricKey } from "~/components/metric-tiles";
import { PeriodPicker, type PeriodValue } from "~/components/period-picker";
import { Realtime } from "~/components/realtime";
import { RetentionTable } from "~/components/retention-table";
import { TrafficChart } from "~/components/traffic-chart";
import { buttonVariants } from "~/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle, Surface } from "~/components/ui/card";
import { Skeleton } from "~/components/ui/skeleton";
import { api, type StatsParams } from "~/lib/api";
import { useSignedIn } from "~/lib/auth";
import { periodValues, type Period } from "~/lib/dims";
import { t } from "~/lib/i18n";
import { ready } from "~/lib/query";

type DashboardSearch = {
  period?: Period;
  from?: string;
  to?: string;
  f?: string[];
  metric?: MetricKey;
};

const metricKeys = new Set<string>(metricDefs.map((m) => m.key));
const isDate = (v: unknown): v is string => typeof v === "string" && /^\d{4}-\d{2}-\d{2}$/.test(v);

export const Route = createFileRoute("/_app/sites/$siteId/")({
  validateSearch: (s: Record<string, unknown>): DashboardSearch => {
    const period = periodValues.has(s.period as string) ? (s.period as Period) : undefined;
    const custom = period === "custom" && isDate(s.from) && isDate(s.to);
    return {
      period: period === "custom" && !custom ? undefined : period,
      from: custom ? (s.from as string) : undefined,
      to: custom ? (s.to as string) : undefined,
      f: Array.isArray(s.f) ? s.f.filter((x): x is string => typeof x === "string" && x.includes(":")) : undefined,
      metric: metricKeys.has(s.metric as string) ? (s.metric as MetricKey) : undefined,
    };
  },
  component: Dashboard,
});

function Dashboard() {
  const params = Route.useParams();
  const search = Route.useSearch();
  const navigate = useNavigate({ from: Route.fullPath });
  const siteId = () => params().siteId;
  // Signed-out viewers of a public dashboard see the stats only.
  const signedIn = useSignedIn();

  const stats = createMemo<StatsParams>(() => ({
    period: search().period ?? "30d",
    from: search().from,
    to: search().to,
    f: search().f ?? [],
  }));
  const metric = () => search().metric ?? "visitors";
  const metricDef = () => metricDefs.find((m) => m.key === metric())!;

  const aggregate = useQuery(() => ({
    queryKey: ["aggregate", siteId(), stats()],
    queryFn: () => api.aggregate(siteId(), stats()),
    placeholderData: keepPreviousData,
  }));
  const appSummary = useQuery(() => ({
    queryKey: ["app", siteId(), stats()],
    queryFn: () => api.app(siteId(), stats()),
    placeholderData: keepPreviousData,
  }));
  const isApp = () => ready(appSummary)?.is_app === true;
  const retention = useQuery(() => ({
    queryKey: ["retention", siteId(), stats()],
    queryFn: () => api.retention(siteId(), stats()),
    placeholderData: keepPreviousData,
    enabled: isApp(),
  }));
  const info = useQuery(() => ({ queryKey: ["info"], queryFn: api.info, staleTime: Infinity }));
  const series = useQuery(() => ({
    queryKey: ["timeseries", siteId(), stats()],
    queryFn: () => api.timeseries(siteId(), stats()),
    placeholderData: keepPreviousData,
  }));

  const setPeriod = (v: PeriodValue) =>
    navigate({ search: (prev) => ({ ...prev, period: v.period === "30d" ? undefined : v.period, from: v.from, to: v.to }) });

  const addFilter = (dim: string, value: string) =>
    navigate({
      search: (prev) => ({
        ...prev,
        f: [...(prev.f ?? []).filter((f) => parseFilter(f).dim !== dim), `${dim}:${value}`],
      }),
    });
  // L opens the live view (and closes it again from there).
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!signedIn() || e.metaKey || e.ctrlKey || e.altKey || e.key.toLowerCase() !== "l") return;
      if ((e.target as HTMLElement).closest("input, textarea, select, [contenteditable], [role=dialog]")) return;
      e.preventDefault();
      navigate({ to: "/sites/$siteId/live", params: { siteId: siteId() } });
    };
    window.addEventListener("keydown", onKey);
    onCleanup(() => window.removeEventListener("keydown", onKey));
  });

  const removeFilter = (f: string) =>
    navigate({ search: (prev) => ({ ...prev, f: (prev.f ?? []).filter((x) => x !== f) }) });

  return (
    <div class="flex flex-col gap-4">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="flex items-center gap-3">
          <Realtime site={siteId()} />
          <Show when={signedIn()}>
            <Link
              to="/sites/$siteId/live"
              params={{ siteId: siteId() }}
              class={buttonVariants({ variant: "outline", size: "sm" })}
              title={t().dashboard.liveViewTitle}
            >
              <Earth class="text-muted-foreground" /> {t().dashboard.liveView}
            </Link>
          </Show>
        </div>
        <PeriodPicker
          value={{ period: stats().period as Period, from: stats().from, to: stats().to }}
          timezone={aggregate.data?.query.timezone}
          onChange={setPeriod}
        />
      </div>

      <FilterBar
        filters={stats().f ?? []}
        onRemove={removeFilter}
        onClear={() => navigate({ search: (prev) => ({ ...prev, f: undefined }) })}
      />

      <Show when={aggregate.error?.message === "site not found"}>
        <Surface class="p-10 text-center">
          <p class="font-medium">{t().common.siteNotFound}</p>
          <Link to="/" class="mt-2 inline-block text-sm text-muted-foreground underline underline-offset-4">
            {t().common.backToSites}
          </Link>
        </Surface>
      </Show>

      <Card>
        <div class="p-1.5">
          <MetricTiles
            current={ready(aggregate)?.current}
            previous={ready(aggregate)?.previous}
            selected={metric()}
            onSelect={(k) => navigate({ search: (prev) => ({ ...prev, metric: k === "visitors" ? undefined : k }) })}
          />
        </div>
        <CardContent class={`px-3 pt-5 pb-2 transition-opacity sm:px-5 ${series.isPlaceholderData ? "opacity-60" : ""}`}>
          <Show when={ready(series)} fallback={<Skeleton class="h-[260px]" />}>
            {(data) => (
              <TrafficChart
                points={data().points}
                interval={data().query.interval}
                timezone={data().query.timezone}
                metric={{ label: t().metrics[metric()], value: (p) => p[metric()], format: metricDef().format }}
              />
            )}
          </Show>
        </CardContent>
      </Card>

      <Show when={isApp()}>
        <Card>
          <AppSummaryTiles data={ready(appSummary)} />
          <CardHeader>
            <CardTitle>{t().app.retentionTitle}</CardTitle>
            <CardDescription>{t().app.retentionDescription}</CardDescription>
          </CardHeader>
          <CardContent class="px-3 py-4 sm:px-5">
            <RetentionTable cohorts={ready(retention)?.cohorts} />
          </CardContent>
        </Card>
      </Show>

      <div class="grid gap-4 md:grid-cols-2">
        <BreakdownPanel
          id="Sources"
          title={t().dashboard.sources}
          site={siteId()}
          params={stats()}
          onFilter={addFilter}
          tabs={
            isApp()
              ? [{ dim: "source", label: t().dashboard.channels, column: t().dashboard.channel, countLabel: t().dashboard.views }]
              : [
                  { dim: "source", label: t().dashboard.sources, column: t().dashboard.source, countLabel: t().dashboard.pageviews },
                  { dim: "referrer", label: t().dashboard.referrers, column: t().dashboard.referrer, countLabel: t().dashboard.pageviews },
                  { dim: "utm_campaign", label: t().dashboard.campaigns, column: t().dashboard.campaign, countLabel: t().dashboard.pageviews },
                ]
          }
        />
        <BreakdownPanel
          id="Pages"
          title={t().dashboard.pages}
          site={siteId()}
          params={stats()}
          onFilter={addFilter}
          tabs={[
            {
              dim: "page",
              label: isApp() ? t().dashboard.screens : t().dashboard.top,
              column: isApp() ? t().dashboard.screen : t().dashboard.page,
              countLabel: isApp() ? t().dashboard.views : t().dashboard.pageviews,
            },
            { dim: "entry_page", label: t().dashboard.entry, column: t().dashboard.entryPage, countLabel: t().dashboard.visits },
            { dim: "exit_page", label: t().dashboard.exit, column: t().dashboard.exitPage, countLabel: t().dashboard.visits },
          ]}
        />
        <BreakdownPanel
          id="Locations"
          title={t().dashboard.locations}
          site={siteId()}
          params={stats()}
          onFilter={addFilter}
          tabs={[
            { dim: "country", label: t().dashboard.countries, column: t().dashboard.country, countLabel: t().dashboard.pageviews },
            { dim: "region", label: t().dashboard.regions, column: t().dashboard.region, countLabel: t().dashboard.pageviews },
            { dim: "city", label: t().dashboard.cities, column: t().dashboard.city, countLabel: t().dashboard.pageviews },
          ]}
          empty={ready(info)?.geoip.enabled === false ? t().dashboard.geoipNeeded : undefined}
          footer={
            <Show when={ready(info)?.geoip.attribution.url}>
              <a
                href={ready(info)!.geoip.attribution.url}
                target="_blank"
                rel="noopener"
                class="underline-offset-4 hover:text-foreground hover:underline"
              >
                {ready(info)!.geoip.attribution.text}
              </a>
            </Show>
          }
        />
        <BreakdownPanel
          id="Devices"
          title={t().dashboard.devices}
          site={siteId()}
          params={stats()}
          onFilter={addFilter}
          tabs={
            isApp()
              ? [
                  { dim: "app_version", label: t().dashboard.versions, column: t().dashboard.appVersion, countLabel: t().dashboard.views },
                  { dim: "os", label: t().dashboard.os, column: t().dashboard.operatingSystem, countLabel: t().dashboard.views },
                  { dim: "device_model", label: t().dashboard.models, column: t().dashboard.deviceModel, countLabel: t().dashboard.views },
                  { dim: "locale", label: t().dashboard.languages, column: t().dashboard.language, countLabel: t().dashboard.views },
                  { dim: "app_build", label: t().dashboard.builds, column: t().dashboard.build, countLabel: t().dashboard.views },
                ]
              : [
                  { dim: "browser", label: t().dashboard.browsers, column: t().dashboard.browser, countLabel: t().dashboard.pageviews },
                  { dim: "os", label: t().dashboard.os, column: t().dashboard.operatingSystem, countLabel: t().dashboard.pageviews },
                  { dim: "device", label: t().dashboard.size, column: t().dashboard.screenSize, countLabel: t().dashboard.pageviews },
                ]
          }
        />
        <BreakdownPanel
          id="Goals"
          title={t().dashboard.goals}
          class="md:col-span-2"
          site={siteId()}
          params={stats()}
          onFilter={addFilter}
          tabs={[
            {
              dim: "event",
              label: t().dashboard.events,
              column: t().dashboard.customEvent,
              countLabel: t().dashboard.events,
              conversion: true,
            },
          ]}
          empty={
            <>
              {t().dashboard.noEvents}
              <Show when={signedIn()}>
                {" "}
                <Link to="/sites/$siteId/settings" params={{ siteId: siteId() }} class="underline underline-offset-4 hover:text-foreground">
                  {t().dashboard.howToSend}
                </Link>
              </Show>
            </>
          }
        />
      </div>
    </div>
  );
}
