import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/solid-router";
import { ArrowLeft, CircleCheck, Globe, LoaderCircle, Lock } from "lucide-solid";
import { createEffect, createSignal, For, Show } from "solid-js";
import { CodeBlock, trackingSnippet } from "~/components/code-block";
import { Button } from "~/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle, Surface } from "~/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "~/components/ui/dialog";
import { Input, Label, NativeSelect } from "~/components/ui/input";
import { api, type Site } from "~/lib/api";
import { errorMessage, t } from "~/lib/i18n";
import { timezones } from "~/lib/timezones";
import { ready } from "~/lib/query";

export const Route = createFileRoute("/_app/sites/$siteId/settings")({
  validateSearch: (s: Record<string, unknown>): { welcome?: boolean } => ({ welcome: s.welcome === true || undefined }),
  component: Settings,
});

function Settings() {
  const params = Route.useParams();
  const search = Route.useSearch();
  const siteId = () => params().siteId;
  const sites = useQuery(() => ({ queryKey: ["sites"], queryFn: api.sites }));
  const site = () => ready(sites)?.find((s) => String(s.id) === siteId());

  return (
    <div class="mx-auto flex max-w-3xl flex-col gap-6">
      <div>
        <Link
          to="/sites/$siteId"
          params={{ siteId: siteId() }}
          class="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
        >
          <ArrowLeft class="size-4" /> {t().settings.dashboard}
        </Link>
        <h1 class="mt-3 text-2xl font-semibold tracking-tight">{t().settings.title}</h1>
      </div>

      <Show when={ready(sites) && !site()}>
        <Surface class="p-10 text-center">
          <p class="font-medium">{t().common.siteNotFound}</p>
          <Link to="/" class="mt-2 inline-block text-sm text-muted-foreground underline underline-offset-4">
            {t().common.backToSites}
          </Link>
        </Surface>
      </Show>

      <Show when={site()}>
        {(s) => (
          <>
            <Show when={search().welcome}>
              <Surface class="bg-chart-wash p-5 shadow-none ring-chart-1/30">
                <p class="font-medium">{t().settings.ready(s().domain)}</p>
                <p class="mt-1 text-sm text-muted-foreground">{t().settings.pasteSnippet}</p>
              </Surface>
            </Show>

            <TrackingCard siteId={siteId()} domain={s().domain} hasData={s().visitors_24h > 0} />
            <AppSdkCard appId={s().domain} />
            <DomainCard site={s()} />
            <TimezoneCard siteId={siteId()} timezone={s().timezone} />
            <VisibilityCard site={s()} />
            <DangerCard siteId={siteId()} domain={s().domain} />
          </>
        )}
      </Show>
    </div>
  );
}

function AppSdkCard(props: { appId: string }) {
  const origin = typeof location === "undefined" ? "https://your-tinytraffic-host" : location.origin;
  const ios = `TinyTraffic.shared.configure(endpoint: "${origin}", appId: "${props.appId}")`;
  const ohos = `TinyTraffic.init(this.context, { endpoint: '${origin}', appId: '${props.appId}' });`;
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t().settings.appTitle}</CardTitle>
        <CardDescription>{t().settings.appDescription(props.appId)}</CardDescription>
      </CardHeader>
      <CardContent class="grid gap-4">
        <div class="grid gap-2">
          <h3 class="text-sm font-medium">iOS</h3>
          <CodeBlock code={ios} />
        </div>
        <div class="grid gap-2">
          <h3 class="text-sm font-medium">HarmonyOS</h3>
          <CodeBlock code={ohos} />
        </div>
        <p class="text-xs text-muted-foreground">{t().settings.appSdkHint}</p>
      </CardContent>
    </Card>
  );
}

function TrackingCard(props: { siteId: string; domain: string; hasData: boolean }) {
  const live = useQuery(() => ({
    queryKey: ["realtime", props.siteId],
    queryFn: () => api.realtime(props.siteId),
    refetchInterval: 5_000,
  }));
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t().settings.trackingTitle}</CardTitle>
        <CardDescription>{t().settings.trackingDescription}</CardDescription>
      </CardHeader>
      <CardContent class="grid gap-5">
        <CodeBlock code={trackingSnippet(props.domain)} />
        <div class="flex items-center gap-2 text-sm">
          <Show
            when={props.hasData || (ready(live)?.visitors ?? 0) > 0}
            fallback={
              <>
                <LoaderCircle class="size-4 animate-spin text-muted-foreground" />
                <span class="text-muted-foreground">{t().settings.waiting(props.domain)}</span>
              </>
            }
          >
            <CircleCheck class="size-4 text-positive" />
            <span>{t().settings.receiving}</span>
          </Show>
        </div>
        <div class="grid gap-2 border-t pt-5">
          <h3 class="text-sm font-medium">{t().settings.customEvents}</h3>
          <p class="text-sm text-muted-foreground">
            {t().settings.customEventsDescription}
          </p>
          <CodeBlock code={`tinytraffic("Signup", { props: { plan: "pro" } })`} />
          <p class="text-xs text-muted-foreground">
            {t().settings.excludeOwn(
              <code class="rounded bg-recessed px-1 py-0.5 ring ring-border">localStorage.tinytraffic_ignore = "true"</code>,
            )}
          </p>
        </div>
      </CardContent>
    </Card>
  );
}

function DomainCard(props: { site: Site }) {
  const qc = useQueryClient();
  const [domain, setDomain] = createSignal(props.site.domain);
  createEffect(() => setDomain(props.site.domain));
  const save = useMutation(() => ({
    mutationFn: () => api.updateSite(props.site.id, { domain: domain() }),
    onSuccess: (site) => {
      setDomain(site.domain);
      return qc.invalidateQueries({ queryKey: ["sites"] });
    },
  }));
  const removeAlias = useMutation(() => ({
    mutationFn: (alias: string) => api.removeAlias(props.site.id, alias),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["sites"] }),
  }));
  const changed = () => domain().trim() !== "" && domain().trim() !== props.site.domain;
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t().settings.domainTitle}</CardTitle>
        <CardDescription>{t().settings.domainDescription}</CardDescription>
      </CardHeader>
      <CardContent class="grid gap-4">
        <form
          class="flex flex-col gap-3 sm:flex-row"
          onSubmit={(e) => {
            e.preventDefault();
            if (changed()) save.mutate();
          }}
        >
          <Input
            aria-label={t().settings.domain}
            autocomplete="off"
            class="sm:max-w-xs"
            value={domain()}
            onInput={(e) => setDomain(e.currentTarget.value)}
            aria-invalid={save.isError}
          />
          <Button type="submit" variant="outline" disabled={!changed() || save.isPending}>
            {save.isSuccess && !changed() ? t().common.saved : t().common.save}
          </Button>
        </form>
        <Show when={save.error}>
          <p class="text-sm text-destructive">{errorMessage(save.error)}</p>
        </Show>
        <Show when={props.site.aliases.length > 0}>
          <div class="grid gap-2 border-t pt-4">
            <h3 class="text-sm font-medium">{t().settings.oldDomains}</h3>
            <p class="text-sm text-muted-foreground">
              {t().settings.oldDomainsDescription}
            </p>
            <ul class="divide-y rounded-lg border">
              <For each={props.site.aliases}>
                {(alias) => (
                  <li class="flex items-center justify-between gap-4 px-4 py-2">
                    <span class="truncate text-sm">{alias}</span>
                    <Button
                      variant="ghost"
                      size="sm"
                      disabled={removeAlias.isPending}
                      onClick={() => removeAlias.mutate(alias)}
                    >
                      {t().common.remove}
                    </Button>
                  </li>
                )}
              </For>
            </ul>
          </div>
        </Show>
      </CardContent>
    </Card>
  );
}

function TimezoneCard(props: { siteId: string; timezone: string }) {
  const qc = useQueryClient();
  const [tz, setTz] = createSignal(props.timezone);
  createEffect(() => setTz(props.timezone));
  const save = useMutation(() => ({
    mutationFn: () => api.updateSite(props.siteId, { timezone: tz() }),
    onSuccess: () => qc.invalidateQueries(),
  }));
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t().settings.timezoneTitle}</CardTitle>
        <CardDescription>{t().settings.timezoneDescription}</CardDescription>
      </CardHeader>
      <CardContent>
        <form
          class="flex flex-col gap-3 sm:flex-row"
          onSubmit={(e) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          <NativeSelect value={tz()} onChange={(e) => setTz(e.currentTarget.value)} class="sm:max-w-xs" aria-label={t().settings.timezone}>
            <For each={timezones()}>{(z) => (
                <option value={z} selected={z === tz()}>
                  {z.replaceAll("_", " ")}
                </option>
              )}</For>
          </NativeSelect>
          <Button type="submit" variant="outline" disabled={tz() === props.timezone || save.isPending}>
            {save.isSuccess && tz() === props.timezone ? t().common.saved : t().common.save}
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

function VisibilityCard(props: { site: Site }) {
  const qc = useQueryClient();
  const toggle = useMutation(() => ({
    mutationFn: () => api.updateSite(props.site.id, { public: !props.site.public }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["sites"] }),
  }));
  const link = () => `${location.origin}/sites/${props.site.id}`;
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t().settings.visibilityTitle}</CardTitle>
        <CardDescription>
          {props.site.public ? t().settings.visibilityPublic : t().settings.visibilityPrivate}
        </CardDescription>
      </CardHeader>
      <CardContent class="grid gap-4">
        <Show when={props.site.public}>
          <div class="grid gap-2">
            <Label>{t().settings.publicLink}</Label>
            <CodeBlock code={link()} />
          </div>
        </Show>
        <div>
          <Button variant="outline" disabled={toggle.isPending} onClick={() => toggle.mutate()}>
            {props.site.public ? <Lock /> : <Globe />}
            {props.site.public ? t().settings.makePrivate : t().settings.makePublic}
          </Button>
        </div>
        <Show when={toggle.error}>
          <p class="text-sm text-destructive">{errorMessage(toggle.error)}</p>
        </Show>
      </CardContent>
    </Card>
  );
}

function DangerCard(props: { siteId: string; domain: string }) {
  const [open, setOpen] = createSignal(false);
  const [confirm, setConfirm] = createSignal("");
  const qc = useQueryClient();
  const navigate = useNavigate();
  const remove = useMutation(() => ({
    mutationFn: () => api.deleteSite(props.siteId),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["sites"] });
      navigate({ to: "/" });
    },
  }));
  return (
    <Card class="ring-destructive/25">
      <CardHeader>
        <CardTitle>{t().settings.deleteTitle}</CardTitle>
        <CardDescription>{t().settings.deleteDescription}</CardDescription>
      </CardHeader>
      <CardContent>
        <Button variant="destructive" onClick={() => setOpen(true)}>
          {t().settings.deleteButton(props.domain)}
        </Button>
      </CardContent>
      <Dialog open={open()} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t().settings.deleteConfirmTitle(props.domain)}</DialogTitle>
            <DialogDescription>{t().settings.deleteConfirmDescription}</DialogDescription>
          </DialogHeader>
          <form
            class="grid gap-5"
            onSubmit={(e) => {
              e.preventDefault();
              if (confirm() === props.domain) remove.mutate();
            }}
          >
            <div class="grid gap-2">
              <Label for="confirm-domain">
                {t().settings.typeToConfirm(<span class="font-semibold">{props.domain}</span>)}
              </Label>
              <Input id="confirm-domain" autocomplete="off" value={confirm()} onInput={(e) => setConfirm(e.currentTarget.value)} />
            </div>
            <DialogFooter>
              <Button variant="outline" onClick={() => setOpen(false)}>
                {t().common.cancel}
              </Button>
              <Button type="submit" variant="destructive" disabled={confirm() !== props.domain || remove.isPending}>
                {t().settings.deleteSubmit}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </Card>
  );
}
