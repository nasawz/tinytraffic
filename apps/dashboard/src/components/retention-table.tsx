import { For, Show } from "solid-js";
import type { Cohort } from "~/lib/api";
import { formatNumber } from "~/lib/format";
import { t } from "~/lib/i18n";
import { Skeleton } from "./ui/skeleton";

/** Days after the first one that get a column. */
const columns = [1, 2, 3, 7, 14, 30];

/** Day cohorts as a heat map: each cell is the share of the cohort active n days later. */
export function RetentionTable(props: { cohorts?: Cohort[] }) {
  // Newest first, and only cohorts that have users.
  const rows = () => (props.cohorts ?? []).filter((c) => c.size > 0).reverse();
  return (
    <Show when={props.cohorts} fallback={<Skeleton class="h-48" />}>
      <Show when={rows().length > 0} fallback={<p class="py-8 text-center text-sm text-muted-foreground">{t().app.noRetention}</p>}>
        <div class="overflow-x-auto">
          <table class="w-full min-w-[34rem] border-separate border-spacing-1 text-sm tabular">
            <thead>
              <tr class="text-xs font-medium text-muted-foreground">
                <th class="px-2 py-1 text-left font-medium">{t().app.cohort}</th>
                <th class="px-2 py-1 text-right font-medium">{t().app.newUsers}</th>
                <For each={columns}>{(n) => <th class="px-2 py-1 text-center font-medium">{t().app.day(n)}</th>}</For>
              </tr>
            </thead>
            <tbody>
              <For each={rows()}>
                {(c) => (
                  <tr>
                    <td class="px-2 py-1 whitespace-nowrap text-muted-foreground">{c.date}</td>
                    <td class="px-2 py-1 text-right">{formatNumber(c.size)}</td>
                    <For each={columns}>
                      {(n) => {
                        const v = () => c.retained[n];
                        const pct = () => (v() == null ? null : (v()! / c.size) * 100);
                        return (
                          <td
                            class="rounded-md px-2 py-1 text-center"
                            style={
                              pct() === null
                                ? undefined
                                : { "background-color": `color-mix(in oklab, var(--chart-1) ${Math.round(Math.min(pct()!, 100) * 0.8)}%, transparent)` }
                            }
                            title={v() == null ? undefined : `${v()} / ${c.size}`}
                          >
                            {pct() === null ? <span class="text-muted-foreground/50">–</span> : `${Math.round(pct()!)}%`}
                          </td>
                        );
                      }}
                    </For>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </Show>
  );
}
