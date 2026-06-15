import { test, expect, type Page } from '@playwright/test';

// demo-crawl.spec.ts is the link crawler: for each demo cluster it discovers
// every resource type (cluster-scoped + the _all-namespaces kinds), visits each
// list, and opens the first object's detail — once per kind — asserting the page
// responds 2xx, shows no "Can't reach" error card, and the detail is NOT blank
// (it renders at least one content section). This is the guard that catches dead
// resource-type links and empty detail pages automatically, instead of a human
// finding them by clicking around.

const CLUSTERS = ['prod', 'staging'];

async function tableLinks(page: Page, path: string): Promise<string[]> {
  const resp = await page.goto(path);
  expect(resp?.ok(), `${path} should respond 2xx`).toBeTruthy();
  const hrefs = await page
    .locator('table a[href]')
    .evaluateAll((els) => els.map((e) => (e as HTMLAnchorElement).getAttribute('href') || ''));
  return [...new Set(hrefs.filter(Boolean))];
}

// kindPath strips the trailing object name so details can be de-duplicated by
// kind (open the first object of each kind once, not every object).
function kindPath(href: string): string {
  return href.replace(/\/[^/]+$/, '/');
}

test.describe('demo crawl', () => {
  for (const cluster of CLUSTERS) {
    test(`every resource type + a detail per kind renders: ${cluster}`, async ({ page }) => {
      const clusterTypes = await tableLinks(page, `/clusters/${cluster}/_resource-types`);
      const nsTypes = await tableLinks(page, `/clusters/${cluster}/namespaces/_all/_resource-types`);
      const lists = [...new Set([...clusterTypes, ...nsTypes])].filter((h) =>
        h.startsWith(`/clusters/${cluster}/`)
      );
      expect(lists.length, `${cluster} should advertise resource types`).toBeGreaterThan(5);

      const seenKind = new Set<string>();
      const problems: string[] = [];

      for (const href of lists) {
        const resp = await page.goto(href);
        if (!resp?.ok()) {
          problems.push(`LIST ${href} -> HTTP ${resp?.status()}`);
          continue;
        }
        if (await page.locator('.ro-error-card').count()) {
          problems.push(`LIST ${href} -> error card`);
          continue;
        }

        const first = page.locator('table.ro-table td.cell-name a').first();
        if (!(await first.count())) continue; // empty list (valid)
        const detail = await first.getAttribute('href');
        if (!detail || seenKind.has(kindPath(detail))) continue;
        seenKind.add(kindPath(detail));

        const r = await page.goto(detail);
        if (!r?.ok()) {
          problems.push(`DETAIL ${detail} -> HTTP ${r?.status()}`);
          continue;
        }
        if (await page.locator('.ro-error-card').count()) {
          problems.push(`DETAIL ${detail} -> error card`);
          continue;
        }
        // Some "first links" are drill-downs into another LIST (a Namespace row
        // links to that namespace's pods), not an object detail. Only a detail
        // page carries the tab strip; a list has none — for those we only need
        // the no-error check above.
        if (!(await page.locator('.ro-tabs').count())) continue;
        // Non-blank: a real detail renders some content on the Default tab —
        // curated card content, a metadata/labels section, label chips, the
        // containers table, or secret data. A body with none of these (only the
        // empty detail shell + header) is the blank-page bug.
        const content = page.locator(
          '.ro-card-content, .ro-section, .ro-chip, .ro-containers, .ro-secret-data'
        );
        if (!(await content.count())) {
          problems.push(`DETAIL ${detail} -> blank (no content)`);
        }
      }

      expect(problems, `crawl found broken pages in ${cluster}:\n${problems.join('\n')}`).toEqual([]);
    });
  }
});
