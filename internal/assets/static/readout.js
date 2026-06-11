"use strict";
(() => {
  // internal/assets/src/js/theme.ts
  var PREFERS_DARK = window.matchMedia("(prefers-color-scheme: dark)");
  function syncThemeTogglePostTarget() {
    const toggle = document.getElementById("btn-theme-toggle");
    if (!toggle) {
      return;
    }
    if (toggle.dataset.themeExplicit !== "false") {
      return;
    }
    const form = toggle.form;
    const input = form && form.querySelector('input[name="theme"]');
    if (input) {
      input.value = PREFERS_DARK.matches ? "light" : "dark";
    }
  }
  PREFERS_DARK.addEventListener("change", syncThemeTogglePostTarget);

  // internal/assets/src/js/prefs.ts
  var PREFS_COOKIE = "ro_prefs";
  var PREFS_VERSION_PREFIX = "v1.";
  var PREFS_MAX_ENCODED = 3072;
  var PREFS_COOKIE_MAX_AGE = 31536e3;
  var REFRESH_KEY = "roRefresh";
  function b64urlEncodeUTF8(text) {
    const bytes = new TextEncoder().encode(text);
    let bin = "";
    for (let i = 0; i < bytes.length; i++) {
      bin += String.fromCharCode(bytes[i]);
    }
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
  function b64urlDecodeUTF8(encoded) {
    const b64 = encoded.replace(/-/g, "+").replace(/_/g, "/");
    const bin = atob(b64 + "====".slice(b64.length % 4 || 4));
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) {
      bytes[i] = bin.charCodeAt(i);
    }
    return new TextDecoder().decode(bytes);
  }
  function decodePrefsValue(value) {
    const empty = { kinds: [], refresh: "", ns: {} };
    if (!value || value.indexOf(PREFS_VERSION_PREFIX) !== 0) {
      return { prefs: empty, ok: false };
    }
    const payload = value.slice(PREFS_VERSION_PREFIX.length);
    if (!payload) {
      return { prefs: empty, ok: false };
    }
    try {
      const decoded = JSON.parse(b64urlDecodeUTF8(payload));
      if (!decoded || typeof decoded !== "object") {
        return { prefs: empty, ok: false };
      }
      const kinds = [];
      if (Array.isArray(decoded.kinds)) {
        decoded.kinds.forEach((e) => {
          if (!e || typeof e !== "object" || typeof e.k !== "string") {
            return;
          }
          const entry = { k: e.k };
          if (typeof e.sort === "string") {
            entry.sort = e.sort;
          }
          if (Array.isArray(e.hide) && e.hide.every((name) => typeof name === "string")) {
            entry.hide = e.hide;
          }
          kinds.push(entry);
        });
      }
      const ns = {};
      if (decoded.ns && typeof decoded.ns === "object" && !Array.isArray(decoded.ns)) {
        Object.keys(decoded.ns).forEach((cluster) => {
          if (typeof decoded.ns[cluster] === "string") {
            ns[cluster] = decoded.ns[cluster];
          }
        });
      }
      return {
        prefs: {
          kinds,
          refresh: typeof decoded.refresh === "string" ? decoded.refresh : "",
          ns
        },
        ok: true
      };
    } catch (e) {
      return { prefs: empty, ok: false };
    }
  }
  function encodePrefsValue(prefs) {
    const out = {};
    if (prefs.kinds && prefs.kinds.length > 0) {
      out.kinds = prefs.kinds;
    }
    if (prefs.refresh) {
      out.refresh = prefs.refresh;
    }
    if (prefs.ns && Object.keys(prefs.ns).length > 0) {
      out.ns = prefs.ns;
    }
    let value = PREFS_VERSION_PREFIX + b64urlEncodeUTF8(JSON.stringify(out));
    while (value.length > PREFS_MAX_ENCODED && out.kinds && out.kinds.length > 0) {
      out.kinds = out.kinds.slice(0, -1);
      if (out.kinds.length === 0) {
        delete out.kinds;
      }
      value = PREFS_VERSION_PREFIX + b64urlEncodeUTF8(JSON.stringify(out));
    }
    return value;
  }
  function prefsCookieValue() {
    const parts = document.cookie ? document.cookie.split("; ") : [];
    for (let i = 0; i < parts.length; i++) {
      if (parts[i].indexOf(PREFS_COOKIE + "=") === 0) {
        return parts[i].slice(PREFS_COOKIE.length + 1);
      }
    }
    return "";
  }
  function readPrefs() {
    return decodePrefsValue(prefsCookieValue()).prefs;
  }
  function writePrefs(prefs) {
    try {
      let cookie = PREFS_COOKIE + "=" + encodePrefsValue(prefs) + "; Path=/; SameSite=Lax; Max-Age=" + PREFS_COOKIE_MAX_AGE;
      if (window.location.protocol === "https:") {
        cookie += "; Secure";
      }
      document.cookie = cookie;
    } catch (e) {
    }
  }
  function prefsTouchKind(prefs, plural) {
    for (let i = 0; i < prefs.kinds.length; i++) {
      if (prefs.kinds[i].k === plural) {
        const entry = prefs.kinds.splice(i, 1)[0];
        prefs.kinds.unshift(entry);
        return entry;
      }
    }
    const fresh = { k: plural };
    prefs.kinds.unshift(fresh);
    return fresh;
  }
  function roPrefsSetSort(plural, sort) {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).sort = sort;
    writePrefs(prefs);
  }
  function roPrefsSetHiddenColumns(plural, names) {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).hide = Array.isArray(names) ? names : [];
    writePrefs(prefs);
  }
  function roPrefsSetRefresh(mode) {
    const prefs = readPrefs();
    prefs.refresh = mode;
    writePrefs(prefs);
  }
  function roPrefsSetNamespace(cluster, namespace) {
    if (!cluster || !namespace) {
      return;
    }
    const prefs = readPrefs();
    prefs.ns[cluster] = namespace;
    writePrefs(prefs);
  }

  // internal/assets/src/js/events.ts
  function closestElement(event, selector) {
    let node = event.target;
    while (node && node.nodeType !== 1) {
      node = node.parentNode;
    }
    return node ? node.closest(selector) : null;
  }
  function dispatch(bindings2, event) {
    for (let i = 0; i < bindings2.length; i++) {
      const binding = bindings2[i];
      let matched = null;
      if (binding.selector !== void 0) {
        matched = closestElement(event, binding.selector);
        if (!matched) {
          continue;
        }
      }
      let result;
      try {
        result = binding.handler(event, matched);
      } catch (e) {
        console.warn("readout event binding failed", binding.event, binding.selector, e);
        continue;
      }
      if (binding.stop && result) {
        return;
      }
    }
  }
  function registerBindings(bindings2) {
    const byType = /* @__PURE__ */ new Map();
    for (const binding of bindings2) {
      const list = byType.get(binding.event);
      if (list) {
        list.push(binding);
      } else {
        byType.set(binding.event, [binding]);
      }
    }
    byType.forEach((list, type) => {
      document.addEventListener(type, (event) => dispatch(list, event));
    });
  }

  // internal/assets/src/js/row-selection.ts
  var rowSelection = /* @__PURE__ */ new Map();
  var rowFocusKey = null;
  function reapplyRowState() {
    const content = document.getElementById("resource-list-content");
    if (!content) {
      return;
    }
    let focusedRow = null;
    content.querySelectorAll("tr[data-key]").forEach((tr) => {
      const row = tr;
      row.classList.toggle("is-selected", rowSelection.has(row.dataset.key));
      const focused = row.dataset.key === rowFocusKey;
      row.classList.toggle("kfocus", focused);
      if (focused) {
        focusedRow = row;
      }
    });
    content.querySelectorAll(".ro-table-wrap").forEach((wrap) => {
      const fr = focusedRow;
      if (fr && fr.id && wrap.contains(fr)) {
        wrap.setAttribute("aria-activedescendant", fr.id);
      } else {
        wrap.removeAttribute("aria-activedescendant");
      }
    });
  }
  function lastKeySegment(key) {
    const parts = (key || "").split("/");
    return parts[parts.length - 1] || "";
  }
  function rowSelectionEntry(key) {
    const content = document.getElementById("resource-list-content");
    let entry = null;
    if (content) {
      content.querySelectorAll("tr[data-key]").forEach((tr) => {
        const row = tr;
        if (row.dataset.key === key) {
          entry = { name: row.dataset.name || lastKeySegment(key) };
        }
      });
    }
    return entry || { name: lastKeySegment(key) };
  }
  function setRowSelected(key, on) {
    if (on) {
      rowSelection.set(key, rowSelectionEntry(key));
    } else {
      rowSelection.delete(key);
    }
    reapplyRowState();
    updateBulkBar();
  }
  function clearRowState() {
    rowSelection.clear();
    rowFocusKey = null;
    reapplyRowState();
    updateBulkBar();
  }
  window.roRowState = {
    setSelected: setRowSelected,
    setFocus(key) {
      rowFocusKey = key || null;
      reapplyRowState();
    },
    // focusedKey is the j/k focus seam the windowed walker (virtualizeMoveFocus,
    // still in legacy.js) reads across the module boundary -- the focused row can
    // be detached off-window, so the store (not the DOM kfocus class) is the
    // truth there. Also a debug sim the console can poll.
    focusedKey() {
      return rowFocusKey;
    },
    clear: clearRowState,
    selectedKeys() {
      return Array.from(rowSelection.keys());
    },
    // selectedEntries feeds the bulk actions: Copy names reads .name, and the
    // bulk Download-YAML builds its names list from .key/.name.
    selectedEntries() {
      return Array.from(rowSelection, ([key, entry]) => ({ key, name: entry.name }));
    }
  };
  var BULK_NAMES_MAX = 100;
  var bulkOverCapToasted = false;
  function updateBulkBar() {
    const bar = document.getElementById("ro-bulkbar");
    if (!bar) {
      return;
    }
    const count = rowSelection.size;
    const label = document.getElementById("ro-bulk-count");
    if (label && count > 0) {
      label.textContent = count + " selected";
    }
    bar.classList.toggle("is-open", count > 0);
    bar.toggleAttribute("inert", count === 0);
    const download = document.getElementById("ro-bulk-download");
    if (download && bar.dataset.bulkHref) {
      const over = count > BULK_NAMES_MAX;
      download.disabled = over;
      download.title = over ? "Over the " + BULK_NAMES_MAX + "-object bulk download cap" : "";
      if (over && !bulkOverCapToasted) {
        roToast("Download refused: " + count + " selected (max " + BULK_NAMES_MAX + ")");
      }
      bulkOverCapToasted = over;
    }
  }
  function roToast(message) {
    const fn = window.roToast;
    if (typeof fn === "function") {
      fn(message);
    }
  }
  function roCopyText(text, done) {
    const fallback = () => {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.top = "-1000px";
      document.body.appendChild(ta);
      ta.select();
      let ok = false;
      try {
        ok = document.execCommand("copy");
      } catch {
        ok = false;
      }
      ta.remove();
      return ok;
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(() => done(true), () => done(fallback()));
      return;
    }
    done(fallback());
  }
  function toggleRowSelection(tr) {
    const key = tr.dataset.key;
    if (!key) {
      return;
    }
    if (rowSelection.has(key)) {
      rowSelection.delete(key);
    } else {
      rowSelection.set(key, { name: tr.dataset.name || lastKeySegment(key) });
    }
    reapplyRowState();
    updateBulkBar();
  }
  var rowSelectionBindings = [
    {
      event: "click",
      selector: "#resource-list-content tr[data-key]",
      handler: (event, matched) => {
        const target = event.target;
        if (target && target.closest("a, button, input, select, textarea, label")) {
          return;
        }
        toggleRowSelection(matched);
      }
    }
  ];

  // internal/assets/src/js/context-menu.ts
  var CTX_CLAMP_W = 220;
  var CTX_CLAMP_H = 240;
  function closeRowMenu() {
    const menu = document.getElementById("ro-ctxmenu");
    if (menu) {
      menu.classList.remove("is-open");
      menu.setAttribute("aria-hidden", "true");
    }
  }
  function openRowMenu(tr, x, y) {
    const menu = document.getElementById("ro-ctxmenu");
    if (!menu) {
      return;
    }
    const bind = (action, href) => {
      const item = menu.querySelector('[data-ctx="' + action + '"]');
      if (!item) {
        return;
      }
      if (href) {
        item.dataset.href = href;
        item.hidden = false;
      } else {
        delete item.dataset.href;
        item.hidden = true;
      }
    };
    bind("open", tr.dataset.href || "");
    bind("yaml", tr.dataset.yaml || "");
    bind("logs", tr.dataset.logs || "");
    bind("download", tr.dataset.download || "");
    menu.dataset.name = tr.dataset.name || lastKeySegment(tr.dataset.key || "");
    menu.style.left = Math.max(8, Math.min(x, window.innerWidth - CTX_CLAMP_W)) + "px";
    menu.style.top = Math.max(8, Math.min(y, window.innerHeight - CTX_CLAMP_H)) + "px";
    menu.classList.add("is-open");
    menu.setAttribute("aria-hidden", "false");
  }
  var contextMenuBindings = [
    // Right-click on an identity row opens the menu; anywhere else closes ours
    // and yields to the native menu.
    {
      event: "contextmenu",
      handler: (event) => {
        const target = event.target;
        const tr = target ? target.closest("#resource-list-content tr[data-key]") : null;
        if (!tr) {
          closeRowMenu();
          return;
        }
        event.preventDefault();
        const me = event;
        openRowMenu(tr, me.clientX, me.clientY);
      }
    },
    // C2 step 1: a context-menu item -> act, then close. Copy stays on the page;
    // the navigation items go through location.assign with the bound data-href.
    // Download YAML is a Content-Disposition attachment, so assigning it
    // downloads WITHOUT leaving the page. Returned in the monolith -> stop:true.
    {
      event: "click",
      selector: "#ro-ctxmenu [data-ctx]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const item = matched;
        const menu = item.closest("#ro-ctxmenu");
        const name = menu && menu.dataset.name || "";
        const href = item.dataset.href || "";
        closeRowMenu();
        if (item.dataset.ctx === "copy") {
          roCopyText(name, () => {
          });
        } else if (href) {
          window.location.assign(href);
        }
        return true;
      }
    },
    // C2 step 2: ANY other click dismisses an open menu. UNCONDITIONAL and
    // NON-stopping -- the click then FALLS THROUGH to the bulk + row-select
    // bindings (bulk-actions.ts / row-selection.ts), so a click that lands on a
    // row both dismisses the menu AND toggles selection (compound case 1). No
    // selector (it runs on every click, like the monolith's step 2); closeRowMenu
    // on a closed menu is a no-op. NO stop: a stop here would silently drop the
    // selection while still passing a "menu closed" check.
    {
      event: "click",
      handler: () => {
        closeRowMenu();
      }
    },
    // K2: Esc closes the context menu. Its own keydown branch (NO preventDefault),
    // idempotent (closeRowMenu on a closed menu is a no-op).
    {
      event: "keydown",
      handler: (event) => {
        if (event.key === "Escape") {
          closeRowMenu();
        }
      }
    }
  ];

  // internal/assets/src/js/bulk-actions.ts
  var bulkCopyResetTimer = 0;
  function bulkCopyNames(button) {
    const entries = roRowState().selectedEntries();
    const names = entries.map((entry) => entry.name).join("\n");
    roCopyText(names, (ok) => {
      if (!ok) {
        return;
      }
      const label = button.querySelector("span:last-child");
      if (!label) {
        return;
      }
      window.clearTimeout(bulkCopyResetTimer);
      label.textContent = "Copied";
      bulkCopyResetTimer = window.setTimeout(() => {
        label.textContent = "Copy names";
      }, 1100);
    });
  }
  function bulkDownloadYAML(bar) {
    if (!bar || !bar.dataset.bulkHref) {
      return;
    }
    const entries = roRowState().selectedEntries();
    if (entries.length === 0 || entries.length > BULK_NAMES_MAX) {
      return;
    }
    const clusterPrefix = (bar.dataset.bulkCluster || "") + "/";
    const names = entries.map((entry) => {
      if (bar.dataset.bulkAllns === "true" && entry.key.indexOf(clusterPrefix) === 0) {
        return entry.key.slice(clusterPrefix.length);
      }
      return entry.name;
    });
    window.location.assign(bar.dataset.bulkHref + "&names=" + encodeURIComponent(names.join(",")));
  }
  function roRowState() {
    return window.roRowState;
  }
  var bulkBindings = [
    {
      event: "click",
      selector: "#ro-bulk-download",
      stop: true,
      handler: (_event, matched) => {
        bulkDownloadYAML(matched.closest("#ro-bulkbar"));
        return true;
      }
    },
    {
      event: "click",
      selector: "#ro-bulk-copy",
      stop: true,
      handler: (_event, matched) => {
        bulkCopyNames(matched);
        return true;
      }
    },
    {
      event: "click",
      selector: "#ro-bulk-clear",
      stop: true,
      handler: () => {
        clearRowState();
        return true;
      }
    }
  ];

  // internal/assets/src/js/palette-rank.ts
  function roFuzzyScore(query, text) {
    const source = String(text || "");
    const q = String(query || "").toLowerCase();
    const t = source.toLowerCase();
    if (!q) {
      return 0;
    }
    let from = 0;
    let first = -1;
    let last = -1;
    for (let i = 0; i < q.length; i++) {
      const at = t.indexOf(q[i], from);
      if (at === -1) {
        return -1;
      }
      if (first === -1) {
        first = at;
      }
      last = at;
      from = at + 1;
    }
    const gaps = last - first + 1 - q.length;
    const camelHump = source[first] >= "A" && source[first] <= "Z" && !(source[first - 1] >= "A" && source[first - 1] <= "Z");
    const wordStart = first === 0 || " -_./:".indexOf(t[first - 1]) !== -1 || camelHump;
    let tier = 2;
    if (gaps === 0 && first === 0) {
      tier = 0;
    } else if (gaps === 0 && wordStart) {
      tier = 1;
    }
    return tier * 1e5 + gaps * 100 + Math.min(first, 99);
  }
  function rankPaletteEntries(list, query, labelOf) {
    if (!query) {
      return list.slice();
    }
    const scored = [];
    list.forEach((entry) => {
      const score = roFuzzyScore(query, labelOf(entry));
      if (score >= 0) {
        scored.push({ entry, score });
      }
    });
    scored.sort((a, b) => a.score - b.score);
    return scored.map((it) => it.entry);
  }
  function paletteRecentTarget(entry) {
    return entry.href ? "href:" + entry.href : "action:" + entry.action;
  }
  function dedupeRecents(prior, entry, max) {
    const kept = prior.filter(
      (it) => paletteRecentTarget(it) !== paletteRecentTarget(entry)
    );
    kept.unshift(entry);
    return kept.slice(0, max);
  }
  var FEED_GROUPS = [
    { title: "Resource types", key: "kinds" },
    { title: "Namespaces", key: "namespaces" },
    { title: "Clusters", key: "clusters" },
    { title: "Actions", key: "actions" }
  ];
  function feedEntryLabel(entry, key) {
    if (key === "kinds") {
      return String(entry.kind || entry.plural || "");
    }
    return String(entry.name || entry.label || "");
  }
  function buildPaletteGroups(query, feed, recents, pageObjects) {
    const q = (query || "").trim();
    const groups = [];
    if (q) {
      groups.push({ title: "Everywhere", key: "everywhere", entries: [{ query: q }] });
    } else if (recents.length > 0) {
      groups.push({ title: "Recents", key: "recents", entries: recents.slice() });
    }
    const objects = rankPaletteEntries(pageObjects, q, (o) => o.name);
    if (objects.length > 0) {
      groups.push({ title: "On this page", key: "objects", entries: objects });
    }
    FEED_GROUPS.forEach((group) => {
      const list = feed[group.key] || [];
      const ranked = rankPaletteEntries(list, q, (entry) => feedEntryLabel(entry, group.key));
      if (ranked.length > 0) {
        groups.push({ title: group.title, key: group.key, entries: ranked });
      }
    });
    return groups;
  }

  // internal/assets/src/js/cluster-bridge.ts
  function clusterBridge() {
    return window.roClusterBridge;
  }

  // internal/assets/src/js/keyboard.ts
  var PALETTE_ID = "ro-palette";
  function roRowState2() {
    return window.roRowState;
  }
  function keyboardTargetIsTextEntry(target) {
    const el = target;
    if (!el || el.nodeType !== 1) {
      return false;
    }
    const tag = el.tagName;
    return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || !!el.isContentEditable;
  }
  function keyboardSurfaceBusy() {
    const palette = document.getElementById(PALETTE_ID);
    if (palette && palette.classList.contains("open")) {
      return true;
    }
    const menu = document.getElementById("ro-ctxmenu");
    if (menu && menu.classList.contains("is-open")) {
      return true;
    }
    const nsDropdown = document.getElementById("namespace-dropdown");
    if (nsDropdown && nsDropdown.classList.contains("is-active")) {
      return true;
    }
    return clusterBridge().colsPopOpen();
  }
  function visibleKeyRows() {
    return Array.from(
      document.querySelectorAll("#resource-list-content tbody tr[data-key]")
    ).filter((tr) => !tr.classList.contains("ro-row-filtered"));
  }
  function moveRowFocus(delta) {
    const bridge = clusterBridge();
    if (bridge.virtualizerActive()) {
      return bridge.virtMoveFocus(delta);
    }
    const rows = visibleKeyRows();
    if (rows.length === 0) {
      return false;
    }
    const focusKey = roRowState2().focusedKey();
    const current = rows.findIndex((tr) => tr.dataset.key === focusKey);
    const next = Math.max(0, Math.min(rows.length - 1, current + delta));
    roRowState2().setFocus(rows[next].dataset.key);
    rows[next].scrollIntoView({ block: "nearest" });
    return true;
  }
  function openFocusedRow() {
    const key = roRowState2().focusedKey();
    if (!key) {
      return false;
    }
    const bridge = clusterBridge();
    let row = visibleKeyRows().find((tr) => tr.dataset.key === key) || null;
    if (!row && bridge.virtualizerActive()) {
      const tr = bridge.virtRowByKey(key);
      if (tr && bridge.virtVisible().indexOf(tr) !== -1) {
        row = tr;
      }
    }
    if (!row || !row.dataset.href) {
      return false;
    }
    window.location.assign(row.dataset.href);
    return true;
  }
  var kbdPriorFocus = null;
  function kbdOverlayEl() {
    return document.getElementById("ro-kbd-overlay");
  }
  function kbdOverlayOpen() {
    const overlay = kbdOverlayEl();
    return !!overlay && overlay.classList.contains("open");
  }
  function openKbdOverlay() {
    const overlay = kbdOverlayEl();
    if (!overlay) {
      return;
    }
    kbdPriorFocus = document.activeElement;
    overlay.classList.add("open");
    overlay.setAttribute("aria-hidden", "false");
    const card = overlay.querySelector(".kbd-card");
    if (card) {
      card.focus();
    }
  }
  function closeKbdOverlay() {
    const overlay = kbdOverlayEl();
    if (!overlay) {
      return;
    }
    overlay.classList.remove("open");
    overlay.setAttribute("aria-hidden", "true");
    const prior = kbdPriorFocus;
    if (prior && document.contains(prior) && typeof prior.focus === "function") {
      prior.focus();
    }
    kbdPriorFocus = null;
  }
  var keyboardBindings = [
    // C3: a click on the overlay backdrop ITSELF (outside the card) closes it --
    // the palette's backdrop contract. Independent.
    {
      event: "click",
      handler: (event) => {
        if (event.target.id === "ro-kbd-overlay") {
          closeKbdOverlay();
        }
      }
    },
    // K3: THE gesture keydown. The DOM guards (kbd overlay open, modifier chord,
    // text-entry, surface-busy) keep it disjoint from the palette/filter keys --
    // registration after the palette keydown is incidental; the busy guard does
    // the real work (compound case 2). No selector (it keys off focus/state).
    {
      event: "keydown",
      handler: (event) => {
        const e = event;
        if (kbdOverlayOpen()) {
          if (e.key === "Escape" || e.key === "?") {
            e.preventDefault();
            closeKbdOverlay();
          } else if (e.key === "Tab") {
            e.preventDefault();
          }
          return;
        }
        if (e.metaKey || e.ctrlKey || e.altKey) {
          return;
        }
        if (keyboardTargetIsTextEntry(e.target) || keyboardSurfaceBusy()) {
          return;
        }
        if (e.key === "?") {
          e.preventDefault();
          openKbdOverlay();
          return;
        }
        if (e.key === "j" || e.key === "k") {
          if (moveRowFocus(e.key === "j" ? 1 : -1)) {
            e.preventDefault();
          }
          return;
        }
        if (e.key === "Enter") {
          const target = e.target;
          if (target && target.closest && target.closest("a, button, summary")) {
            return;
          }
          if (openFocusedRow()) {
            e.preventDefault();
          }
        }
      }
    }
  ];

  // internal/assets/src/js/palette.ts
  var PALETTE_ID2 = "ro-palette";
  window.roFuzzy = roFuzzyScore;
  function readPaletteData() {
    const empty = {
      currentCluster: null,
      currentNamespace: null,
      clusters: [],
      namespaces: [],
      kinds: [],
      actions: []
    };
    const el = document.getElementById("ro-palette-data");
    if (!el) {
      return empty;
    }
    const raw = (el.textContent || "").trim();
    if (!raw) {
      return empty;
    }
    try {
      const data = JSON.parse(raw);
      if (!data || typeof data !== "object") {
        return empty;
      }
      ["clusters", "namespaces", "kinds", "actions"].forEach((k) => {
        if (!Array.isArray(data[k])) {
          data[k] = [];
        }
      });
      return data;
    } catch {
      return empty;
    }
  }
  function paletteHrefSafe(href) {
    if (!href || typeof href !== "string") {
      return "";
    }
    const trimmed = href.trim();
    if (/^[a-z][a-z0-9+.-]*:/i.test(trimmed) && !/^https?:/i.test(trimmed)) {
      return "";
    }
    return trimmed;
  }
  var PALETTE_RECENTS_KEY = "ro-pref-recents";
  var PALETTE_RECENTS_MAX = 5;
  function readPaletteRecents() {
    let raw = null;
    try {
      raw = window.localStorage.getItem(PALETTE_RECENTS_KEY);
    } catch {
      return [];
    }
    if (!raw) {
      return [];
    }
    try {
      const list = JSON.parse(raw);
      if (!Array.isArray(list)) {
        return [];
      }
      return list.filter((entry) => entry && typeof entry === "object" && typeof entry.label === "string" && entry.label !== "" && (typeof entry.href === "string" && paletteHrefSafe(entry.href) !== "" || typeof entry.action === "string" && entry.action !== "")).slice(0, PALETTE_RECENTS_MAX);
    } catch {
      return [];
    }
  }
  function recordPaletteRecent(label, href, action) {
    if (!label || !href && !action) {
      return;
    }
    const entry = { label };
    if (href) {
      entry.href = href;
    }
    if (action) {
      entry.action = action;
    }
    const kept = dedupeRecents(readPaletteRecents(), entry, PALETTE_RECENTS_MAX);
    try {
      window.localStorage.setItem(PALETTE_RECENTS_KEY, JSON.stringify(kept));
    } catch {
    }
  }
  var paletteRows = [];
  var paletteActive = 0;
  var paletteScope = {
    cluster: null,
    namespace: null
  };
  function buildPaletteRow(entry, key) {
    const row = document.createElement("div");
    row.className = "ro-pal-item";
    row.setAttribute("role", "option");
    row.setAttribute("aria-selected", "false");
    if (key === "kinds" && entry.icon) {
      const holder = document.createElement("template");
      holder.innerHTML = String(entry.icon);
      row.appendChild(holder.content);
    }
    const labelText = key === "kinds" ? String(entry.kind || entry.plural || "") : String(entry.name || entry.label || "");
    const display = typeof entry.display === "string" && entry.display !== "" ? entry.display : labelText;
    const label = document.createElement("span");
    label.className = "pal-label";
    label.textContent = display;
    if (display !== labelText) {
      row.title = labelText;
    }
    const isCurrent = key === "clusters" && entry.name && entry.name === paletteScope.cluster || key === "namespaces" && entry.name && entry.name === paletteScope.namespace;
    if (isCurrent) {
      const ctx = document.createElement("span");
      ctx.className = "pal-ctx";
      ctx.textContent = "current";
      label.appendChild(ctx);
    }
    row.appendChild(label);
    if (key === "kinds") {
      const meta = document.createElement("span");
      meta.className = "pal-meta";
      meta.textContent = String(entry.group || "core");
      row.appendChild(meta);
      const scope = document.createElement("span");
      scope.className = "pal-scope " + (entry.namespaced ? "ns" : "cluster");
      scope.textContent = entry.namespaced ? "namespaced" : "cluster";
      row.appendChild(scope);
    }
    const href = paletteHrefSafe(entry.href);
    if (href) {
      row.dataset.href = href;
    }
    if (entry.action) {
      row.dataset.action = String(entry.action);
    }
    row.dataset.label = labelText;
    return row;
  }
  function buildEverywhereRow(query) {
    const row = document.createElement("div");
    row.className = "ro-pal-item";
    row.setAttribute("role", "option");
    row.setAttribute("aria-selected", "false");
    const glyph = document.querySelector("#" + PALETTE_ID2 + " .ro-pal-search .ico");
    if (glyph) {
      row.appendChild(glyph.cloneNode(true));
    }
    const label = document.createElement("span");
    label.className = "pal-label";
    label.textContent = "Search all clusters for “" + query + "”";
    row.appendChild(label);
    row.dataset.href = "/search?q=" + encodeURIComponent(query);
    row.dataset.label = label.textContent;
    return row;
  }
  function buildRecentRow(entry) {
    const row = document.createElement("div");
    row.className = "ro-pal-item";
    row.setAttribute("role", "option");
    row.setAttribute("aria-selected", "false");
    const label = document.createElement("span");
    label.className = "pal-label";
    label.textContent = entry.label;
    row.appendChild(label);
    const href = paletteHrefSafe(entry.href);
    if (href) {
      row.dataset.href = href;
    }
    if (entry.action) {
      row.dataset.action = entry.action;
    }
    row.dataset.label = entry.label;
    return row;
  }
  function harvestPageObjects() {
    const out = [];
    const bridge = clusterBridge();
    const rows = bridge.virtualizerActive() ? bridge.virtRows() : document.querySelectorAll("#resource-list-content table.ro-table tbody tr");
    Array.prototype.forEach.call(rows, (tr) => {
      const a = tr.querySelector("td.cell-name a");
      if (!a) {
        return;
      }
      const href = a.getAttribute("href");
      const name = (a.textContent || "").trim();
      if (!href || !name) {
        return;
      }
      let status = "";
      let tone = "";
      const st = tr.querySelector(".cell-status");
      if (st) {
        status = (st.textContent || "").trim();
        ["ok", "warn", "err", "info", "mute"].forEach((t) => {
          if (!tone && st.classList.contains(t)) {
            tone = t;
          }
        });
      }
      out.push({ name, href, status, tone });
    });
    return out;
  }
  function buildObjectRow(o) {
    const row = document.createElement("div");
    row.className = "ro-pal-item";
    row.setAttribute("role", "option");
    row.setAttribute("aria-selected", "false");
    const label = document.createElement("span");
    label.className = "pal-label";
    label.textContent = o.name;
    row.appendChild(label);
    if (o.status) {
      const st = document.createElement("span");
      st.className = "pal-status" + (o.tone ? " " + String(o.tone) : "");
      st.textContent = String(o.status);
      row.appendChild(st);
    }
    row.dataset.href = String(o.href);
    row.dataset.label = o.name;
    return row;
  }
  function renderPalette(query) {
    const list = document.getElementById("ro-palette-list");
    if (!list) {
      return;
    }
    const data = readPaletteData();
    paletteScope.cluster = data.currentCluster || null;
    paletteScope.namespace = data.currentNamespace || null;
    const scope = document.getElementById("ro-palette-scope");
    if (scope) {
      const scopeText = paletteScope.namespace || paletteScope.cluster || "";
      scope.textContent = scopeText;
      scope.hidden = scopeText === "";
    }
    const q = (query || "").trim();
    list.textContent = "";
    paletteRows = [];
    const rowFor = (item, key) => {
      switch (key) {
        case "everywhere":
          return buildEverywhereRow(item.query);
        case "recents":
          return buildRecentRow(item);
        case "objects":
          return buildObjectRow(item);
        default:
          return buildPaletteRow(item, key);
      }
    };
    const groups = buildPaletteGroups(
      q,
      { clusters: data.clusters, namespaces: data.namespaces, kinds: data.kinds, actions: data.actions },
      readPaletteRecents(),
      harvestPageObjects()
    );
    groups.forEach((group) => {
      const heading = document.createElement("div");
      heading.className = "ro-pal-group";
      heading.textContent = group.title;
      list.appendChild(heading);
      group.entries.forEach((item) => {
        const row = rowFor(item, group.key);
        const idx = paletteRows.length;
        row.addEventListener("mousemove", () => setPaletteActive(idx));
        list.appendChild(row);
        paletteRows.push({ el: row, item, key: group.key });
      });
    });
    if (paletteRows.length === 0) {
      const none = document.createElement("div");
      none.className = "ro-pal-empty";
      none.textContent = "No matching targets.";
      list.appendChild(none);
    }
    paletteActive = 0;
    paintPaletteActive();
  }
  function paintPaletteActive() {
    paletteRows.forEach((r, i) => {
      const on = i === paletteActive;
      r.el.classList.toggle("active", on);
      r.el.setAttribute("aria-selected", on ? "true" : "false");
    });
    if (paletteRows[paletteActive]) {
      paletteRows[paletteActive].el.scrollIntoView({ block: "nearest" });
    }
  }
  function setPaletteActive(index) {
    if (paletteRows.length === 0) {
      return;
    }
    let i = index;
    if (i < 0) {
      i = 0;
    }
    if (i > paletteRows.length - 1) {
      i = paletteRows.length - 1;
    }
    paletteActive = i;
    paintPaletteActive();
  }
  function movePaletteActive(delta) {
    if (paletteRows.length === 0) {
      return;
    }
    paletteActive = (paletteActive + delta + paletteRows.length) % paletteRows.length;
    paintPaletteActive();
  }
  function choosePaletteRow(rowEl) {
    if (!rowEl) {
      return;
    }
    const action = rowEl.dataset.action;
    const href = rowEl.dataset.href;
    recordPaletteRecent(rowEl.dataset.label || "", href || "", action || "");
    closePalette();
    if (action === "theme") {
      const toggle = document.getElementById("btn-theme-toggle");
      if (toggle) {
        toggle.click();
      }
      return;
    }
    if (href) {
      window.location.assign(href);
    }
  }
  function activatePaletteSelection() {
    const active = paletteRows[paletteActive];
    if (active) {
      choosePaletteRow(active.el);
    }
  }
  var palettePriorFocus = null;
  var paletteRestoringFocus = false;
  function openPalette(prefill) {
    const palette = document.getElementById(PALETTE_ID2);
    const input = document.getElementById("ro-palette-input");
    if (!palette || !input) {
      return;
    }
    if (!palette.classList.contains("open")) {
      palettePriorFocus = document.activeElement;
    }
    palette.classList.add("open");
    palette.setAttribute("aria-hidden", "false");
    input.value = typeof prefill === "string" ? prefill : "";
    renderPalette(input.value);
    input.focus();
  }
  window.roOpenPalette = openPalette;
  function closePalette() {
    const palette = document.getElementById(PALETTE_ID2);
    if (!palette) {
      return;
    }
    palette.classList.remove("open");
    palette.setAttribute("aria-hidden", "true");
    if (palettePriorFocus && document.contains(palettePriorFocus) && !palette.contains(palettePriorFocus) && typeof palettePriorFocus.focus === "function") {
      paletteRestoringFocus = true;
      palettePriorFocus.focus();
      paletteRestoringFocus = false;
    }
    palettePriorFocus = null;
  }
  var paletteBindings = [
    // ⌘K palette result row: a click on a result row activates it (navigate or
    // run its named action, then close). FIRST so a click inside the open
    // palette never falls through to a page handler. (C1 head, returned.)
    {
      event: "click",
      selector: ".ro-pal-item",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        choosePaletteRow(matched);
        return true;
      }
    },
    // The read-only topbar search box ([data-palette-open]) opens the palette on
    // click instead of typing inline. (C1, returned.)
    {
      event: "click",
      selector: "[data-palette-open]",
      stop: true,
      handler: (event) => {
        event.preventDefault();
        openPalette();
        return true;
      }
    },
    // The search page's "Refine · ⌘K" button (D12): open the palette PREFILLED
    // with the query the page searched (server-baked data-query). (C1, returned.)
    {
      event: "click",
      selector: "[data-search-refine]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        openPalette(matched.dataset.query || "");
        return true;
      }
    },
    // A click on the palette backdrop ITSELF (the dimmed area outside the panel)
    // closes it, like Esc. A click inside the panel does not match. The selector
    // is the backdrop root id; the handler still verifies target.id === PALETTE_ID
    // so a click that bubbles from a descendant (closest matched the root) does
    // NOT close it -- the monolith's exact `target.id === PALETTE_ID` test.
    {
      event: "click",
      selector: "#" + PALETTE_ID2,
      stop: true,
      handler: (event) => {
        if (event.target.id === PALETTE_ID2) {
          closePalette();
          return true;
        }
        return false;
      }
    },
    // ⌘K palette query box: re-render the grouped rows fuzzy-matched + ranked
    // against the label, re-seating the active row. (Monolith input head, returned.)
    {
      event: "input",
      selector: "#ro-palette-input",
      stop: true,
      handler: (_event, matched) => {
        renderPalette(matched.value);
        return true;
      }
    },
    // ⌘K / Ctrl+K chord opens the palette from anywhere (ignored with Alt/Shift,
    // so an unrelated OS/browser shortcut is never hijacked). The palette is
    // exclusive: an open "?" overlay or row menu closes FIRST so one Esc later
    // closes exactly one surface. No selector (it keys off the chord, not a
    // delegated target). Does NOT stop: the still-resident gesture keydown (K3)
    // returns on the modifier chord on its own, and the filter editor's keydown
    // is unaffected -- mirroring the monolith's separate listeners.
    {
      event: "keydown",
      handler: (event) => {
        const e = event;
        if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && (e.key === "k" || e.key === "K")) {
          e.preventDefault();
          closeKbdOverlay();
          closeRowMenu();
          openPalette();
        }
      }
    },
    // Palette-open keyboard model (Esc/Arrow/Enter/Tab). Acts ONLY while the
    // palette is open AND the target is not the filter editor: in the monolith
    // the filter-input keydown branch RETURNED before this palette branch, so an
    // Escape with focus in #ro-filter-input routed to the filter handler and
    // never reached closePalette (compound case 4). The still-resident filter
    // keydown listener keeps owning #ro-filter-input keys; this binding excludes
    // that target so the focus-routed Escape semantics are byte-identical. No
    // stop: the gesture keydown (K3) is kept inert by keyboardSurfaceBusy()
    // (palette `.open`), the real decoupler.
    {
      event: "keydown",
      handler: (event) => {
        const e = event;
        const target = e.target;
        if (target && target.id === "ro-filter-input") {
          return;
        }
        const palette = document.getElementById(PALETTE_ID2);
        if (!palette || !palette.classList.contains("open")) {
          return;
        }
        if (e.key === "Escape") {
          e.preventDefault();
          closePalette();
        } else if (e.key === "ArrowDown") {
          e.preventDefault();
          movePaletteActive(1);
        } else if (e.key === "ArrowUp") {
          e.preventDefault();
          movePaletteActive(-1);
        } else if (e.key === "Enter") {
          e.preventDefault();
          activatePaletteSelection();
        } else if (e.key === "Tab") {
          e.preventDefault();
          movePaletteActive(e.shiftKey ? -1 : 1);
        }
      }
    },
    // The topbar search box also opens the palette on keyboard FOCUS (Tab-into /
    // programmatic focus): focusin bubbles to document. openPalette runs FIRST
    // (while the box still holds focus) so it captures the box as the Esc restore
    // target; the blur after only matters when openPalette no-opped. The
    // paletteRestoringFocus gate keeps the close-restore from re-opening: focusing
    // the box FROM closePalette fires this very binding.
    {
      event: "focusin",
      selector: "[data-palette-open]",
      handler: (event) => {
        if (paletteRestoringFocus) {
          return;
        }
        openPalette();
        const t = event.target;
        if (typeof t.blur === "function") {
          t.blur();
        }
      }
    }
  ];

  // internal/assets/src/js/yaml-folds.ts
  function yamlEffectiveIndent(text) {
    const stripped = text.replace(/^\n+/, "");
    let i = 0;
    while (i < stripped.length && stripped[i] === " ") {
      i++;
    }
    const rest = stripped.slice(i);
    if (rest === "-" || rest.startsWith("- ") || rest.startsWith("-	")) {
      return i + 2;
    }
    return i;
  }
  function yamlCodeText(codeCell) {
    if (!codeCell.querySelector(".ro-fold-toggle, .ro-fold-note")) {
      return codeCell.textContent || "";
    }
    const clone = codeCell.cloneNode(true);
    clone.querySelectorAll(".ro-fold-toggle, .ro-fold-note").forEach((el) => {
      el.remove();
    });
    return clone.textContent || "";
  }
  function toggleYamlFold(toggle) {
    const id = toggle.dataset.fold;
    if (!id) {
      return;
    }
    const pre = toggle.closest("pre");
    if (!pre) {
      return;
    }
    const folded = !toggle.classList.contains("is-folded");
    toggle.classList.toggle("is-folded", folded);
    toggle.setAttribute("aria-expanded", folded ? "false" : "true");
    pre.querySelectorAll("[data-fold-of]").forEach((line) => {
      const owners = (line.dataset.foldOf || "").split(" ");
      if (owners.indexOf(id) !== -1) {
        line.classList.toggle("ro-line-folded", folded);
      }
    });
  }
  function injectFoldControls(lineSpan, bodyCount) {
    const toggle = document.createElement("button");
    toggle.type = "button";
    toggle.className = "ro-fold-toggle";
    toggle.setAttribute("aria-expanded", "true");
    toggle.setAttribute("aria-label", "Toggle block");
    toggle.dataset.fold = lineSpan.id;
    const note = document.createElement("span");
    note.className = "ro-fold-note";
    const lineWord = bodyCount === 1 ? "line" : "lines";
    note.textContent = ` … ${bodyCount} ${lineWord}`;
    const anchor = lineSpan.querySelector("a");
    if (anchor && anchor.nextSibling) {
      lineSpan.insertBefore(toggle, anchor.nextSibling);
    } else if (anchor) {
      lineSpan.appendChild(toggle);
    } else {
      lineSpan.insertBefore(toggle, lineSpan.firstChild);
    }
    const last = lineSpan.lastChild;
    if (last && last.nodeType === 3 && (last.textContent || "").indexOf("\n") !== -1) {
      lineSpan.insertBefore(note, last);
    } else {
      lineSpan.appendChild(note);
    }
  }
  function buildYamlFolds() {
    document.querySelectorAll(".highlighttable td.code pre").forEach((pre) => {
      if (pre.dataset.roFolds) {
        return;
      }
      try {
        const lines = Array.prototype.filter.call(
          pre.children,
          (el) => el.tagName === "SPAN" && el.id && el.id.indexOf("line-") !== -1
        );
        pre.dataset.roFolds = "1";
        if (lines.length < 3) {
          return;
        }
        const indents = lines.map((el) => yamlEffectiveIndent(el.textContent || ""));
        const isBlank = lines.map((el) => (el.textContent || "").trim() === "");
        for (let i = 0; i < lines.length; i++) {
          if (isBlank[i]) {
            continue;
          }
          let j = i + 1;
          while (j < lines.length && isBlank[j]) {
            j++;
          }
          if (j >= lines.length || indents[j] <= indents[i]) {
            continue;
          }
          let end = i + 1;
          let bodyCount = 0;
          while (end < lines.length) {
            if (isBlank[end]) {
              end++;
              continue;
            }
            if (indents[end] > indents[i]) {
              const cur = lines[end];
              cur.dataset.foldOf = cur.dataset.foldOf ? `${cur.dataset.foldOf} ${lines[i].id}` : lines[i].id;
              bodyCount++;
              end++;
            } else {
              break;
            }
          }
          if (bodyCount === 0) {
            continue;
          }
          injectFoldControls(lines[i], bodyCount);
        }
      } catch (e) {
      }
    });
  }
  function highlightYamlLine() {
    const fragment = location.hash;
    if (!fragment) {
      return;
    }
    document.querySelectorAll("pre > span.yaml-line-highlight").forEach((el) => {
      el.classList.remove("yaml-line-highlight");
    });
    const element = document.getElementById(`yaml-${fragment.substring(1)}`);
    if (element) {
      element.classList.add("yaml-line-highlight");
      element.scrollIntoView({ block: "center" });
    }
  }
  var foldBindings = [
    // .ro-fold-toggle (NESTED YAML block fold): toggle the deeper-indented child
    // lines of a `key:`/`- key:` block in place. Matched BEFORE the section-fold
    // + gutter-anchor handlers (registration order) so a nested-fold click never
    // collapses the whole section or jumps a line anchor. The monolith called
    // preventDefault + stopPropagation + return; we keep stopPropagation (inert
    // for document siblings per the inventory, but preserved 1:1) and stop:true
    // mirrors the early return.
    {
      event: "click",
      selector: ".ro-fold-toggle",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        event.stopPropagation();
        toggleYamlFold(matched);
        return true;
      }
    },
    // YAML line-number anchors (.linenos a): set the URL hash to the clicked
    // line, re-highlight, and suppress the default anchor jump. In the monolith
    // this branch sits AFTER the section-fold branch; here it shares the leaf
    // list and the section-fold handler (misc-ui) is registered separately. The
    // two never co-match (an anchor in the gutter is not a section title), so
    // relative order is immaterial -- but it keeps its own early-return.
    {
      event: "click",
      selector: ".linenos a",
      stop: true,
      handler: (event, matched) => {
        const anchor = matched;
        location.hash = `#${anchor.href.split("#")[1]}`;
        highlightYamlLine();
        event.preventDefault();
        return true;
      }
    }
  ];

  // internal/assets/src/js/logs.ts
  function logsScrollToTail() {
    const pre = document.querySelector("pre.ro-logpre");
    if (pre) {
      pre.scrollTop = pre.scrollHeight;
    }
  }
  function logsPinTailIfFollowing() {
    const follow = document.getElementById("logFollow");
    if (follow && !follow.classList.contains("quiet")) {
      logsScrollToTail();
    }
  }
  function initLogsFollow() {
    logsPinTailIfFollowing();
  }
  var logsBindings = [
    // Logs Follow toggle (D25): the active accent "Following" sticks the stream
    // to its tail; clicking flips to the quiet "Follow" (and back). Re-activating
    // snaps the stream to the tail immediately. Pure class + label flips -- no
    // request, the read-only floor is untouched. Kept its monolith early-return
    // (stop:true).
    {
      event: "click",
      selector: "#logFollow",
      stop: true,
      handler: (_event, matched) => {
        const logFollow = matched;
        const following = !logFollow.classList.toggle("quiet");
        logFollow.setAttribute("aria-pressed", following ? "true" : "false");
        const label = logFollow.querySelector(".follow-label");
        if (label) {
          label.textContent = following ? "Following" : "Follow";
        }
        if (following) {
          logsScrollToTail();
        }
        return true;
      }
    },
    // Logs display toggles (D25): CLIENT-SIDE only, no refetch. The timestamps
    // checkbox shows/hides the .log-ts spans via the stream's `hide-ts` class.
    // Both flips reflow the stream, so while Following is active the tail is
    // re-pinned afterwards. The monolith #logTs branch early-returned (stop:true).
    {
      event: "change",
      selector: "#logTs",
      stop: true,
      handler: (_event, matched) => {
        const logTs = matched;
        const pre = document.querySelector("pre.ro-logpre");
        if (pre) {
          pre.classList.toggle("hide-ts", !logTs.checked);
          logsPinTailIfFollowing();
        }
        return true;
      }
    },
    // The wrap checkbox toggles `wrap` (pre-wrap + break-word). In the monolith
    // this was the LAST change branch (no branch follows it), so stop:true is the
    // faithful mirror.
    {
      event: "change",
      selector: "#logWrap",
      stop: true,
      handler: (_event, matched) => {
        const logWrap = matched;
        const pre = document.querySelector("pre.ro-logpre");
        if (pre) {
          pre.classList.toggle("wrap", logWrap.checked);
          logsPinTailIfFollowing();
        }
        return true;
      }
    }
  ];

  // internal/assets/src/js/collapse-hash.ts
  function parseCollapsedNames(hash) {
    if (!hash) {
      return [];
    }
    const names = [];
    hash.replace(/^#/, "").split(";").forEach((param) => {
      const keyVal = param.split("=");
      if (keyVal[0] === "collapsed" && keyVal[1]) {
        keyVal[1].split(",").forEach((name) => {
          if (name) {
            names.push(name);
          }
        });
      }
    });
    return names;
  }

  // internal/assets/src/js/misc-ui.ts
  function collapseSectionsFromHash() {
    parseCollapsedNames(document.location.hash).forEach((name) => {
      document.querySelectorAll(`main .collapsible[data-name="${CSS.escape(name)}"]`).forEach((el) => {
        el.classList.add("is-collapsed");
      });
    });
  }
  var miscBindings = [
    // Mobile hamburger: a delegated click on `.menu-toggle` reveals/hides the
    // sidebar by toggling `.is-active` on `.ro-sidebar`. No-op when no sidebar is
    // present (e.g. the Clusters entry page). Kept its early-return (stop:true).
    {
      event: "click",
      selector: ".menu-toggle",
      stop: true,
      handler: (event) => {
        event.preventDefault();
        const sidebar = document.querySelector(".ro-sidebar");
        if (sidebar) {
          sidebar.classList.toggle("is-active");
        }
        return true;
      }
    },
    // .ro-copy-btn (per-section YAML copy): copy THIS section's raw YAML to the
    // clipboard via navigator.clipboard.writeText -- CSP-clean. The raw text is
    // read from the section's Pygments `td.code` cell (the gutter lives in a
    // separate `td.linenos`), with any injected fold controls stripped first
    // (yamlCodeText) so the copy is the full source YAML in any fold state. The
    // button briefly flips its label to "copied". Matched (and stop:true) BEFORE
    // the section-fold binding so a copy click never toggles the section fold.
    {
      event: "click",
      selector: ".ro-copy-btn",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const copyBtn = matched;
        const section = copyBtn.closest(".collapsible");
        const codeCell = section && section.querySelector(".highlighttable td.code");
        const text = codeCell ? yamlCodeText(codeCell) : "";
        const label = copyBtn.querySelector(".ro-copy-text");
        const done = (ok) => {
          if (!label) {
            return;
          }
          label.textContent = ok ? "copied" : "press ⌘C";
          window.setTimeout(() => {
            label.textContent = "copy";
          }, 1500);
        };
        if (navigator.clipboard && navigator.clipboard.writeText && text) {
          navigator.clipboard.writeText(text).then(() => done(true), () => done(false));
        } else {
          done(false);
        }
        return true;
      }
    },
    // .collapsible h4.title: toggle `is-collapsed` on the section and sync the
    // URL fragment (collapsed=<names>) with all currently-collapsed sections. The
    // section is resolved via closest('.collapsible') (NOT parentElement) so a
    // Unit-10 YAML card (h4.title nested in .ro-card-head) folds the right node.
    // Registered AFTER the copy binding (copy's stop:true short-circuits a copy
    // click), reproducing the monolith order. Kept its early-return (stop:true).
    {
      event: "click",
      selector: "main .collapsible h4.title",
      stop: true,
      handler: (_event, matched) => {
        const section = matched.closest(".collapsible");
        if (!section) {
          return true;
        }
        section.classList.toggle("is-collapsed");
        const names = [];
        document.querySelectorAll("main .is-collapsed").forEach((el) => {
          const name = el.dataset.name;
          if (name !== void 0) {
            names.push(name);
          }
        });
        if (names.length) {
          document.location.hash = `collapsed=${names.join(",")}`;
        } else {
          window.history.replaceState(null, "", window.location.pathname + window.location.search);
        }
        return true;
      }
    },
    // Namespace switch (D9): picking a namespace in the topbar dropdown records it
    // as this cluster's last-used namespace in the ro_prefs cookie (server-read
    // only, for cluster-entry hrefs -- never a redirect). The click is
    // deliberately NOT prevented; the boosted navigation proceeds. The cookie
    // write rides the prefs.ts surface directly (the same seam legacy uses).
    // Kept its early-return (stop:true).
    {
      event: "click",
      selector: "#namespace-dropdown .namespace-item",
      stop: true,
      handler: (_event, matched) => {
        const hrefMatch = /^\/clusters\/([^/]+)\/namespaces\/([^/]+)\//.exec(matched.getAttribute("href") || "");
        if (hrefMatch) {
          roPrefsSetNamespace(
            decodeURIComponent(hrefMatch[1]),
            decodeURIComponent(hrefMatch[2])
          );
        }
        return true;
      }
    },
    // #namespace-dropdown .context-trigger: toggle `is-active`; focus the
    // searchbox when opening. Kept its early-return (stop:true).
    {
      event: "click",
      selector: "#namespace-dropdown .context-trigger",
      stop: true,
      handler: (_event, matched) => {
        const nsDropdown = matched.closest("#namespace-dropdown");
        if (!nsDropdown) {
          return true;
        }
        nsDropdown.classList.toggle("is-active");
        if (nsDropdown.classList.contains("is-active")) {
          const searchbox = document.getElementById("namespace-searchbox");
          if (searchbox) {
            searchbox.focus();
          }
        }
        return true;
      }
    },
    // #namespace-searchbox input: filter the .namespace-item links by
    // case-insensitive substring. Terminal branch in the monolith input listener
    // (no branch followed it), reproduced as stop:true.
    {
      event: "input",
      selector: "#namespace-searchbox",
      stop: true,
      handler: (_event, matched) => {
        const filterText = matched.value.toLowerCase();
        document.querySelectorAll(".namespace-item").forEach((element) => {
          const text = (element.innerText || "").toLowerCase();
          if (text.indexOf(filterText) === -1) {
            element.classList.add("is-hidden");
          } else {
            element.classList.remove("is-hidden");
          }
        });
        return true;
      }
    },
    // #namespace-searchbox keyup: Enter selects the first still-visible match.
    // Sole branch of the monolith keyup listener; stop:true mirrors its return.
    {
      event: "keyup",
      selector: "#namespace-searchbox",
      stop: true,
      handler: (event) => {
        if (event.key !== "Enter") {
          return true;
        }
        const elements = document.querySelectorAll(".namespace-item");
        for (let i = 0; i < elements.length; i++) {
          if (!elements[i].classList.contains("is-hidden")) {
            elements[i].click();
            break;
          }
        }
        return true;
      }
    }
  ];

  // internal/assets/src/js/bindings.ts
  var bindings = [
    ...contextMenuBindings,
    ...bulkBindings,
    ...rowSelectionBindings,
    ...paletteBindings,
    ...keyboardBindings,
    ...foldBindings,
    ...logsBindings,
    // misc-ui's click bindings keep their relative monolith order: copy is
    // registered before the section-fold binding (copy stop:true short-circuits
    // a copy click), so a copy click never folds its section.
    ...miscBindings
  ];

  // internal/assets/src/js/toasts.ts
  var TOAST_VISIBLE_MS = 3500;
  var TOAST_LEAVE_MS = 200;
  function showToast(message) {
    const host = document.getElementById("ro-toasts");
    if (!host) {
      return;
    }
    const toast = document.createElement("div");
    toast.className = "ro-toast";
    toast.textContent = message;
    host.appendChild(toast);
    window.setTimeout(() => {
      toast.classList.add("is-leaving");
      window.setTimeout(() => toast.remove(), TOAST_LEAVE_MS);
    }, TOAST_VISIBLE_MS);
  }

  // internal/assets/src/js/stale.ts
  var STALE_DIM_CLASS = "ro-stale";
  var staleCountdownId = null;
  function updateStaleCountdown() {
    const span = document.querySelector(".ro-stale-banner [data-stale-countdown]");
    if (!span) {
      return;
    }
    const nextAt = refreshNextAtMs();
    if (!nextAt) {
      span.textContent = "…";
      return;
    }
    const remaining = Math.max(0, Math.ceil((nextAt - Date.now()) / 1e3));
    span.textContent = remaining + "s";
  }
  function isListRefreshEvent(event) {
    const detail = event.detail;
    if (!detail || isPreloadRequest(event)) {
      return false;
    }
    const elt = detail.elt;
    if (!!elt && elt.id === "resource-list-content") {
      return true;
    }
    const target = detail.target;
    return !!target && target.id === "resource-list-content";
  }
  function markListStale() {
    const content = document.getElementById("resource-list-content");
    if (content) {
      content.classList.add(STALE_DIM_CLASS);
    }
    const banner = document.querySelector(".ro-stale-banner");
    if (banner) {
      banner.hidden = false;
    }
    if (staleCountdownId === null) {
      staleCountdownId = window.setInterval(updateStaleCountdown, 1e3);
    }
    updateStaleCountdown();
  }
  function clearListStale() {
    const content = document.getElementById("resource-list-content");
    if (content) {
      content.classList.remove(STALE_DIM_CLASS);
    }
    const banner = document.querySelector(".ro-stale-banner");
    if (banner) {
      banner.hidden = true;
    }
    if (staleCountdownId !== null) {
      window.clearInterval(staleCountdownId);
      staleCountdownId = null;
    }
  }
  document.addEventListener("htmx:responseError", (event) => {
    if (isListRefreshEvent(event)) {
      noteRefreshFailure();
      markListStale();
    }
  });
  document.addEventListener("htmx:sendError", (event) => {
    if (isListRefreshEvent(event)) {
      noteRefreshFailure();
      markListStale();
    }
  });

  // internal/assets/src/js/live-policy.ts
  function effectivePollSeconds(mode, intervalSeconds, liveFallbackSeconds2) {
    if (intervalSeconds > 0) {
      return intervalSeconds;
    }
    return mode === "Live" ? liveFallbackSeconds2 : 0;
  }
  function refreshDelaySeconds(effectiveSeconds, failureStage) {
    if (effectiveSeconds <= 0) {
      return 0;
    }
    if (failureStage <= 1) {
      return effectiveSeconds;
    }
    const factor = failureStage === 2 ? 2 : 4;
    return Math.min(effectiveSeconds * factor, 60);
  }
  function nextFailureStage(stage) {
    return Math.min(stage + 1, 3);
  }
  function classifyStreamClose(facts) {
    if (facts.superseded) {
      return { kind: "ignore" };
    }
    switch (facts.cause) {
      case "connect-error":
      case "bad-status":
        return { kind: "fallback", banner: false, terminal: false };
      case "read-error":
      case "eof":
        return { kind: "fallback", banner: true, terminal: false };
      case "terminal-frame":
        return { kind: "fallback", banner: true, terminal: true };
    }
  }
  function shouldDiscardPush(facts) {
    if (facts.frameGeneration !== facts.currentGeneration) {
      return "stale-generation";
    }
    if (facts.liveStreamBase !== facts.openedStreamBase) {
      return "wrong-page";
    }
    if (facts.requestInFlight) {
      return "request-in-flight";
    }
    return "none";
  }

  // internal/assets/src/js/live.ts
  function getHtmx() {
    return window.htmx;
  }
  var liveState = {
    status: "idle",
    // 'idle' | 'connecting' | 'open' | 'fallback' | 'hidden'
    abort: null,
    // AbortController of the current stream fetch
    gen: "",
    // the minted generation every frame must echo (string compare)
    streamPath: ""
    // the stream URL sans ?g= -- the page/params identity
  };
  var liveGenSeq = 0;
  var liveDiscards = 0;
  var liveFallbackSecs = 0;
  function liveFallbackSeconds() {
    return liveFallbackSecs;
  }
  function liveSupported() {
    const content = document.getElementById("resource-list-content");
    if (!content || content.dataset.liveUrl !== "location") {
      return false;
    }
    const option = document.querySelector(
      '.refresh-option[data-interval="Live"]'
    );
    return !!option && !option.disabled;
  }
  function liveStreamBase() {
    const u = new URL(window.location.href);
    return u.pathname.replace(/\/+$/, "") + "/_stream" + u.search;
  }
  function liveTeardown() {
    const ctrl = liveState.abort;
    liveState.abort = null;
    liveFallbackSecs = 0;
    if (ctrl) {
      try {
        ctrl.abort();
      } catch {
      }
    }
  }
  function liveEngageFallback(banner) {
    liveTeardown();
    liveState.status = "fallback";
    liveFallbackSecs = document.getElementById("resource-list-content") ? 5 : 0;
    scheduleRefreshTick();
    if (banner) {
      markListStale();
    }
  }
  function liveOpen(base) {
    liveTeardown();
    liveFallbackSecs = 0;
    liveState.streamPath = base;
    if (!base) {
      liveEngageFallback(false);
      return;
    }
    liveState.status = "connecting";
    liveGenSeq += 1;
    liveState.gen = Date.now().toString(36) + "." + liveGenSeq;
    const ctrl = new AbortController();
    liveState.abort = ctrl;
    const url = base + (base.indexOf("?") === -1 ? "?" : "&") + "g=" + encodeURIComponent(liveState.gen);
    scheduleRefreshTick();
    void liveConnect(url, ctrl);
  }
  async function liveConnect(url, ctrl) {
    let res;
    try {
      res = await fetch(url, { signal: ctrl.signal });
    } catch {
      applyClose({ superseded: liveState.abort !== ctrl, cause: "connect-error" });
      return;
    }
    if (liveState.abort !== ctrl) {
      return;
    }
    if (res.status !== 200 || !res.body) {
      applyClose({ superseded: false, cause: "bad-status" });
      return;
    }
    liveState.status = "open";
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffered = "";
    let eventName = "";
    let dataText = "";
    try {
      for (; ; ) {
        const chunk = await reader.read();
        if (liveState.abort !== ctrl) {
          return;
        }
        if (chunk.done) {
          break;
        }
        buffered += decoder.decode(chunk.value, { stream: true });
        let nl = buffered.indexOf("\n");
        while (nl !== -1) {
          const line = buffered.slice(0, nl).replace(/\r$/, "");
          buffered = buffered.slice(nl + 1);
          if (line === "") {
            const ended = liveHandleFrame(eventName, dataText, ctrl);
            eventName = "";
            dataText = "";
            if (ended || liveState.abort !== ctrl) {
              return;
            }
          } else if (line.indexOf("event:") === 0) {
            eventName = line.slice(6).trim();
          } else if (line.indexOf("data:") === 0) {
            const piece = line.slice(5).replace(/^ /, "");
            dataText = dataText === "" ? piece : `${dataText}
${piece}`;
          }
          nl = buffered.indexOf("\n");
        }
      }
    } catch {
      applyClose({ superseded: liveState.abort !== ctrl, cause: "read-error" });
      return;
    }
    if (liveState.abort !== ctrl) {
      return;
    }
    applyClose({ superseded: false, cause: "eof" });
  }
  function applyClose(facts) {
    const action = classifyStreamClose(facts);
    if (action.kind === "ignore") {
      return;
    }
    liveEngageFallback(action.banner);
  }
  function liveHandleFrame(name, text, ctrl) {
    if (liveState.abort !== ctrl || text === "") {
      return false;
    }
    let payload = null;
    try {
      payload = JSON.parse(text);
    } catch {
      return false;
    }
    if (!payload || typeof payload !== "object") {
      return false;
    }
    if (name === "ro-terminal") {
      applyClose({ superseded: false, cause: "terminal-frame" });
      return true;
    }
    if (name !== "ro-table") {
      return false;
    }
    pruneSettledListRequests(userListRequestsInFlight);
    pruneSettledListRequests(containerListRequestsInFlight);
    const reason = shouldDiscardPush({
      frameGeneration: String(payload.g),
      currentGeneration: liveState.gen,
      liveStreamBase: liveStreamBase(),
      openedStreamBase: liveState.streamPath,
      requestInFlight: userListRequestsInFlight.size > 0 || containerListRequestsInFlight.size > 0
    });
    if (reason !== "none" || liveStreamBase() !== liveState.streamPath) {
      liveDiscards += 1;
      return false;
    }
    liveMorph(String(payload.html));
    return false;
  }
  function liveMorph(html) {
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx();
    if (!content || !htmx2 || typeof htmx2.swap !== "function") {
      return;
    }
    htmx2.swap(content, html, { swapStyle: "morph" }, {
      contextElement: content,
      eventInfo: { target: content, roLivePush: true }
    });
  }
  function liveOnListSwap(event) {
    const detail = event.detail;
    if (detail && detail.roLivePush) {
      return;
    }
    if (liveState.status !== "open" && liveState.status !== "connecting") {
      return;
    }
    let base = liveStreamBase();
    const pathInfo = detail && detail.pathInfo;
    const requestPath = pathInfo && (pathInfo.finalRequestPath || pathInfo.requestPath);
    if (requestPath && requestPath.indexOf("/_table") !== -1) {
      base = requestPath.replace("/_table", "/_stream");
    }
    liveOpen(base);
  }
  function liveApply(force) {
    if (refreshMode() !== "Live") {
      if (liveState.status !== "idle") {
        liveTeardown();
        liveState.status = "idle";
        liveState.streamPath = "";
        liveFallbackSecs = 0;
      }
      return;
    }
    const base = liveSupported() ? liveStreamBase() : "";
    if (!force && base === liveState.streamPath && liveState.status !== "idle") {
      return;
    }
    liveOpen(base);
  }
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      if (liveState.status === "open" || liveState.status === "connecting") {
        liveTeardown();
        liveState.status = "hidden";
      }
      return;
    }
    if (liveState.status === "hidden" && refreshMode() === "Live") {
      liveOpen(liveSupported() ? liveStreamBase() : "");
    }
  });
  window.roLive = {
    discards() {
      return liveDiscards;
    }
  };

  // internal/assets/src/js/refresh.ts
  function getHtmx2() {
    return window.htmx;
  }
  var refreshTimerId = null;
  var refreshNextAt = 0;
  var refreshFailureStage = 0;
  function refreshNextAtMs() {
    return refreshNextAt;
  }
  var userListRequestsInFlight = /* @__PURE__ */ new Set();
  var containerListRequestsInFlight = /* @__PURE__ */ new Set();
  function pruneSettledListRequests(requests) {
    requests.forEach((xhr) => {
      if (xhr.readyState === 4 || xhr.readyState === 0) {
        requests.delete(xhr);
      }
    });
  }
  function isPreloadRequest(event) {
    const cfg = event.detail && event.detail.requestConfig;
    return !!cfg && !!cfg.headers && cfg.headers["HX-Preloaded"] === "true";
  }
  function isUserListRequest(event) {
    const detail = event.detail;
    if (!detail || !detail.elt || !detail.target) {
      return false;
    }
    if (detail.elt.id === "resource-list-content") {
      return false;
    }
    return detail.target.id === "resource-list-content" && !isPreloadRequest(event);
  }
  document.addEventListener("htmx:configRequest", (event) => {
    const elt = event.detail && event.detail.elt;
    if (elt && elt.id === "resource-list-content") {
      event.detail.headers["RO-No-Push"] = "true";
    }
  });
  document.addEventListener("htmx:beforeRequest", (event) => {
    const detail = event.detail;
    if (detail && detail.xhr && detail.elt && detail.elt.id === "resource-list-content") {
      containerListRequestsInFlight.add(detail.xhr);
      return;
    }
    if (!isUserListRequest(event)) {
      return;
    }
    if (detail && detail.xhr) {
      userListRequestsInFlight.add(detail.xhr);
    }
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx2();
    if (content && htmx2) {
      htmx2.trigger(content, "htmx:abort");
    }
  });
  document.addEventListener("htmx:afterRequest", (event) => {
    const xhr = event.detail && event.detail.xhr;
    if (xhr) {
      userListRequestsInFlight.delete(xhr);
      containerListRequestsInFlight.delete(xhr);
    }
  });
  function refreshMode() {
    const stored = readPrefs().refresh;
    if (stored) {
      return stored;
    }
    let legacy = null;
    try {
      legacy = window.localStorage.getItem(REFRESH_KEY);
    } catch {
      return "";
    }
    if (legacy === null || legacy === "") {
      return "";
    }
    const secs = parseInt(legacy, 10) || 0;
    const mode = secs > 0 ? String(secs) : "Off";
    roPrefsSetRefresh(mode);
    return mode;
  }
  function refreshInterval() {
    const secs = parseInt(refreshMode(), 10);
    return Number.isFinite(secs) && secs > 0 ? secs : 0;
  }
  function listTableURL() {
    const u = new URL(window.location.href);
    return u.pathname.replace(/\/+$/, "") + "/_table" + u.search;
  }
  function requestListRefresh() {
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx2();
    if (!content || !htmx2) {
      return;
    }
    if (content.dataset.liveUrl === "location") {
      const request = htmx2.ajax("GET", listTableURL(), { source: content });
      if (request && typeof request.catch === "function") {
        request.catch(() => {
        });
      }
    } else {
      htmx2.trigger(content, "ro:refresh");
    }
  }
  window.requestListRefresh = requestListRefresh;
  function fireRefresh() {
    if (document.hidden) {
      return;
    }
    pruneSettledListRequests(userListRequestsInFlight);
    pruneSettledListRequests(containerListRequestsInFlight);
    if (userListRequestsInFlight.size > 0) {
      return;
    }
    if (containerListRequestsInFlight.size > 0) {
      return;
    }
    requestListRefresh();
  }
  function effectivePollSeconds2() {
    return effectivePollSeconds(refreshMode(), refreshInterval(), liveFallbackSeconds());
  }
  function refreshDelaySeconds2() {
    return refreshDelaySeconds(effectivePollSeconds2(), refreshFailureStage);
  }
  function scheduleRefreshTick() {
    if (refreshTimerId !== null) {
      window.clearTimeout(refreshTimerId);
      refreshTimerId = null;
    }
    const delay = refreshDelaySeconds2();
    if (delay <= 0) {
      refreshNextAt = 0;
      updateStaleCountdown();
      return;
    }
    refreshNextAt = Date.now() + delay * 1e3;
    refreshTimerId = window.setTimeout(() => {
      refreshTimerId = null;
      scheduleRefreshTick();
      fireRefresh();
    }, delay * 1e3);
    updateStaleCountdown();
  }
  function applyRefresh() {
    refreshFailureStage = 0;
    scheduleRefreshTick();
  }
  function syncRefreshUI() {
    const live = refreshMode() === "Live";
    const secs = refreshInterval();
    const label = document.getElementById("refresh-label");
    if (label) {
      label.textContent = live ? "Live" : secs > 0 ? `${secs}s` : "Off";
    }
    document.querySelectorAll(".refresh-option").forEach((opt) => {
      const value = opt.dataset.interval ?? "";
      opt.classList.toggle("is-active", live ? value === "Live" : value !== "Live" && (parseInt(value, 10) || 0) === secs);
    });
    const dropdown = document.getElementById("refresh-dropdown");
    if (dropdown) {
      dropdown.classList.toggle("refresh-on", live || secs > 0);
    }
  }
  function noteRefreshFailure() {
    refreshFailureStage = nextFailureStage(refreshFailureStage);
    scheduleRefreshTick();
  }
  function noteRefreshRecovery() {
    if (refreshFailureStage === 0) {
      return;
    }
    refreshFailureStage = 0;
    scheduleRefreshTick();
    const toast = window.roToast;
    if (typeof toast === "function") {
      toast("Refresh resumed");
    }
  }
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && effectivePollSeconds2() > 0) {
      fireRefresh();
    }
  });

  // internal/assets/src/js/skeleton.ts
  function listRegionIsEmpty(content) {
    return content.childElementCount === 0;
  }
  document.addEventListener("htmx:beforeRequest", (event) => {
    if (!isListRefreshEvent(event)) {
      return;
    }
    const content = document.getElementById("resource-list-content");
    const template = document.getElementById("ro-skel-template");
    if (!content || !template || !listRegionIsEmpty(content)) {
      return;
    }
    content.replaceChildren(
      ...Array.from(template.children, (node) => node.cloneNode(true))
    );
  });
  function clearListSkeleton() {
    const content = document.getElementById("resource-list-content");
    const skel = content && content.querySelector(":scope > .ro-skel");
    if (skel) {
      skel.remove();
    }
  }
  document.addEventListener("htmx:responseError", (event) => {
    if (isListRefreshEvent(event)) {
      clearListSkeleton();
    }
  });
  document.addEventListener("htmx:sendError", (event) => {
    if (isListRefreshEvent(event)) {
      clearListSkeleton();
    }
  });

  // internal/assets/src/js/legacy.js
  registerBindings(bindings);
  if (typeof htmx !== "undefined") {
    htmx.config.globalViewTransitions = !window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  }
  if (typeof Idiomorph !== "undefined" && Idiomorph.defaults && Idiomorph.defaults.callbacks && !window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
    const PRIOR = /* @__PURE__ */ new WeakMap();
    Idiomorph.defaults.callbacks.beforeNodeMorphed = (oldNode) => {
      if (oldNode && oldNode.nodeType === 1 && oldNode.tagName === "TD") {
        PRIOR.set(oldNode, oldNode.textContent);
      }
    };
    Idiomorph.defaults.callbacks.afterNodeMorphed = (oldNode) => {
      if (!oldNode || oldNode.nodeType !== 1 || oldNode.tagName !== "TD") {
        return;
      }
      if (!PRIOR.has(oldNode)) {
        return;
      }
      const before = PRIOR.get(oldNode);
      PRIOR.delete(oldNode);
      if (before !== oldNode.textContent) {
        oldNode.classList.remove("ro-cell-changed");
        void oldNode.offsetWidth;
        oldNode.classList.add("ro-cell-changed");
      }
    };
  }
  if (typeof htmx !== "undefined" && typeof Idiomorph !== "undefined") {
    htmx.defineExtension("ro-morph", {
      isInlineSwap: (swapStyle) => swapStyle === "morph",
      handleSwap: (swapStyle, target, fragment) => {
        if (swapStyle !== "morph") {
          return false;
        }
        if (target && target.id === "resource-list-content") {
          captureRowModel(fragment);
          virtualizePrepareSwap(fragment);
        }
        return Idiomorph.morph(target, fragment.children, {
          morphStyle: "innerHTML",
          ignoreActiveValue: true
        });
      }
    });
  }
  document.addEventListener("click", (event) => {
    const target = event.target;
    const staleRetry = target.closest(".ro-stale-retry");
    if (staleRetry) {
      event.preventDefault();
      const content = document.getElementById("resource-list-content");
      if (content && typeof htmx !== "undefined") {
        htmx.trigger(content, "htmx:abort");
      }
      requestListRefresh();
      return;
    }
    const chipRemove = target.closest("#ro-filter-field .chip-x");
    if (chipRemove) {
      event.preventDefault();
      const href = chipRemove.getAttribute("href");
      if (href) {
        issueFilterNavigation(href);
      }
      return;
    }
    const acItem = target.closest("#ro-filter-ac .ro-ac-item");
    if (acItem) {
      event.preventDefault();
      setFilterACActive(Number(acItem.dataset.acIndex) || 0);
      acceptFilterAC(true);
      const input = document.getElementById("ro-filter-input");
      if (input) {
        input.focus();
      }
      return;
    }
    const filterField = target.closest("#ro-filter-field");
    if (filterField) {
      const input = document.getElementById("ro-filter-input");
      if (input && target !== input) {
        input.focus();
      }
      return;
    }
    const colsBtn = target.closest("[data-cols-toggle]");
    if (colsBtn) {
      event.preventDefault();
      const pop = document.getElementById("ro-cols-pop");
      setColsPopOpen(!!pop && !pop.classList.contains("is-open"));
      return;
    }
    const colToggle = target.closest(".col-toggle");
    if (colToggle) {
      event.preventDefault();
      const check = colToggle.querySelector(".ro-check");
      if (check) {
        check.checked = !check.checked;
      }
      commitColumnVisibility(colToggle.closest(".ro-pop"));
      return;
    }
    const moreChips = target.closest("[data-more]");
    if (moreChips) {
      event.preventDefault();
      const chips = moreChips.closest(".ro-chips");
      if (chips) {
        const expanded = chips.classList.toggle("expanded");
        moreChips.setAttribute("aria-expanded", expanded ? "true" : "false");
      }
      return;
    }
    const annoToggle = target.closest("[data-annolong]");
    if (annoToggle) {
      event.preventDefault();
      const pre = annoToggle.parentElement && annoToggle.parentElement.querySelector(".anno-pre");
      if (pre) {
        const open = pre.hidden;
        pre.hidden = !open;
        annoToggle.setAttribute("aria-expanded", open ? "true" : "false");
        annoToggle.classList.toggle("open", open);
      }
      return;
    }
    const refreshOption = target.closest(".refresh-option");
    if (refreshOption) {
      if (refreshOption.dataset.interval === "Live") {
        roPrefsSetRefresh("Live");
      } else {
        const interval = parseInt(refreshOption.dataset.interval, 10) || 0;
        roPrefsSetRefresh(interval > 0 ? String(interval) : "Off");
      }
      liveApply(true);
      syncRefreshUI();
      applyRefresh();
      refreshOption.blur();
      event.preventDefault();
      return;
    }
    const toggle = target.closest(".toggle-tools");
    if (toggle) {
      event.preventDefault();
      toggle.classList.toggle("is-active");
      const targetEl = document.getElementById(toggle.dataset.target);
      if (targetEl) {
        targetEl.classList.toggle("is-active");
      }
      return;
    }
  });
  document.addEventListener("change", (event) => {
    const checkbox = event.target.closest("input[data-toggle-button]");
    if (checkbox) {
      const buttonId = checkbox.dataset.toggleButton;
      const button = document.getElementById(buttonId);
      if (button) {
        const anyChecked = document.querySelectorAll(
          `input[data-toggle-button="${buttonId}"]:checked`
        ).length > 0;
        button.disabled = !anyChecked;
      }
      return;
    }
  });
  document.addEventListener("input", (event) => {
    const filterInput = event.target.closest("#ro-filter-input");
    if (filterInput) {
      hideFilterFieldHint();
      applyLiveNameFilter();
      updateFilterAC();
      return;
    }
  });
  document.addEventListener("keydown", (event) => {
    if (event.target && event.target.id === "ro-filter-input") {
      handleFilterInputKeydown(event);
    }
  });
  function popFormMergedHref(form) {
    const owned = /* @__PURE__ */ new Set();
    const fields = [];
    Array.prototype.slice.call(form.elements).forEach((el) => {
      if (el.tagName !== "INPUT" || el.type === "hidden" || !el.name) {
        return;
      }
      owned.add(el.name);
      if (el.value) {
        fields.push(el.name + "=" + encodeURIComponent(el.value));
      }
    });
    const kept = [];
    window.location.search.replace(/^\?/, "").split("&").forEach((pair) => {
      if (pair && !owned.has(pair.split("=")[0])) {
        kept.push(pair);
      }
    });
    const query = kept.concat(fields).join("&");
    return window.location.pathname + (query ? "?" + query : "");
  }
  document.addEventListener("submit", (event) => {
    const popForm = event.target.closest("form.ro-pop-form");
    if (popForm) {
      event.preventDefault();
      issueFilterNavigation(popFormMergedHref(popForm));
      return;
    }
    const form = event.target.closest("form.tools-form");
    if (form) {
      Array.prototype.slice.call(form.getElementsByTagName("input")).forEach((input) => {
        if (input.name && !input.value) {
          input.name = "";
        }
      });
    }
  });
  document.addEventListener("htmx:beforeRequest", (event) => {
    const detail = event.detail;
    const cfg = detail && detail.requestConfig;
    if (!cfg || !detail.elt || !detail.target || detail.target.id !== "resource-list-content") {
      return;
    }
    if (cfg.headers && (cfg.headers["RO-No-Push"] || cfg.headers["HX-Preloaded"] === "true")) {
      return;
    }
    if (typeof detail.elt.closest !== "function" || !detail.elt.closest("thead th")) {
      return;
    }
    const pathMatch = /\/([^/]+)\/_table(?:[?#]|$)/.exec(cfg.path || "");
    if (!pathMatch) {
      return;
    }
    let sort = "";
    try {
      sort = new URL(cfg.path, window.location.href).searchParams.get("sort") || "";
    } catch (e) {
      return;
    }
    const plural = decodeURIComponent(pathMatch[1]);
    if (plural && sort) {
      roPrefsSetSort(plural, sort);
    }
  });
  document.addEventListener("htmx:afterSwap", (event) => {
    if (isListRefreshEvent(event)) {
      noteRefreshRecovery();
      clearListStale();
      reapplyRowState();
      applyLiveNameFilter();
      const filterInput = document.getElementById("ro-filter-input");
      if (filterInput && document.activeElement === filterInput && filterInput.value) {
        updateFilterAC();
      }
      if (colsPopOpen) {
        setColsPopOpen(true);
      }
      virtualizeAfterSwap();
      liveOnListSwap(event);
    }
  });
  window.roToast = showToast;
  document.addEventListener("htmx:beforeSwap", (event) => {
    if (event.detail && event.detail.target === document.body) {
      closeRowMenu();
      clearRowState();
      clearListStale();
      liveTeardown();
      liveState.status = "idle";
      liveState.streamPath = "";
    }
  });
  document.addEventListener("htmx:historyRestore", () => {
    reapplyRowState();
    updateBulkBar();
  });
  var VIRT_BUFFER_ROWS = 12;
  var virtState = {
    active: false,
    rows: [],
    // the FULL identity row set, server order
    byKey: /* @__PURE__ */ new Map(),
    // key -> tr over `rows` (rendered or detached)
    visible: [],
    // rows passing the live free-text filter, in order
    rowH: 0,
    // the measured fixed row pitch (px)
    start: 0,
    // rendered slice bounds over `visible`
    end: 0,
    table: null,
    tbody: null,
    topSpacer: null,
    bottomSpacer: null,
    pinnedWidths: [],
    // engagement-time column widths (full-render truth)
    pendingRows: null,
    // adoption handoff from the ro-morph handleSwap
    pendingScrollY: null
  };
  function virtualizerActive() {
    return virtState.active && !!virtState.tbody && virtState.tbody.isConnected;
  }
  function virtReset() {
    virtState.active = false;
    virtState.rows = [];
    virtState.byKey = /* @__PURE__ */ new Map();
    virtState.visible = [];
    virtState.rowH = 0;
    virtState.start = 0;
    virtState.end = 0;
    virtState.table = null;
    virtState.tbody = null;
    virtState.topSpacer = null;
    virtState.bottomSpacer = null;
    virtState.pinnedWidths = [];
    virtState.pendingRows = null;
    virtState.pendingScrollY = null;
  }
  function virtMakeSpacer() {
    const tr = document.createElement("tr");
    tr.className = "ro-vspacer";
    tr.setAttribute("aria-hidden", "true");
    tr.appendChild(document.createElement("td"));
    return tr;
  }
  function virtSetSpacerColspan() {
    const cols = virtState.table.querySelectorAll("thead th").length || 1;
    virtState.topSpacer.firstElementChild.colSpan = cols;
    virtState.bottomSpacer.firstElementChild.colSpan = cols;
  }
  function virtMeasureRowHeight() {
    const rendered = virtState.tbody.querySelectorAll(":scope > tr[data-key]");
    if (rendered.length === 0) {
      return 0;
    }
    const first = rendered[0].getBoundingClientRect();
    const last = rendered[rendered.length - 1].getBoundingClientRect();
    const pitch = (last.bottom - first.top) / rendered.length;
    return pitch > 0 ? pitch : 0;
  }
  function virtFallbackRowHeight() {
    let py = 9;
    let lh = 18;
    try {
      const cs = window.getComputedStyle(document.documentElement);
      py = parseFloat(cs.getPropertyValue("--row-py")) || py;
      const cell = virtState.tbody && virtState.tbody.querySelector("td");
      if (cell) {
        lh = parseFloat(window.getComputedStyle(cell).lineHeight) || lh;
      }
    } catch (e) {
    }
    return py * 2 + lh + 1;
  }
  function virtApplyPins() {
    const ths = virtState.table.querySelectorAll("thead th");
    if (virtState.pinnedWidths.length !== ths.length) {
      return false;
    }
    ths.forEach((th, i) => {
      th.style.width = virtState.pinnedWidths[i] + "px";
    });
    virtState.table.classList.add("ro-virtualized");
    return true;
  }
  function virtPinColumns() {
    const ths = Array.from(virtState.table.querySelectorAll("thead th"));
    virtState.pinnedWidths = ths.map((th) => th.getBoundingClientRect().width);
    virtApplyPins();
  }
  function virtComputeVisible() {
    const keys = roRowModel.visibleKeys;
    virtState.visible = keys ? virtState.rows.filter((tr) => keys.has(tr.dataset.key)) : virtState.rows.slice();
  }
  function virtWindowBounds() {
    const rect = virtState.tbody.getBoundingClientRect();
    const rowH = virtState.rowH || 1;
    const n = virtState.visible.length;
    const first = Math.floor((0 - rect.top) / rowH);
    const last = Math.ceil((window.innerHeight - rect.top) / rowH);
    let start = Math.max(0, first - VIRT_BUFFER_ROWS);
    let end = Math.min(n, last + VIRT_BUFFER_ROWS);
    if (start > n) {
      start = n;
    }
    if (end < start) {
      end = start;
    }
    return { start, end };
  }
  function virtRenderWindow() {
    const s = virtState;
    const bounds = virtWindowBounds();
    s.start = bounds.start;
    s.end = bounds.end;
    const n = s.visible.length;
    s.topSpacer.firstElementChild.style.height = s.start * s.rowH + "px";
    s.bottomSpacer.firstElementChild.style.height = (n - s.end) * s.rowH + "px";
    const slice = s.visible.slice(s.start, s.end);
    slice.forEach((tr) => tr.classList.remove(FILTER_HIDE_CLASS));
    s.tbody.replaceChildren(s.topSpacer, ...slice, s.bottomSpacer);
    reapplyRowState();
  }
  function virtBindMounts() {
    const content = document.getElementById("resource-list-content");
    const wrap = content && content.querySelector(".ro-table-wrap.ro-windowed");
    const table = wrap && wrap.querySelector("table.ro-table");
    const tbody = table && table.tBodies.length > 0 ? table.tBodies[0] : null;
    virtState.table = table || null;
    virtState.tbody = tbody || null;
    return !!tbody;
  }
  function virtualizeInit() {
    const content = document.getElementById("resource-list-content");
    const wrap = content && content.querySelector(".ro-table-wrap.ro-windowed");
    if (!wrap) {
      virtReset();
      return;
    }
    const table = wrap.querySelector("table.ro-table");
    const tbody = table && table.tBodies.length > 0 ? table.tBodies[0] : null;
    if (!tbody) {
      virtReset();
      return;
    }
    if (tbody.querySelector(":scope > tr.ro-vspacer")) {
      if (virtState.active && virtState.tbody === tbody) {
        return;
      }
      virtReset();
      requestListRefresh();
      return;
    }
    const rows = Array.from(tbody.querySelectorAll(":scope > tr[data-key]"));
    if (rows.length === 0) {
      virtReset();
      return;
    }
    virtReset();
    virtState.table = table;
    virtState.tbody = tbody;
    virtState.rows = rows;
    virtState.byKey = new Map(rows.map((tr) => [tr.dataset.key, tr]));
    virtState.topSpacer = virtMakeSpacer();
    virtState.bottomSpacer = virtMakeSpacer();
    virtSetSpacerColspan();
    virtState.rowH = virtMeasureRowHeight() || virtFallbackRowHeight();
    virtPinColumns();
    virtState.active = true;
    virtComputeVisible();
    virtRenderWindow();
  }
  function virtualizePrepareSwap(fragment) {
    virtState.pendingRows = null;
    virtState.pendingScrollY = null;
    const wrap = fragment.querySelector(".ro-table-wrap.ro-windowed");
    const tbody = wrap ? wrap.querySelector("table.ro-table tbody") : null;
    if (!tbody) {
      return;
    }
    const rows = [];
    Array.prototype.forEach.call(tbody.children, (el) => {
      if (el.tagName === "TR" && el.dataset.key) {
        rows.push(el);
      }
    });
    if (rows.length === 0) {
      return;
    }
    virtState.pendingRows = rows;
    virtState.pendingScrollY = window.scrollY;
    const rowH = virtState.rowH || virtFallbackRowHeight();
    const start = Math.min(virtState.active ? virtState.start : 0, rows.length);
    const topSpacer = virtMakeSpacer();
    const bottomSpacer = virtMakeSpacer();
    topSpacer.firstElementChild.style.height = start * rowH + "px";
    bottomSpacer.firstElementChild.style.height = Math.max(0, rows.length - start) * rowH + "px";
    tbody.replaceChildren(topSpacer, bottomSpacer);
  }
  function virtualizeAfterSwap() {
    const pending = virtState.pendingRows;
    virtState.pendingRows = null;
    if (!pending) {
      if (virtState.active) {
        virtReset();
      }
      return;
    }
    const prior = virtState.byKey;
    const wasActive = virtState.active;
    if (!virtBindMounts()) {
      virtReset();
      return;
    }
    virtState.rows = pending;
    virtState.byKey = new Map(pending.map((tr) => [tr.dataset.key, tr]));
    if (!virtState.topSpacer) {
      virtState.topSpacer = virtMakeSpacer();
      virtState.bottomSpacer = virtMakeSpacer();
    }
    virtSetSpacerColspan();
    virtState.active = true;
    if (!virtState.rowH) {
      virtState.rowH = virtFallbackRowHeight();
    }
    virtComputeVisible();
    virtRenderWindow();
    if (!wasActive) {
      const measured = virtMeasureRowHeight();
      if (measured && Math.abs(measured - virtState.rowH) > 0.5) {
        virtState.rowH = measured;
        virtRenderWindow();
      }
    }
    if (!virtApplyPins()) {
      virtPinColumns();
    }
    if (virtState.pendingScrollY !== null && window.scrollY !== virtState.pendingScrollY) {
      window.scrollTo(0, virtState.pendingScrollY);
      virtRenderWindow();
    }
    virtState.pendingScrollY = null;
    virtFlashChangedCells(prior);
  }
  function virtFlashChangedCells(prior) {
    if (!prior || prior.size === 0 || window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      return;
    }
    virtState.tbody.querySelectorAll(":scope > tr[data-key]").forEach((tr) => {
      const old = prior.get(tr.dataset.key);
      if (!old) {
        return;
      }
      const oldCells = old.children;
      const newCells = tr.children;
      for (let i = 0; i < newCells.length; i++) {
        const o = oldCells[i];
        const nd = newCells[i];
        if (o && nd && nd.tagName === "TD" && o.textContent !== nd.textContent) {
          nd.classList.remove("ro-cell-changed");
          void nd.offsetWidth;
          nd.classList.add("ro-cell-changed");
        }
      }
    });
  }
  function virtualizeOnFilterChange() {
    if (!virtualizerActive() || virtState.pendingRows) {
      return;
    }
    virtComputeVisible();
    virtRenderWindow();
  }
  function virtualizeMoveFocus(delta) {
    const list = virtState.visible;
    if (list.length === 0) {
      return false;
    }
    let current = -1;
    const focusKey = window.roRowState.focusedKey();
    for (let i = 0; i < list.length; i++) {
      if (list[i].dataset.key === focusKey) {
        current = i;
        break;
      }
    }
    const next = Math.max(0, Math.min(list.length - 1, current + delta));
    virtualizeScrollToIndex(next);
    window.roRowState.setFocus(list[next].dataset.key);
    return true;
  }
  function virtualizeScrollToIndex(index) {
    const rect = virtState.tbody.getBoundingClientRect();
    const rowTop = rect.top + index * virtState.rowH;
    const rowBottom = rowTop + virtState.rowH;
    const topbar = document.querySelector("header.ro-topbar");
    const topMin = topbar ? topbar.getBoundingClientRect().bottom : 0;
    if (rowTop < topMin) {
      window.scrollBy(0, rowTop - topMin);
    } else if (rowBottom > window.innerHeight) {
      window.scrollBy(0, rowBottom - window.innerHeight);
    }
    virtRenderWindow();
  }
  var virtScrollScheduled = false;
  function virtOnScroll() {
    if (!virtualizerActive()) {
      return;
    }
    const bounds = virtWindowBounds();
    if (bounds.start !== virtState.start || bounds.end !== virtState.end) {
      virtRenderWindow();
    }
  }
  window.addEventListener("scroll", () => {
    if (!virtState.active || virtScrollScheduled) {
      return;
    }
    virtScrollScheduled = true;
    window.requestAnimationFrame(() => {
      virtScrollScheduled = false;
      virtOnScroll();
    });
  }, { passive: true });
  window.addEventListener("resize", virtOnScroll);
  if (document.fonts && document.fonts.ready && typeof document.fonts.ready.then === "function") {
    document.fonts.ready.then(() => {
      if (!virtualizerActive()) {
        return;
      }
      const measured = virtMeasureRowHeight();
      if (measured && Math.abs(measured - virtState.rowH) > 0.5) {
        virtState.rowH = measured;
        virtRenderWindow();
      }
    });
  }
  window.roVirtual = {
    active: virtualizerActive,
    renderedBounds() {
      return { start: virtState.start, end: virtState.end, total: virtState.visible.length };
    },
    scrollToKey(key) {
      if (!virtualizerActive()) {
        return false;
      }
      const tr = virtState.byKey.get(key);
      const index = tr ? virtState.visible.indexOf(tr) : -1;
      if (index === -1) {
        return false;
      }
      virtualizeScrollToIndex(index);
      return true;
    }
  };
  var colsPopOpen = false;
  function setColsPopOpen(open) {
    colsPopOpen = open;
    const pop = document.getElementById("ro-cols-pop");
    if (pop) {
      pop.classList.toggle("is-open", open);
    }
    const btn = document.getElementById("ro-cols-btn");
    if (btn) {
      btn.setAttribute("aria-expanded", open ? "true" : "false");
    }
  }
  function syncColsPopState() {
    const pop = document.getElementById("ro-cols-pop");
    colsPopOpen = !!pop && pop.classList.contains("is-open");
  }
  window.roClusterBridge = {
    virtualizerActive,
    virtRows() {
      return virtState.rows;
    },
    virtVisible() {
      return virtState.visible;
    },
    virtRowByKey(key) {
      return virtState.byKey.get(key) || null;
    },
    virtMoveFocus(delta) {
      return virtualizeMoveFocus(delta);
    },
    colsPopOpen() {
      return colsPopOpen;
    }
  };
  function commitColumnVisibility(pop) {
    if (!pop) {
      return;
    }
    const plural = pop.dataset.plural || "";
    if (!plural) {
      return;
    }
    const hidden = [];
    pop.querySelectorAll(".col-toggle").forEach((toggle) => {
      const check = toggle.querySelector(".ro-check");
      if (!toggle.disabled && check && !check.checked && toggle.dataset.col) {
        hidden.push(toggle.dataset.col);
      }
    });
    roPrefsSetHiddenColumns(plural, hidden);
    const content = document.getElementById("resource-list-content");
    if (content && typeof htmx !== "undefined") {
      htmx.trigger(content, "htmx:abort");
    }
    requestListRefresh();
  }
  document.addEventListener("click", (event) => {
    if (!colsPopOpen) {
      return;
    }
    if (event.target.closest("#ro-cols-pop") || event.target.closest("[data-cols-toggle]")) {
      return;
    }
    setColsPopOpen(false);
  });
  var roRowModel = {
    fields: [],
    // [{ label, name, hint }] -- hint '' = not filterable
    rows: [],
    // [{ key, name, cells: [string] }] -- cells align with fields
    visibleKeys: null
    // Set of keys passing the live name match; null = no live filter
  };
  window.roRowModel = roRowModel;
  function normalizeFieldName(s) {
    return (s || "").toLowerCase().replace(/-/g, " ").trim();
  }
  function fieldSuggestionText(label) {
    return (label || "").toLowerCase().trim().replace(/\s+/g, "-");
  }
  function captureRowModel(root) {
    const table = root.querySelector("table.ro-table");
    if (!table) {
      roRowModel.fields = [];
      roRowModel.rows = [];
      return;
    }
    const fields = [];
    table.querySelectorAll("thead th").forEach((th) => {
      const label = (th.textContent || "").trim();
      fields.push({ label, name: fieldSuggestionText(label), hint: th.dataset.hint || "" });
    });
    const rows = [];
    table.querySelectorAll("tbody tr[data-key]").forEach((tr) => {
      const cells = [];
      tr.querySelectorAll("td").forEach((td) => {
        cells.push((td.textContent || "").trim());
      });
      const nameLink = tr.querySelector("td.cell-name a");
      rows.push({
        key: tr.dataset.key,
        name: nameLink ? (nameLink.textContent || "").trim() : cells[0] || "",
        cells
      });
    });
    roRowModel.fields = fields;
    roRowModel.rows = rows;
  }
  function captureRowModelFromDocument() {
    const content = document.getElementById("resource-list-content");
    if (content && document.getElementById("ro-filter-input") && !virtualizerActive()) {
      captureRowModel(content);
    }
  }
  function splitFilterDraft(s) {
    for (let i = 0; i < s.length; i++) {
      const c = s[i];
      if (c === "!" && s[i + 1] === "=") {
        return { field: s.slice(0, i).trim(), op: "!=", value: s.slice(i + 2) };
      }
      if (c === ":" || c === ">" || c === "<") {
        return { field: s.slice(0, i).trim(), op: c, value: s.slice(i + 1) };
      }
    }
    return null;
  }
  function filterSuggestionFields() {
    const out = [];
    roRowModel.fields.forEach((f) => {
      if (!f.hint) {
        return;
      }
      const norm = normalizeFieldName(f.label);
      if (norm === "cpu" || norm === "memory") {
        return;
      }
      out.push({ text: f.name, hint: f.hint });
    });
    out.push({ text: "label", hint: "key=value" });
    if (hasModelColumn("cpu usage")) {
      out.push({ text: "cpu", hint: "quantity" });
    }
    if (hasModelColumn("memory usage")) {
      out.push({ text: "memory", hint: "quantity" });
    }
    return out;
  }
  function hasModelColumn(normName) {
    return roRowModel.fields.some((f) => f.hint && normalizeFieldName(f.label) === normName);
  }
  function filterFieldKnown(field) {
    const want = normalizeFieldName(field);
    if (!want) {
      return false;
    }
    if (want === "label") {
      return true;
    }
    if (want === "cpu" || want === "memory") {
      return hasModelColumn(want + " usage");
    }
    return roRowModel.fields.some((f) => f.hint && normalizeFieldName(f.label) === want);
  }
  function fieldColumnIndex(field) {
    let want = normalizeFieldName(field);
    if (want === "cpu" || want === "memory") {
      want += " usage";
    }
    for (let i = 0; i < roRowModel.fields.length; i++) {
      const f = roRowModel.fields[i];
      if (f.hint && normalizeFieldName(f.label) === want) {
        return i;
      }
    }
    return -1;
  }
  var FILTER_HIDE_CLASS = "ro-row-filtered";
  function applyLiveNameFilter() {
    const content = document.getElementById("resource-list-content");
    if (!content) {
      return;
    }
    const input = document.getElementById("ro-filter-input");
    const draft = input ? input.value : "";
    const text = !draft || splitFilterDraft(draft) ? "" : draft.trim().toLowerCase();
    let visible = null;
    if (text) {
      visible = /* @__PURE__ */ new Set();
      roRowModel.rows.forEach((row) => {
        if (row.name.toLowerCase().indexOf(text) !== -1) {
          visible.add(row.key);
        }
      });
    }
    roRowModel.visibleKeys = visible;
    content.querySelectorAll("tbody tr[data-key]").forEach((tr) => {
      tr.classList.toggle(FILTER_HIDE_CLASS, !!visible && !visible.has(tr.dataset.key));
    });
    virtualizeOnFilterChange();
  }
  function issueFilterNavigation(href) {
    const content = document.getElementById("resource-list-content");
    const input = document.getElementById("ro-filter-input");
    if (!content || !input || typeof htmx === "undefined") {
      window.location.assign(href);
      return;
    }
    const u = new URL(href, window.location.href);
    const partial = u.pathname.replace(/\/+$/, "") + "/_table" + u.search;
    const request = htmx.ajax("GET", partial, {
      source: input,
      target: "#resource-list-content",
      swap: "morph"
    });
    if (request && typeof request.catch === "function") {
      request.catch(() => {
      });
    }
  }
  function commitFilterChip(draft) {
    const text = draft.trim();
    const parsed = splitFilterDraft(text);
    if (!parsed) {
      return;
    }
    if (!filterFieldKnown(parsed.field)) {
      showFilterFieldHint();
      return;
    }
    const raw = encodeURIComponent(text).replace(/%2C/gi, ",");
    const search = window.location.search;
    const href = window.location.pathname + (search ? search + "&" : "?") + "f=" + raw;
    clearFilterDraft();
    issueFilterNavigation(href);
  }
  function popLastFilterChip() {
    const removers = document.querySelectorAll("#ro-filter-field .ro-scope-chip .chip-x");
    if (removers.length === 0) {
      return;
    }
    const href = removers[removers.length - 1].getAttribute("href");
    if (href) {
      issueFilterNavigation(href);
    }
  }
  function clearFilterDraft() {
    const input = document.getElementById("ro-filter-input");
    if (input) {
      input.value = "";
    }
    closeFilterAC();
    applyLiveNameFilter();
  }
  function showFilterFieldHint() {
    const el = document.getElementById("ro-filter-error");
    if (!el) {
      return;
    }
    const names = filterSuggestionFields().slice(0, 3).map((f) => f.text);
    el.textContent = "no such field — try " + (names.length ? names.join(", ") : "status, node, age") + "…";
    el.hidden = false;
  }
  function hideFilterFieldHint() {
    const el = document.getElementById("ro-filter-error");
    if (el) {
      el.hidden = true;
    }
  }
  var filterACItems = [];
  var filterACActive = -1;
  function filterACOpen() {
    const ac = document.getElementById("ro-filter-ac");
    return !!ac && !ac.hidden;
  }
  function closeFilterAC() {
    const ac = document.getElementById("ro-filter-ac");
    if (ac) {
      ac.hidden = true;
      ac.textContent = "";
    }
    filterACItems = [];
    filterACActive = -1;
  }
  function openFilterAC(items) {
    const ac = document.getElementById("ro-filter-ac");
    if (!ac || items.length === 0) {
      closeFilterAC();
      return;
    }
    ac.textContent = "";
    ac.setAttribute("role", "listbox");
    filterACItems = items;
    filterACActive = 0;
    items.forEach((item, idx) => {
      const row = document.createElement("div");
      row.className = "ro-ac-item" + (idx === 0 ? " active" : "");
      row.setAttribute("role", "option");
      row.setAttribute("aria-selected", idx === 0 ? "true" : "false");
      row.dataset.acIndex = String(idx);
      const name = document.createElement("span");
      name.className = "ac-name";
      name.textContent = item.label;
      row.appendChild(name);
      if (item.hint) {
        const hint = document.createElement("span");
        hint.className = "ac-hint";
        hint.textContent = item.hint;
        row.appendChild(hint);
      }
      row.addEventListener("mousemove", () => setFilterACActive(idx));
      ac.appendChild(row);
    });
    ac.hidden = false;
  }
  function setFilterACActive(index) {
    if (filterACItems.length === 0) {
      return;
    }
    filterACActive = Math.max(0, Math.min(filterACItems.length - 1, index));
    const ac = document.getElementById("ro-filter-ac");
    if (!ac) {
      return;
    }
    ac.querySelectorAll(".ro-ac-item").forEach((el) => {
      const on = Number(el.dataset.acIndex) === filterACActive;
      el.classList.toggle("active", on);
      el.setAttribute("aria-selected", on ? "true" : "false");
    });
  }
  function moveFilterACActive(delta) {
    if (filterACItems.length === 0) {
      return;
    }
    setFilterACActive((filterACActive + delta + filterACItems.length) % filterACItems.length);
  }
  function updateFilterAC() {
    const input = document.getElementById("ro-filter-input");
    if (!input) {
      return;
    }
    const draft = input.value;
    if (!draft.trim()) {
      closeFilterAC();
      return;
    }
    const parsed = splitFilterDraft(draft);
    if (!parsed) {
      const q = normalizeFieldName(draft);
      const fields = filterSuggestionFields().filter(
        (f) => normalizeFieldName(f.text).indexOf(q) !== -1
      );
      fields.sort((a, b) => {
        const ap = normalizeFieldName(a.text).indexOf(q) === 0 ? 0 : 1;
        const bp = normalizeFieldName(b.text).indexOf(q) === 0 ? 0 : 1;
        return ap - bp;
      });
      openFilterAC(fields.map((f) => ({
        label: f.text,
        hint: f.hint,
        insert: f.text + ":",
        kind: "field"
      })));
      return;
    }
    const isLabel = normalizeFieldName(parsed.field) === "label";
    if (parsed.op !== ":" || isLabel || !filterFieldKnown(parsed.field)) {
      closeFilterAC();
      return;
    }
    const idx = fieldColumnIndex(parsed.field);
    if (idx < 0) {
      closeFilterAC();
      return;
    }
    const freq = /* @__PURE__ */ new Map();
    roRowModel.rows.forEach((row) => {
      const v = row.cells[idx];
      if (v) {
        freq.set(v, (freq.get(v) || 0) + 1);
      }
    });
    const typed = parsed.value.trim().toLowerCase();
    let entries = Array.from(freq.entries());
    if (typed) {
      entries = entries.filter(([v]) => v.toLowerCase().indexOf(typed) !== -1);
    }
    entries.sort((a, b) => b[1] - a[1]);
    openFilterAC(entries.slice(0, 8).map(([v, n]) => ({
      label: v,
      hint: "×" + n,
      insert: parsed.field.trim() + ":" + v,
      kind: "value"
    })));
  }
  function acceptFilterAC(commitValues) {
    const input = document.getElementById("ro-filter-input");
    const item = filterACItems[filterACActive];
    if (!input || !item) {
      return;
    }
    input.value = item.insert;
    closeFilterAC();
    if (item.kind === "value" && commitValues) {
      commitFilterChip(input.value);
    } else {
      applyLiveNameFilter();
      updateFilterAC();
    }
  }
  function handleFilterInputKeydown(event) {
    const input = event.target;
    if (event.key === "Enter") {
      event.preventDefault();
      if (filterACOpen() && filterACActive >= 0) {
        acceptFilterAC(true);
        return;
      }
      commitFilterChip(input.value);
      return;
    }
    if (event.key === "Tab" && filterACOpen()) {
      event.preventDefault();
      acceptFilterAC(false);
      return;
    }
    if (event.key === "Escape" && filterACOpen()) {
      event.preventDefault();
      closeFilterAC();
      return;
    }
    if (event.key === "ArrowDown" && filterACOpen()) {
      event.preventDefault();
      moveFilterACActive(1);
      return;
    }
    if (event.key === "ArrowUp" && filterACOpen()) {
      event.preventDefault();
      moveFilterACActive(-1);
      return;
    }
    if (event.key === "Backspace" && input.value === "") {
      event.preventDefault();
      popLastFilterChip();
    }
  }
  document.addEventListener("click", (event) => {
    if (!event.target.closest("#ro-filter-field")) {
      closeFilterAC();
    }
  });
  function setupStickyNamespace() {
    document.querySelectorAll(".ro-table-wrap table.ro-table").forEach((table) => {
      const firstCell = table.querySelector("tbody tr:not(.ro-vspacer) td:first-child");
      if (firstCell && firstCell.classList.contains("cell-ns")) {
        table.style.setProperty("--ns-col-w", firstCell.getBoundingClientRect().width + "px");
        table.classList.add("ro-sticky2");
      } else {
        table.classList.remove("ro-sticky2");
        table.style.removeProperty("--ns-col-w");
      }
    });
  }
  function runInitStep(step) {
    try {
      step();
    } catch (e) {
      console.warn("readout init step failed", e);
    }
  }
  function runInit() {
    [
      syncRefreshUI,
      // Live stream reconciliation (Unit 27/D19), BEFORE applyRefresh so
      // the poll chain arms against fresh live state: a riding stream
      // disarms it (effective 0), a fallback sets the 5s cadence.
      liveApply,
      applyRefresh,
      buildYamlFolds,
      collapseSectionsFromHash,
      highlightYamlLine,
      initLogsFollow,
      syncThemeTogglePostTarget,
      setupStickyNamespace,
      // Chips-editor row model (D7/D20): captured from the full server-rendered
      // document. ORDER CONTRACT: this step must stay BEFORE the windowing
      // init (Unit 24) that prunes rows from the DOM -- at this point
      // the DOM still IS the complete dataset.
      captureRowModelFromDocument,
      // Virtualization engagement (Unit 24/D20): windows the >threshold
      // table the server marked `.ro-windowed`. AFTER the model capture,
      // per the order contract above.
      virtualizeInit,
      // Columns-popover open flag (D8): re-derived from the fresh DOM so a
      // boosted body swap (rendered closed) never leaves a stale-open flag.
      syncColsPopState,
      // Row state is keyed by OBJECT identity; the store clears when an
      // hx-boost navigation swaps the body (the Unit-16 htmx:beforeSwap
      // hook), so this init re-paint scrubs any stale is-selected classes a
      // cached/boosted body carried in -- and the bulk bar re-syncs to the
      // same store right after.
      reapplyRowState,
      updateBulkBar
    ].forEach(runInitStep);
  }
  document.addEventListener("DOMContentLoaded", runInit);
  document.addEventListener("htmx:load", runInit);
  document.addEventListener("htmx:afterSettle", setupStickyNamespace);
  window.addEventListener("resize", setupStickyNamespace);
})();
