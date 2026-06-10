import { test, expect } from '@playwright/test';

// Smoke: the harness chain works end to end -- fakeapi serves discovery, the
// generated kubeconfig wires the cluster in, the built readout binary renders
// the clusters page with the app chrome.
test('clusters page renders the app chrome against the harness', async ({ page }) => {
  await page.goto('/clusters');
  await expect(page.locator('.ro-topbar')).toBeVisible();
  await expect(page.locator('.brand-name')).toHaveText('readout');
  // The kubeconfig context name is the cluster name readout serves.
  await expect(page.locator('main')).toContainText('e2e');
});
