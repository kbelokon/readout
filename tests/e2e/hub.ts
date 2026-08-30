import { baseURL, controlURL } from './playwright.config';

// hub.ts -- the WatchHub-aware fixture reset shared by the Live specs.
//
// Upstream Kubernetes watches belong to the POD, not to a browser: readout's
// WatchHub owns one LIST+watch per (credential, cluster, GVR, namespace,
// selector) key, shares it across every SSE subscriber, and RETAINS a source
// for 30 seconds after its last subscriber leaves. Two consequences shape every
// Live spec:
//
//   1. An open upstream watch is never evidence that a particular browser is
//      streaming -- a source retained from the previous spec is watching too.
//      Watch counts are therefore only ever read in the positive direction;
//      "this browser stopped" is asserted on the client (roLive.stats, the
//      absence of new `_stream` requests).
//   2. `/__control/reset` reseeds the fake apiserver and expires every watch
//      cursor, so a retained source discovers a 410 on its next re-watch and
//      relists. That relist publishes a forced snapshot. A spec that opened its
//      stream before the relist landed would receive the pre-reset table first
//      and the forced snapshot a moment later -- one extra swap in the middle
//      of a spec that counts them. resetFixture waits for it.

export interface HubCounters {
  sources: number;
  relists: number;
}

// hubCounters reads the two WatchHub families the reset barrier needs off
// readout's own /metrics surface: how many sources the pod currently owns and
// how many 410 recovery LISTs it has performed.
export async function hubCounters(): Promise<HubCounters> {
  const response = await fetch(`${baseURL}/metrics`);
  if (!response.ok) throw new Error(`metrics: ${response.status}`);
  const text = await response.text();
  const read = (name: string): number => {
    const line = text.split('\n').find((candidate) => candidate.startsWith(`${name} `));
    return line ? Number(line.slice(name.length + 1)) : 0;
  };
  return {
    sources: read('readout_watchhub_sources_active'),
    relists: read('readout_watchhub_relists_total'),
  };
}

// resetFixture reseeds the fake apiserver AND waits for readout to notice, so
// every Live spec starts against sources that already know the reseeded state
// and against a pod with no re-watch in flight (an in-flight re-watch would
// race a spec's one-shot `/__control/watch-401`, which is global).
//
// EVERY retained source relists exactly once after a reset -- each holds a
// cursor the reset expired -- so the barrier is "as many relists as there were
// sources". A source whose 30s retention expires during the wait counts too:
// it will never relist, and it is equally gone.
export async function resetFixture(): Promise<void> {
  const before = await hubCounters();
  const reset = await fetch(`${controlURL}/__control/reset`);
  if (!reset.ok) {
    throw new Error(`control reset: ${reset.status} ${await reset.text()}`);
  }
  if (before.sources === 0) return;
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    const now = await hubCounters();
    const relisted = now.relists - before.relists;
    const retired = Math.max(0, before.sources - now.sources);
    if (relisted + retired >= before.sources) return;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error(
    `only some of ${before.sources} retained WatchHub source(s) relisted after the fixture reset`
  );
}
