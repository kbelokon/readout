import { test, expect, type Page } from '@playwright/test';

// demo-crawl.spec.ts is the link crawler: for each demo cluster it discovers
// every resource type (cluster-scoped + the _all-namespaces kinds), visits each
// list, and opens up to SAMPLE objects PER KIND. For each object it checks the
// Default detail (non-blank), the YAML tab (renders the object), and the Events
// tab (no error). Every page must respond 2xx and show no "Can't reach" error
// card. This is the guard that catches dead links, blank detail pages, and
// broken tabs automatically, instead of a human finding them by clicking.

const CLUSTERS = ['prod', 'staging'];
const SAMPLE = 3; // objects opened per kind

// safeGoto retries net::ERR_ABORTED: rapid navigation across the crawl races
// readout's background SSE/htmx requests, which can abort an in-flight goto. A
// short retry makes the crawl deterministic without masking real failures.
async function safeGoto(page: Page, url: string) {
  for (let attempt = 0; ; attempt++) {
    try {
      return await page.goto(url, { waitUntil: 'domcontentloaded' });
    } catch (e) {
      if (attempt >= 3) throw e;
      await page.waitForTimeout(150);
    }
  }
}

async function tableLinks(page: Page, path: string): Promise<{ ok: boolean; hrefs: string[] }> {
  const resp = await safeGoto(page, path);
  if (!resp?.ok()) return { ok: false, hrefs: [] };
  const hrefs = await page
    .locator('table a[href]')
    .evaluateAll((els) => els.map((e) => (e as HTMLAnchorElement).getAttribute('href') || ''));
  return { ok: true, hrefs: [...new Set(hrefs.filter(Boolean))] };
}

async function rowDetailLinks(page: Page): Promise<string[]> {
  return page
    .locator('table.ro-table td.cell-name a')
    .evaluateAll((els) =>
      els.slice(0, 8).map((e) => (e as HTMLAnchorElement).getAttribute('href') || '')
    );
}

// kindPath strips the trailing object name so a kind can be sampled (open a few
// objects of each kind, not every object).
function kindPath(href: string): string {
  return href.replace(/\/[^/]+$/, '/');
}

test.describe('demo crawl', () => {
  for (const cluster of CLUSTERS) {
    test(`every resource type + sampled details/tabs render: ${cluster}`, async ({ page }) => {
      test.setTimeout(240_000); // an exhaustive crawl over every kind × 3 tabs is inherently long
      const clusterTypes = await tableLinks(page, `/clusters/${cluster}/_resource-types`);
      const nsTypes = await tableLinks(page, `/clusters/${cluster}/namespaces/_all/_resource-types`);
      const lists = [...new Set([...clusterTypes.hrefs, ...nsTypes.hrefs])].filter((h) =>
        h.startsWith(`/clusters/${cluster}/`)
      );
      expect(lists.length, `${cluster} should advertise resource types`).toBeGreaterThan(5);

      const perKind = new Map<string, number>();
      const problems: string[] = [];

      const errored = async (where: string): Promise<boolean> => {
        if (await page.locator('.ro-error-card').count()) {
          problems.push(`${where} -> error card`);
          return true;
        }
        return false;
      };

      for (const href of lists) {
        const resp = await safeGoto(page, href);
        if (!resp?.ok()) {
          problems.push(`LIST ${href} -> HTTP ${resp?.status()}`);
          continue;
        }
        if (await errored(`LIST ${href}`)) continue;

        for (const detail of await rowDetailLinks(page)) {
          if (!detail) continue;
          const kp = kindPath(detail);
          if ((perKind.get(kp) ?? 0) >= SAMPLE) continue;

          // --- Default tab ---
          const r = await safeGoto(page, detail);
          if (!r?.ok()) {
            problems.push(`DETAIL ${detail} -> HTTP ${r?.status()}`);
            continue;
          }
          if (await errored(`DETAIL ${detail}`)) continue;
          // A "first link" can be a drill-down into another LIST (a Namespace row
          // links to that namespace's pods), not an object detail. Only a detail
          // page carries the tab strip.
          if (!(await page.locator('.ro-tabs').count())) continue;
          perKind.set(kp, (perKind.get(kp) ?? 0) + 1);

          const content = page.locator(
            '.ro-card-content, .ro-section, .ro-chip, .ro-containers, .ro-secret-data'
          );
          if (!(await content.count())) problems.push(`DETAIL ${detail} -> blank (no content)`);

          // --- YAML tab: must render the object's manifest ---
          const ry = await safeGoto(page, `${detail}?view=yaml`);
          if (!ry?.ok()) {
            problems.push(`YAML ${detail} -> HTTP ${ry?.status()}`);
          } else if (!(await errored(`YAML ${detail}`))) {
            const yaml = await page.locator('.ro-yaml-view').innerText().catch(() => '');
            if (!/kind:/.test(yaml)) problems.push(`YAML ${detail} -> no manifest rendered`);
          }

          // --- Events tab: must render (events table or empty state), no error ---
          const re = await safeGoto(page, `${detail}?view=events`);
          if (!re?.ok()) {
            problems.push(`EVENTS ${detail} -> HTTP ${re?.status()}`);
          } else {
            await errored(`EVENTS ${detail}`);
          }
        }
      }

      expect(problems, `crawl found broken pages in ${cluster}:\n${problems.join('\n')}`).toEqual([]);
    });
  }
});
