import { test, expect, type Page, request as pwRequest } from '@playwright/test';
import { resolve } from 'node:path';
import { demoURL } from './playwright.config';

// demo.spec.ts is the thin, deterministic walk over `readout --demo` (render
// smoke + bulk-export coverage). The demo is the curated two-cluster tour (prod + staging,
// see internal/demo/scenario.go); because it lights up every render path, a
// single descent gives near-total render coverage. Four jobs, all against the
// real demo server (the `demo` Playwright project + its own webServer, breathing
// FROZEN via READOUT_DEMO_FREEZE=1 so every frame is still):
//
//   1. RENDER SMOKE  — every demo page renders with no error + a key element.
//   2. DETAIL DESCENT — the load-bearing part: open a pod detail (containers +
//      init-container section), a secret detail (key chips, masked values), a
//      node detail (conditions + capacity), and the long-annotation collapse —
//      list-only walking leaves these detail-only branches dark.
//   3. EXPORT (--grep export) — bulk YAML + TSV on ONE single-namespace list and
//      ONE _all-namespaces list; assert non-empty multi-doc output. Export is a
//      download ACTION, not a page; a page-render walk misses it.
//   4. LANDING SCREENSHOTS — plain page.screenshot captures into docs/screenshots
//      (the README's screenshot home). NOT toHaveScreenshot baselines.
//
// CONTRACT: snapshots/screenshots regenerate intentionally — the assertions
// below MUST NOT couple to demo content (a test must not break when the demo
// gains an entity for a screenshot). So we assert on STRUCTURE (a key element is
// present, output is multi-doc/non-empty), never on specific row counts or
// specific object text beyond the named navigation targets.

// The curated demo names this walk descends into (from scenario.go). These are
// navigation targets, not content assertions: the demo may grow more objects
// around them without touching this spec.
const POD_MULTI = '/clusters/prod/namespaces/databases/pods/orders-db-0'; // multi-container (postgres + exporter)
const POD_INIT = '/clusters/prod/namespaces/kube-system/pods/node-setup-fb12-zx'; // 2 init containers
const SECRET = '/clusters/prod/namespaces/kube-system/secrets/registry-creds'; // multi-key secret
const NODE = '/clusters/prod/nodes/worker-4'; // Ready + MemoryPressure condition
const ANNO_OBJECT = '/clusters/prod/namespaces/storefront/deployments/storefront-web'; // >120-char annotation
const STOREFRONT_PODS = '/clusters/prod/namespaces/storefront/pods'; // the large fleet (breathing target lives here)

// The repo's screenshot home — the README references docs/screenshots/*.png. The
// spec runs from tests/e2e, so the repo root is two levels up.
const SHOTS = resolve(__dirname, '../../docs/screenshots');

// settle awaits the self-hosted fonts before a screenshot so the capture never
// lands a fallback face.
async function settle(page: Page): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
}

test.describe('demo render smoke', () => {
  // List-level pages across the demo's themed namespaces: clusters → each
  // cluster overview → namespaces → key resource lists (healthy serving, failing,
  // stateful, the CRD zoo, batch, the empty namespace, the virtualized big
  // namespace, and the cross-cluster/_all fan-outs). One key element asserted per
  // page proves it rendered without an error card.
  const pages: Array<{ name: string; path: string; ready: (p: Page) => Promise<void> }> = [
    {
      name: 'clusters list',
      path: '/clusters',
      ready: async (p) => {
        await expect(p.locator('.ro-topbar')).toBeVisible();
        await expect(p.locator('main')).toContainText('prod');
        await expect(p.locator('main')).toContainText('staging');
      },
    },
    {
      name: 'prod cluster overview',
      path: '/clusters/prod',
      ready: async (p) => expect(p.locator('.ro-breadcrumb')).toBeVisible(),
    },
    {
      name: 'staging cluster overview',
      path: '/clusters/staging',
      ready: async (p) => expect(p.locator('.ro-breadcrumb')).toBeVisible(),
    },
    {
      name: 'prod namespaces',
      path: '/clusters/prod/namespaces',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
    {
      name: 'prod nodes',
      path: '/clusters/prod/nodes',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
    {
      name: 'storefront pods (the large healthy fleet)',
      path: '/clusters/prod/namespaces/storefront/pods',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
    {
      name: 'checkout pods (the incident: crash-loop + image-pull)',
      path: '/clusters/prod/namespaces/checkout/pods',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
    {
      name: 'databases statefulsets (stateful story)',
      path: '/clusters/prod/namespaces/databases/statefulsets',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
    {
      name: 'cert-manager certificates (CRD list + icon family)',
      path: '/clusters/prod/namespaces/cert-manager/certificates',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
    {
      name: 'payments cronjobs (batch)',
      path: '/clusters/prod/namespaces/payments/cronjobs',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
    {
      name: 'kube-system secrets',
      path: '/clusters/prod/namespaces/kube-system/secrets',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
    {
      name: 'empty namespace pods (empty-list render path)',
      path: '/clusters/prod/namespaces/default/pods',
      // The `default` namespace is empty: the empty-state card, NOT a table.
      ready: async (p) => expect(p.locator('.ro-topbar')).toBeVisible(),
    },
    {
      name: 'empty-OF-KIND namespace list (ingresses in cert-manager)',
      // cert-manager runs no Ingresses. A real apiserver answers an empty 200 for
      // any served kind in any namespace, so this list must render the empty-state
      // card, NOT the "Can't reach" error card — the regression guard for the
      // per-namespace empty-list fill (a served kind a namespace holds none of
      // used to 404 here).
      path: '/clusters/prod/namespaces/cert-manager/ingresses',
      ready: async (p) => {
        await expect(p.locator('.ro-empty-lg')).toBeVisible();
        await expect(p.locator('.ro-error-card')).toHaveCount(0);
        await expect(p.locator('main')).not.toContainText('Can’t reach');
      },
    },
    {
      name: 'empty-OF-KIND list (cronjobs in storefront)',
      path: '/clusters/prod/namespaces/storefront/cronjobs',
      ready: async (p) => {
        await expect(p.locator('.ro-empty-lg')).toBeVisible();
        await expect(p.locator('.ro-error-card')).toHaveCount(0);
      },
    },
    {
      name: 'storefront pods (virtualized large fleet)',
      path: '/clusters/prod/namespaces/storefront/pods',
      ready: async (p) => expect(p.locator('.ro-table-wrap.ro-windowed')).toBeVisible(),
    },
    {
      name: 'prod _all-namespaces pods',
      path: '/clusters/prod/namespaces/_all/pods',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
    {
      name: '_all-cluster _all-namespaces pods',
      path: '/clusters/_all/namespaces/_all/pods',
      ready: async (p) => expect(p.locator('table.ro-table td.cell-name').first()).toBeVisible(),
    },
  ];

  for (const { name, path, ready } of pages) {
    test(`renders: ${name}`, async ({ page }) => {
      const resp = await page.goto(path);
      expect(resp?.ok(), `${path} should respond 2xx`).toBeTruthy();
      await ready(page);
    });
  }
});

test.describe('demo detail descent', () => {
  // The load-bearing part: list-only walking leaves these detail-only render
  // branches dark, so each is opened and a key element asserted.

  test('pod detail: containers + init-container section', async ({ page }) => {
    // A multi-container pod proves the per-container rows render.
    await page.goto(POD_MULTI);
    await expect(page.locator('.ro-containers')).toBeVisible();
    await expect(page.locator('.ro-containers table.ro-table tbody tr').first()).toBeVisible();

    // A pod WITH init containers proves the init-container branch: the section
    // counts init containers and badges init rows.
    await page.goto(POD_INIT);
    const initPods = page.locator('.ro-containers');
    await expect(initPods).toBeVisible();
    await expect(initPods.locator('.ro-section-label')).toContainText('init');
    await expect(initPods.locator('.ro-kind-badge.init').first()).toBeVisible();
  });

  test('breathing target pod detail: containers section stays populated', async ({ page }) => {
    // The breathing loop pulses the first healthy storefront pod. The pulse must
    // be non-destructive: it preserves the pod's full object, so its detail page
    // keeps its containers section and a real created timestamp (a pulse that
    // replaced the pod with a metadata stub blanked both). The demo here runs
    // with breathing FROZEN, so this is the seeded baseline the live pulse must
    // never degrade below. The exact pod name is fleet-hashed, so navigate via
    // the list and open the first pod rather than hardcoding a name.
    await page.goto(STOREFRONT_PODS);
    await page.locator('table.ro-table td.cell-name a').first().click();
    await expect(page.locator('.ro-containers')).toBeVisible();
    await expect(page.locator('.ro-containers table.ro-table tbody tr').first()).toBeVisible();
    // A real created timestamp rides the detail (not blank/unknown).
    await expect(page.locator('main')).not.toContainText('<unknown>');
  });

  test('secret detail: key chips render, values masked', async ({ page }) => {
    await page.goto(SECRET);
    const block = page.locator('.ro-secret-data');
    await expect(block).toBeVisible();
    // The masked-values notice + at least one key row with a masked value.
    await expect(block).toContainText('Values masked');
    await expect(block.locator('.ro-secret-key').first()).toBeVisible();
    await expect(block.locator('.ro-secret-mask').first()).toBeVisible();
    // Read-only guarantee: a real secret value is never serialized to the page.
    await expect(page.locator('body')).not.toContainText('never-rendered-in-the-ui');
  });

  test('node detail: conditions + capacity kv', async ({ page }) => {
    await page.goto(NODE);
    await expect(page.locator('.ro-cond-pill').first()).toBeVisible();
    await expect(page.getByText('Capacity / Allocatable')).toBeVisible();
    await expect(page.locator('.ro-kv-cols')).toBeVisible();
  });

  test('long annotation collapse control', async ({ page }) => {
    await page.goto(ANNO_OBJECT);
    const block = page.locator('.anno-long');
    const toggle = block.locator('button[data-ro-annolong]');
    await expect(block).toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(block.locator('pre.anno-pre')).toBeHidden();
    // The collapse control reveals the payload in place (no navigation).
    await toggle.click();
    await expect(block.locator('pre.anno-pre')).toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  });
});

test.describe('demo export', () => {
  // Export is a download ACTION, not a page. The bulk YAML/TSV
  // endpoints are invoked directly via the request context (the same
  // `?download=yaml|tsv` grammar the bulk button drives) and asserted non-empty +
  // multi-doc. Both a single-namespace list and an _all-namespaces list are
  // covered: an empty list would 0-byte, so non-empty is the gate.

  async function get(url: string): Promise<{ status: number; body: string }> {
    const ctx = await pwRequest.newContext({ baseURL: demoURL });
    try {
      const resp = await ctx.get(url);
      return { status: resp.status(), body: await resp.text() };
    } finally {
      await ctx.dispose();
    }
  }

  test('single-namespace bulk YAML is non-empty multi-doc', async () => {
    // databases runs the orders-db StatefulSet's two stable-named pods; request
    // both so the multi-document join is exercised (their names resolve, so no
    // `# not found` comment doc).
    const { status, body } = await get(
      '/clusters/prod/namespaces/databases/pods?download=yaml&names=orders-db-0,orders-db-1'
    );
    expect(status).toBe(200);
    expect(body.length).toBeGreaterThan(0);
    // Multi-doc: more than one `kind:` entry and a `---` separator between docs.
    const kinds = body.match(/^kind:/gm) ?? [];
    expect(kinds.length).toBeGreaterThan(1);
    expect(body).toMatch(/^---$/m);
  });

  test('single-namespace bulk TSV is non-empty header + rows', async () => {
    const { status, body } = await get('/clusters/prod/namespaces/databases/pods?download=tsv');
    expect(status).toBe(200);
    const lines = body.split('\n').filter((l) => l.trim().length > 0);
    // A header row plus at least one data row.
    expect(lines.length).toBeGreaterThan(1);
    expect(lines[0]).toContain('\t'); // tab-separated header
  });

  test('_all-namespaces bulk YAML is non-empty multi-doc (ns/name grammar)', async () => {
    // _all-namespaces uses the `ns/name` row grammar. Two pods from different
    // namespaces prove the join across namespaces.
    const { status, body } = await get(
      '/clusters/prod/namespaces/_all/pods?download=yaml&names=databases/orders-db-0,search/opensearch-0'
    );
    expect(status).toBe(200);
    expect(body.length).toBeGreaterThan(0);
    const kinds = body.match(/^kind:/gm) ?? [];
    expect(kinds.length).toBeGreaterThan(1);
    expect(body).toMatch(/^---$/m);
  });

  test('_all-namespaces bulk TSV is non-empty header + rows', async () => {
    const { status, body } = await get('/clusters/prod/namespaces/_all/pods?download=tsv');
    expect(status).toBe(200);
    const lines = body.split('\n').filter((l) => l.trim().length > 0);
    expect(lines.length).toBeGreaterThan(1);
    // The _all-namespaces TSV carries a Namespace column in the header.
    expect(lines[0]).toContain('Namespace');
  });
});

test.describe('demo landing screenshots', () => {
  // Plain page.screenshot captures into docs/screenshots (the README's home) —
  // a VERIFIED byproduct of the demo run, not manual capture. NOT toHaveScreenshot
  // baselines: these regenerate intentionally, so they are written, not diffed.
  // The four names match the README references (pods, cluster-overview, detail,
  // palette). Gated behind RO_SHOTS so a plain `make e2e` stays tree-clean: these
  // overwrite tracked product PNGs, so run `RO_SHOTS=1 make e2e` to regenerate
  // them deliberately, then review and commit the new captures.
  test.skip(!process.env.RO_SHOTS, 'set RO_SHOTS=1 to regenerate landing screenshots');

  test('capture pods.png', async ({ page }) => {
    await page.goto('/clusters/prod/namespaces/checkout/pods'); // the incident: rich status cells
    await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
    await settle(page);
    await page.screenshot({ path: resolve(SHOTS, 'pods.png'), fullPage: false });
  });

  test('capture cluster-overview.png', async ({ page }) => {
    await page.goto('/clusters/prod');
    await expect(page.locator('.ro-breadcrumb')).toBeVisible();
    await settle(page);
    await page.screenshot({ path: resolve(SHOTS, 'cluster-overview.png'), fullPage: false });
  });

  test('capture detail.png', async ({ page }) => {
    await page.goto(POD_MULTI); // a rich detail: chips, containers, YAML cards
    await expect(page.locator('.ro-containers')).toBeVisible();
    await settle(page);
    await page.screenshot({ path: resolve(SHOTS, 'detail.png'), fullPage: false });
  });

  test('capture palette.png', async ({ page }) => {
    await page.goto('/clusters/prod/namespaces/storefront/pods');
    await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
    await page.keyboard.press('ControlOrMeta+k');
    await expect(page.locator('#ro-palette')).toHaveClass(/open/);
    await page.locator('#ro-palette-input').fill('pods');
    await expect(page.locator('#ro-palette-list .ro-pal-item').first()).toBeVisible();
    await settle(page);
    await page.screenshot({ path: resolve(SHOTS, 'palette.png'), fullPage: false });
  });
});
