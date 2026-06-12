import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { controlURL } from './playwright.config';

// Logs toggles + YAML folding alignment:
//
//   - The wrap + timestamps toggles are CLIENT-SIDE display flips on the
//     server-rendered stream (classes on .ro-logpre) -- no refetch ever.
//   - Follow is the stateful Following⇄Follow button: active (accent) sticks
//     the stream scroll to its tail, inactive (quiet) leaves it alone;
//     re-activating snaps back to the tail.
//   - Download-logs is a REAL download (plain GET + the hx-boost opt-out --
//     boost would otherwise swap the attachment bytes into <body>).
//   - YAML folding (detail page): the fold chevron is a hover-revealed gutter
//     affordance, folding hides the nested block and shows the faint italic
//     "… N lines" note; Spec renders open and Status collapsed by default
//     (the shipped default, asserted here, not rebuilt).
//
// The pod_log.txt fixture serves 40 RFC3339-stamped lines (the stream
// overflows its max-height so Follow has a real tail) including one ~380-char
// line for the wrap bounding-box assertion.

const LOGS = '/clusters/e2e/namespaces/default/pods/nginx/logs';
const POD = '/clusters/e2e/namespaces/default/pods/nginx';

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the logs/fold geometry assertions assume the desktop viewport'
  );
  await control('/__control/reset');
});

test('wrap toggle reflows the long line client-side; timestamps toggle hides .log-ts -- no refetch', async ({
  page,
}) => {
  await page.goto(LOGS);
  const pre = page.locator('pre.ro-logpre');
  await expect(pre).toBeVisible();

  // Every toggle below must be a pure display flip: count any further
  // logs-page requests from here on.
  let refetches = 0;
  page.on('request', (request) => {
    if (request.url().includes('/logs')) {
      refetches++;
    }
  });

  // The deliberately long fixture line renders on ONE visual row unwrapped
  // (white-space: pre; the text overflows sideways) ...
  const longLine = page.locator('.ro-logpre .log-line', {
    hasText: 'upstream timeout while proxying',
  });
  const before = await longLine.boundingBox();
  expect(before).not.toBeNull();

  // ... and reflows TALL once wrap adds pre-wrap + break-word.
  await page.locator('#logWrap').check();
  await expect(pre).toHaveClass(/wrap/);
  const after = await longLine.boundingBox();
  expect(after).not.toBeNull();
  expect(after!.height).toBeGreaterThan(before!.height * 1.8);

  // Unchecking restores the single-row layout.
  await page.locator('#logWrap').uncheck();
  await expect(pre).not.toHaveClass(/wrap/);
  const restored = await longLine.boundingBox();
  expect(restored!.height).toBeLessThan(after!.height);

  // Timestamps render by default; unchecking hides every .log-ts span
  // (display:none via the stream's hide-ts class -- the spans stay in the DOM).
  await expect(page.locator('.ro-logpre .log-ts').first()).toBeVisible();
  await page.locator('#logTs').uncheck();
  await expect(pre).toHaveClass(/hide-ts/);
  await expect(page.locator('.ro-logpre .log-ts').first()).toBeHidden();
  await page.locator('#logTs').check();
  await expect(page.locator('.ro-logpre .log-ts').first()).toBeVisible();

  expect(refetches, 'display toggles must never refetch').toBe(0);
  expect(page.url()).toContain(LOGS);
});

test('Follow is stateful: Following sticks to the tail, Follow goes quiet, re-following snaps back', async ({
  page,
}) => {
  await page.goto(LOGS);
  const pre = page.locator('pre.ro-logpre');
  const follow = page.locator('#logFollow');
  const label = page.locator('#logFollow .follow-label');

  // The fixture stream overflows its max-height -- Follow has a real tail.
  expect(await pre.evaluate((el) => el.scrollHeight > el.clientHeight)).toBe(true);
  const atTail = () =>
    pre.evaluate((el) => Math.abs(el.scrollTop + el.clientHeight - el.scrollHeight) < 2);

  // Default state: active accent "Following", stream pinned to the tail.
  await expect(label).toHaveText('Following');
  await expect(follow).toHaveAttribute('aria-pressed', 'true');
  await expect(follow).not.toHaveClass(/quiet/);
  expect(await atTail()).toBe(true);

  // First click: quiet "Follow", scroll position is the user's again.
  await follow.click();
  await expect(label).toHaveText('Follow');
  await expect(follow).toHaveAttribute('aria-pressed', 'false');
  await expect(follow).toHaveClass(/quiet/);
  await pre.evaluate((el) => {
    el.scrollTop = 0;
  });
  expect(await atTail()).toBe(false);

  // Re-activating snaps the stream back to the tail.
  await follow.click();
  await expect(label).toHaveText('Following');
  await expect(follow).toHaveAttribute('aria-pressed', 'true');
  expect(await atTail()).toBe(true);
});

test('Download logs is a real plain-GET download with the stream as text; the page stays', async ({
  page,
}) => {
  await page.goto(LOGS);
  const downloadEvent = page.waitForEvent('download');
  await page.locator('.ro-detail-actions a[title="Download logs"]').click();
  const download = await downloadEvent;
  expect(download.suggestedFilename()).toBe('clusters_e2e_namespaces_default_pods_nginx_logs.txt');

  // Text content: one `pod container text` line per on-screen entry.
  const body = readFileSync((await download.path())!, 'utf8');
  expect(body).toContain('nginx nginx 2026-01-01T00:00:00Z Starting nginx');
  expect(body).toContain('GET / 200');

  // hx-boost opt-out: the attachment bytes never replace <body>.
  expect(page.url()).toContain(LOGS);
  await expect(page.locator('.ro-topbar')).toBeVisible();
  await expect(page.locator('pre.ro-logpre')).toBeVisible();
});

test('YAML fold: hover-revealed chevron folds the containers block to the "… N lines" note; Spec open + Status collapsed', async ({
  page,
}) => {
  await page.goto(POD);

  // The shipped YAML-card defaults, asserted not rebuilt: Spec open, Status collapsed.
  const specCard = page.locator('.ro-yaml-card[data-name="spec"]');
  await expect(specCard).not.toHaveClass(/is-collapsed/);
  await expect(page.locator('.ro-yaml-card[data-name="status"]')).toHaveClass(/is-collapsed/);

  // The spec card's first line opens the nested containers block; its fold
  // chevron is invisible until the line is hovered (the v2 gutter-hover
  // affordance, opacity-transitioned).
  const opener = specCard.locator('td.code pre > span[id*="line-"]').first();
  const toggle = opener.locator('.ro-fold-toggle');
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  await expect(toggle).toHaveCSS('opacity', '0');
  await opener.hover();
  await expect(toggle).toHaveCSS('opacity', '1');

  // Folding hides every deeper-indented child line and shows the faint
  // italic "… N lines" note on the opener (the fixture's containers block is
  // 5 lines deep). Every opener carries its own hidden note (ports: too), so
  // the assertion scopes to the opener line's note.
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  const note = opener.locator('.ro-fold-note');
  await expect(note).toBeVisible();
  await expect(note).toHaveText(/… 5 lines/);
  await expect(note).toHaveCSS('font-style', 'italic');
  const folded = specCard.locator('.ro-line-folded');
  await expect(folded).toHaveCount(5);
  await expect(folded.first()).toBeHidden();

  // Unfolding restores the block and re-hides the note.
  await toggle.click();
  await expect(specCard.locator('.ro-line-folded')).toHaveCount(0);
  await expect(note).toBeHidden();
});
