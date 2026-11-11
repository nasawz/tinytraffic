import { For, Show } from "solid-js";
import type { AppSummary } from "~/lib/api";
import { formatFull, formatNumber } from "~/lib/format";
import { t } from "~/lib/i18n";
import { Skeleton } from "./ui/skeleton";

/** Active and new users of an app, as a row of tiles. */
export function AppSummaryTiles(props: { data?: AppSummary }) {
  const tiles = () => [
    { label: t().app.dau, value: props.data?.dau },
    { label: t().app.wau, value: props.data?.wau },
    { label: t().app.mau, value: props.data?.mau },
    { label: t().app.newUsers, value: props.data?.new_users },
    { label: t().app.returningUsers, value: props.data?.returning_users },
    { label: t().app.totalUsers, value: props.data?.total_users },
  ];
  return (
    <div class="grid grid-cols-2 gap-1 p-1.5 sm:grid-cols-3 lg:grid-cols-6" title={t().app.activeHint}>
      <For each={tiles()}>
        {(tile) => (
          <div class="rounded-lg px-3.5 py-3">
            <div class="text-xs font-medium text-muted-foreground">{tile.label}</div>
            <Show when={tile.value !== undefined} fallback={<Skeleton class="mt-2 h-7 w-20" />}>
              <div class="mt-1.5 text-2xl font-semibold tracking-tight tabular" title={formatFull(tile.value!)}>
                {formatNumber(tile.value!)}
              </div>
            </Show>
          </div>
        )}
      </For>
    </div>
  );
}
