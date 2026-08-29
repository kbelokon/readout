"use strict";
(() => {
  // internal/assets/src/js/htmx-config.ts
  if (typeof htmx !== "undefined") {
    htmx.config.globalViewTransitions = false;
  }

  // internal/assets/src/js/filters-parse.ts
  var GO_FIELD_WHITESPACE = /* @__PURE__ */ new Set([
    9,
    10,
    11,
    12,
    13,
    32,
    133,
    160,
    5760,
    8192,
    8193,
    8194,
    8195,
    8196,
    8197,
    8198,
    8199,
    8200,
    8201,
    8202,
    8232,
    8233,
    8239,
    8287,
    12288
  ]);
  function isGoFieldWhitespace(character) {
    return GO_FIELD_WHITESPACE.has(character.charCodeAt(0));
  }
  function trimFilterWhitespace(s) {
    const characters = Array.from(s || "");
    const first = characters.findIndex((character) => !isGoFieldWhitespace(character));
    const last = characters.reduceRight(
      (found, character, index) => found === -1 && !isGoFieldWhitespace(character) ? index : found,
      -1
    );
    return characters.slice(first, last + 1).join("");
  }
  function normalizeFieldWhitespace(s) {
    const normalized = [];
    let pendingSpace = false;
    for (const character of s || "") {
      if (isGoFieldWhitespace(character)) {
        pendingSpace = normalized.length > 0;
        continue;
      }
      if (pendingSpace) normalized.push(" ");
      normalized.push(character);
      pendingSpace = false;
    }
    return normalized.join("");
  }
  function normalizeFieldName(s) {
    return normalizeFieldWhitespace((s || "").toLowerCase().replace(/-/g, " "));
  }
  function fieldSuggestionText(label) {
    return normalizeFieldName(label).replace(/ /g, "-");
  }
  function splitFilterDraft(s) {
    const operator = /!=|[:<>]/.exec(s);
    if (!operator) {
      return null;
    }
    const op = operator[0];
    return {
      field: trimFilterWhitespace(s.slice(0, operator.index)),
      op,
      value: s.slice(operator.index + op.length)
    };
  }
  function hasModelColumn(fields, normName) {
    return fields.some((f) => !!f.hint && normalizeFieldName(f.label) === normName);
  }
  function filterSuggestionFields(fields) {
    const out = [];
    fields.forEach((f) => {
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
    if (hasModelColumn(fields, "cpu usage")) {
      out.push({ text: "cpu", hint: "quantity" });
    }
    if (hasModelColumn(fields, "memory usage")) {
      out.push({ text: "memory", hint: "quantity" });
    }
    return out;
  }
  function filterFieldKnown(fields, field) {
    const want = normalizeFieldName(field);
    if (!want) {
      return false;
    }
    if (want === "label") {
      return true;
    }
    if (want === "cpu" || want === "memory") {
      return hasModelColumn(fields, `${want} usage`);
    }
    return fields.some((f) => !!f.hint && normalizeFieldName(f.label) === want);
  }
  function fieldColumnIndex(fields, field) {
    let want = normalizeFieldName(field);
    if (want === "cpu" || want === "memory") {
      want += " usage";
    }
    for (let i = 0; i < fields.length; i++) {
      const f = fields[i];
      if (f.hint && normalizeFieldName(f.label) === want) {
        return i;
      }
    }
    return -1;
  }
  function rankFieldSuggestions(fields, draft) {
    const q = normalizeFieldName(draft);
    const ranked = [[], []];
    filterSuggestionFields(fields).forEach((field) => {
      const matchAt = normalizeFieldName(field.text).indexOf(q);
      if (matchAt >= 0) {
        ranked[matchAt === 0 ? 0 : 1].push(field);
      }
    });
    return [...ranked[0], ...ranked[1]].map((f) => ({
      label: f.text,
      hint: f.hint,
      insert: `${f.text}:`,
      kind: "field"
    }));
  }
  function rankValueSuggestions(fields, rows, split) {
    const idx = fieldColumnIndex(fields, split.field);
    if (idx < 0) {
      return [];
    }
    const freq = /* @__PURE__ */ new Map();
    rows.forEach((row) => {
      const v = row.cells[idx];
      if (v) {
        freq.set(v, (freq.get(v) || 0) + 1);
      }
    });
    const typed = trimFilterWhitespace(split.value).toLowerCase();
    const entries = Array.from(freq.entries()).filter(
      ([v]) => v.toLowerCase().indexOf(typed) !== -1
    );
    entries.sort((a, b) => b[1] - a[1]);
    return entries.slice(0, 8).map(([v, n]) => ({
      label: v,
      hint: `×${n}`,
      insert: `${trimFilterWhitespace(split.field)}:${v}`,
      kind: "value"
    }));
  }
  function liveNameMatchKeys(rows, draft) {
    const text = !draft || splitFilterDraft(draft) ? "" : trimFilterWhitespace(draft).toLowerCase();
    if (!text) {
      return null;
    }
    const visible = /* @__PURE__ */ new Set();
    rows.forEach((row) => {
      if (row.name.toLowerCase().indexOf(text) !== -1) {
        visible.add(row.key);
      }
    });
    return visible;
  }
  function mergeColParams(pathname, search, owned, fields) {
    const kept = [];
    search.replace(/^\?/, "").split("&").forEach((pair) => {
      if (pair && !owned.has(pair.split("=")[0])) {
        kept.push(pair);
      }
    });
    const query = kept.concat(fields).join("&");
    return pathname + (query ? `?${query}` : "");
  }

  // internal/assets/src/js/list-projection.ts
  var rowModel = {
    fields: [],
    rows: [],
    visibleKeys: null
  };
  function emptySnapshot() {
    return {
      rows: [],
      byKey: /* @__PURE__ */ new Map(),
      indexByKey: /* @__PURE__ */ new Map(),
      order: [],
      cardsByKey: /* @__PURE__ */ new Map(),
      fields: [],
      modelRows: [],
      windowed: false
    };
  }
  var projection = emptySnapshot();
  var prepared = null;
  var projectionRoot = null;
  var projectionRevision = 0;
  window.roRowModel = rowModel;
  function captureFields(table) {
    return Array.from(table.querySelectorAll("thead th")).map((th) => {
      const label = normalizeFieldWhitespace(th.textContent || "");
      return {
        label,
        name: fieldSuggestionText(label),
        hint: th.dataset.hint || ""
      };
    });
  }
  function captureModelRow(tr) {
    const cells = Array.from(tr.querySelectorAll("td")).map(
      (td) => trimFilterWhitespace(td.textContent || "")
    );
    const nameLink = tr.querySelector("td.cell-name a");
    return {
      key: tr.dataset.key,
      name: nameLink ? trimFilterWhitespace(nameLink.textContent || "") : cells[0] || "",
      cells
    };
  }
  function captureModelRows(rows) {
    return rows.map(captureModelRow);
  }
  function captureCards(root, order) {
    const cards = Array.from(root.querySelectorAll(".ro-cardlist > .ro-pcard"));
    const cardsByKey = /* @__PURE__ */ new Map();
    cards.forEach((card) => {
      const key = card.dataset.key;
      if (key) {
        cardsByKey.set(key, card);
      }
    });
    if (cardsByKey.size === 0 && cards.length === order.length) {
      cards.forEach((card, index) => {
        cardsByKey.set(order[index], card);
      });
    }
    return cardsByKey;
  }
  function snapshotFrom(root) {
    const table = root.querySelector("table.ro-table");
    if (!table) {
      return emptySnapshot();
    }
    const tbody = table.tBodies.item(0);
    if (!tbody || tbody.querySelector(":scope > tr.ro-vspacer")) {
      return emptySnapshot();
    }
    const rows = Array.from(tbody.querySelectorAll(":scope > tr[data-key]"));
    const byKey = new Map(rows.map((row) => [row.dataset.key, row]));
    const order = rows.map((row) => row.dataset.key);
    return {
      rows,
      byKey,
      indexByKey: new Map(order.map((key, index) => [key, index])),
      order,
      cardsByKey: captureCards(root, order),
      fields: captureFields(table),
      modelRows: captureModelRows(rows),
      windowed: table.closest(".ro-table-wrap.ro-windowed") !== null
    };
  }
  function publishModel(snapshot) {
    rowModel.fields = snapshot.fields;
    rowModel.rows = snapshot.modelRows;
  }
  function adoptListProjection(root) {
    projection = snapshotFrom(root);
    projectionRoot = root;
    prepared = null;
    projectionRevision += 1;
    publishModel(projection);
    rowModel.visibleKeys = null;
  }
  function ensureListProjection(root) {
    if (projectionRoot === root) {
      return false;
    }
    adoptListProjection(root);
    return true;
  }
  function prepareListProjectionSwap(root) {
    if (prepared?.root !== root) {
      const snapshot = snapshotFrom(root);
      projectionRevision += 1;
      prepared = {
        root,
        snapshot,
        // Snapshot maps are created once and never mutated or exposed. Keep
        // the immutable prior index by reference for the windowed cell diff.
        previousByKey: projection.byKey
      };
      publishModel(snapshot);
    }
    return {
      rows: prepared.snapshot.rows,
      windowed: prepared.snapshot.windowed
    };
  }
  function commitListProjectionSwap() {
    if (!prepared) {
      return null;
    }
    const incoming = prepared;
    prepared = null;
    const content = document.getElementById("resource-list-content");
    if (incoming.snapshot.windowed) {
      projection = incoming.snapshot;
      projectionRoot = content || incoming.root;
    } else {
      projection = content ? snapshotFrom(content) : emptySnapshot();
      projectionRoot = content;
      publishModel(projection);
    }
    return incoming.previousByKey;
  }
  function resetListProjection() {
    projection = emptySnapshot();
    projectionRoot = null;
    prepared = null;
    projectionRevision += 1;
    publishModel(projection);
    rowModel.visibleKeys = null;
  }
  function listProjectionSwapPending() {
    return prepared !== null;
  }
  function listProjectionRevision() {
    return projectionRevision;
  }
  function listProjectionWindowed() {
    return projection.windowed;
  }
  function listProjectionRows() {
    return projection.rows;
  }
  function listProjectionRowByKey(key) {
    return projection.byKey.get(key) || null;
  }
  function listProjectionRowModel() {
    return rowModel;
  }
  function setListProjectionVisibleKeys(keys) {
    rowModel.visibleKeys = keys;
  }
  function listProjectionVisibleRows() {
    const keys = rowModel.visibleKeys;
    return keys ? projection.rows.filter((row) => keys.has(row.dataset.key)) : projection.rows;
  }
  var LIVE_FRAGMENT_BYTES = 128 * 1024;
  var LIVE_DELTA_BYTES = 256 * 1024;
  var LIVE_FRAGMENT_NODES = 4096;
  var LIVE_FRAGMENT_DEPTH = 64;
  var LIVE_FRAGMENT_ATTRIBUTES = 8192;
  var liveTextEncoder = new TextEncoder();
  function projectionError(code, message, fatal = false) {
    return { code, message, fatal };
  }
  function oneElementRoot(parent) {
    let root = null;
    for (const node of parent.childNodes) {
      if (node.nodeType === Node.TEXT_NODE && !node.textContent?.trim()) {
        continue;
      }
      if (node.nodeType !== Node.ELEMENT_NODE || root) {
        return null;
      }
      root = node;
    }
    return root;
  }
  function liveRowDOMID(key) {
    let result = "row-";
    for (const character of key) {
      const code = character.codePointAt(0);
      if (code <= 32 || character === '"' || character === "\\" || character === "%" || code === 127) {
        result += `%${code.toString(16).toUpperCase().padStart(2, "0")}`;
      } else {
        result += character;
      }
    }
    return result;
  }
  function fragmentIntroducesIdentity(root) {
    return root.querySelector("[id], [data-ro-live-region]") !== null;
  }
  function fragmentIsCSPClean(root) {
    const forbiddenElements = "script, style, link, iframe, object, embed, base, meta[http-equiv]";
    let nodes = 0;
    let attributes = 0;
    const pending = [{ node: root, depth: 1 }];
    for (let cursor = 0; cursor < pending.length; cursor += 1) {
      const current = pending[cursor];
      nodes += 1;
      if (nodes > LIVE_FRAGMENT_NODES || current.depth > LIVE_FRAGMENT_DEPTH) {
        return false;
      }
      if (!(current.node instanceof Element)) continue;
      attributes += current.node.attributes.length;
      if (attributes > LIVE_FRAGMENT_ATTRIBUTES || current.node.matches(forbiddenElements)) {
        return false;
      }
      for (const attribute of Array.from(current.node.attributes)) {
        const name = attribute.name.toLowerCase();
        if (name === "style") {
          if (current.node.tagName !== "I" || current.node.parentElement?.classList.contains("cap-bar") !== true || !/^width\s*:\s*(?:100|[0-9]{1,2})%\s*;?$/u.test(attribute.value)) {
            return false;
          }
          continue;
        }
        if (name === "srcdoc" || name.startsWith("on")) {
          return false;
        }
        if (!["href", "src", "xlink:href", "action", "formaction"].includes(name)) {
          continue;
        }
        let normalizedURL = "";
        for (const character of attribute.value) {
          const code = character.codePointAt(0);
          if (code > 32 && !(code >= 127 && code <= 159)) {
            normalizedURL += character;
          }
        }
        if (/^(?:(?:javascript|vbscript):|data:text\/html)/iu.test(normalizedURL)) {
          return false;
        }
      }
      for (const child of Array.from(current.node.childNodes)) {
        pending.push({ node: child, depth: current.depth + 1 });
      }
    }
    return true;
  }
  function parseRowFragment(html, key) {
    try {
      const tbody = document.createElement("tbody");
      tbody.innerHTML = html;
      const row = oneElementRoot(tbody);
      if (row?.tagName !== "TR" || row.dataset.key !== key || row.id !== liveRowDOMID(key) || row.hasAttribute("data-ro-live-region") || row.classList.contains("ro-vspacer") || fragmentIntroducesIdentity(row) || !fragmentIsCSPClean(row)) {
        return projectionError(
          "fragment-invalid",
          `row fragment for ${key} is not one canonical keyed tr`
        );
      }
      return row;
    } catch {
      return projectionError("fragment-invalid", `row fragment for ${key} cannot be parsed`);
    }
  }
  function parseCardFragment(html, key) {
    try {
      const template = document.createElement("template");
      template.innerHTML = html;
      const card = oneElementRoot(template.content);
      if (card?.tagName !== "DIV" || !card.matches(".ro-pcard[data-key]") || card.dataset.key !== key || card.hasAttribute("id") || card.hasAttribute("data-ro-live-region") || fragmentIntroducesIdentity(card) || !fragmentIsCSPClean(card)) {
        return projectionError(
          "fragment-invalid",
          `card fragment for ${key} is not one canonical keyed card`
        );
      }
      return card;
    } catch {
      return projectionError("fragment-invalid", `card fragment for ${key} cannot be parsed`);
    }
  }
  function parseRegionFragment(update) {
    try {
      const mounts = document.querySelectorAll(
        `[data-ro-live-region="${update.region}"]`
      );
      if (mounts.length !== 1) {
        return projectionError(
          "projection-mismatch",
          `region ${update.region} does not have exactly one fixed mount`
        );
      }
      const template = document.createElement("template");
      template.innerHTML = update.html;
      const incoming = oneElementRoot(template.content);
      const current = mounts.item(0);
      const expectedTag = update.region === "phase" ? "DIV" : "SPAN";
      const expectedClass = update.region === "count" ? "ro-count" : update.region === "phase" ? "ro-phase-strip" : "ro-foundline";
      if (!incoming || incoming.tagName !== expectedTag || !incoming.classList.contains(expectedClass) || incoming.dataset.roLiveRegion !== update.region || incoming.hasAttribute("id") || current.tagName !== expectedTag || !current.classList.contains(expectedClass) || incoming.querySelector("[id], [data-ro-live-region]") !== null || !fragmentIsCSPClean(incoming)) {
        return projectionError(
          "fragment-invalid",
          `region ${update.region} is not one canonical fixed-region root`
        );
      }
      return { current, incoming };
    } catch {
      return projectionError("fragment-invalid", `region ${update.region} cannot be parsed`);
    }
  }
  function arraysEqual(left, right) {
    return left.length === right.length && left.every((value, index) => value === right[index]);
  }
  function validateDeltaHTMLBounds(plan) {
    let aggregate = 0;
    const fragments = [
      ...plan.upsert.flatMap(
        (operation) => operation.card === void 0 ? [operation.row] : [operation.row, operation.card]
      ),
      ...plan.regions.map((operation) => operation.html)
    ];
    for (const html of fragments) {
      if (html.length > LIVE_FRAGMENT_BYTES) {
        return projectionError("limit-exceeded", "Live delta fragment exceeds its limit");
      }
      const bytes = liveTextEncoder.encode(html).byteLength;
      if (bytes > LIVE_FRAGMENT_BYTES) {
        return projectionError("limit-exceeded", "Live delta fragment exceeds its limit");
      }
      aggregate += bytes;
      if (aggregate > LIVE_DELTA_BYTES) {
        return projectionError(
          "limit-exceeded",
          "Live delta fragments exceed their aggregate limit"
        );
      }
    }
    return null;
  }
  function currentProjectionMode() {
    if (projection.rows.length === 0) {
      return projectionError("projection-mismatch", "empty projections require a snapshot");
    }
    if (projection.windowed) {
      return projection.cardsByKey.size === 0 ? "windowed" : projectionError("projection-mismatch", "windowed projection unexpectedly has cards");
    }
    if (projection.cardsByKey.size === projection.rows.length) {
      return "cards";
    }
    return projectionError("projection-mismatch", "projection mode is not delta-capable");
  }
  function validateCurrentProjection(content, fastPath) {
    if (prepared || projectionRoot !== content || !content.isConnected) {
      return projectionError("projection-mismatch", "canonical projection is not stably mounted");
    }
    const mode = currentProjectionMode();
    if (typeof mode !== "string") return mode;
    const tables = content.querySelectorAll("table.ro-table");
    const tbody = tables.length === 1 ? tables[0].tBodies.item(0) : null;
    if (!tbody) {
      return projectionError("projection-mismatch", "projection table mount is ambiguous");
    }
    if (!fastPath) {
      const orderSet = new Set(projection.order);
      if (orderSet.size !== projection.order.length || projection.rows.length !== projection.order.length || projection.modelRows.length !== projection.order.length || projection.order.some((key, index) => {
        const row = projection.rows[index];
        const model = projection.modelRows[index];
        return !row || !model || row.dataset.key !== key || row.id !== liveRowDOMID(key) || model.key !== key;
      })) {
        return projectionError(
          "projection-mismatch",
          "canonical projection invariants are broken"
        );
      }
    }
    let cardMount = null;
    if (mode === "cards") {
      const mounts = content.querySelectorAll(".ro-cardlist");
      if (mounts.length !== 1) {
        return projectionError("projection-mismatch", "card mount is ambiguous");
      }
      cardMount = mounts[0];
      if (!fastPath && projection.order.some((key) => {
        const card = projection.cardsByKey.get(key);
        return !card || card.dataset.key !== key || card.parentElement !== cardMount;
      })) {
        return projectionError(
          "projection-mismatch",
          "canonical keyed-card invariants are broken"
        );
      }
      if (!fastPath && projection.rows.some((row) => row.parentElement !== tbody)) {
        return projectionError("projection-mismatch", "small-list rows are not fully mounted");
      }
    } else if (!fastPath && projection.rows.some((row) => row.isConnected && row.parentElement !== tbody)) {
      return projectionError("projection-mismatch", "windowed rows are mounted outside tbody");
    }
    return { mode, tbody, cardMount };
  }
  function prepareDelta(plan) {
    const boundsError = validateDeltaHTMLBounds(plan);
    if (boundsError) return boundsError;
    const fastPath = plan.remove.length === 0 && plan.order === void 0 && plan.upsert.every((operation) => projection.byKey.has(operation.key));
    const content = document.getElementById("resource-list-content");
    if (!content) {
      return projectionError("projection-mismatch", "resource list content is missing");
    }
    const current = validateCurrentProjection(content, fastPath);
    if ("code" in current) return current;
    const removed = /* @__PURE__ */ new Set();
    let deleted = 0;
    let projected = 0;
    for (const operation of plan.remove) {
      if (removed.has(operation.key)) {
        return projectionError(
          "projection-mismatch",
          `remove key ${operation.key} is duplicate`
        );
      }
      if (!projection.byKey.has(operation.key)) {
        return projectionError("projection-mismatch", `remove key ${operation.key} is absent`);
      }
      removed.add(operation.key);
      if (operation.cause === "delete") deleted += 1;
      else projected += 1;
    }
    const parsedRows = /* @__PURE__ */ new Map();
    const parsedCards = /* @__PURE__ */ new Map();
    let inserted = 0;
    let updated = 0;
    for (const operation of plan.upsert) {
      if (parsedRows.has(operation.key) || removed.has(operation.key)) {
        return projectionError(
          "projection-mismatch",
          `upsert key ${operation.key} is duplicate or also removed`
        );
      }
      const row = parseRowFragment(operation.row, operation.key);
      if (!("dataset" in row)) return row;
      const existingRow = projection.byKey.get(operation.key);
      if (existingRow && existingRow.id !== row.id) {
        return projectionError(
          "fragment-invalid",
          `row fragment for ${operation.key} changed its canonical id`
        );
      }
      if (existingRow) {
        const index = projection.indexByKey.get(operation.key);
        const model = index === void 0 ? void 0 : projection.modelRows[index];
        if (index === void 0 || projection.rows[index] !== existingRow || !model || model.key !== operation.key || existingRow.isConnected && existingRow.parentElement !== current.tbody) {
          return projectionError(
            "projection-mismatch",
            `row ${operation.key} is not at its canonical index`
          );
        }
      }
      const globalMatches = document.querySelectorAll(`[id="${row.id}"]`);
      if (existingRow?.isConnected && globalMatches.length !== 1 || existingRow && !existingRow.isConnected && globalMatches.length !== 0 || !existingRow && globalMatches.length !== 0) {
        return projectionError(
          "fragment-invalid",
          `row fragment for ${operation.key} collides with a document id`
        );
      }
      parsedRows.set(operation.key, row);
      if (current.mode === "cards") {
        if (operation.card === void 0) {
          return projectionError(
            "fragment-invalid",
            `card-mode upsert ${operation.key} is missing its card`
          );
        }
        const card = parseCardFragment(operation.card, operation.key);
        if (!("dataset" in card)) return card;
        const existingCard = projection.cardsByKey.get(operation.key);
        if (existingRow && (!existingCard || existingCard.dataset.key !== operation.key || existingCard.parentElement !== current.cardMount)) {
          return projectionError(
            "projection-mismatch",
            `card ${operation.key} is not canonically mounted`
          );
        }
        parsedCards.set(operation.key, card);
      } else if (operation.card !== void 0) {
        return projectionError(
          "fragment-invalid",
          `windowed upsert ${operation.key} unexpectedly carries a card`
        );
      }
      if (projection.byKey.has(operation.key)) updated += 1;
      else inserted += 1;
    }
    const parsedRegions = /* @__PURE__ */ new Map();
    for (const update of plan.regions) {
      if (parsedRegions.has(update.region)) {
        return projectionError("projection-mismatch", `region ${update.region} is duplicate`);
      }
      const parsed = parseRegionFragment(update);
      if ("code" in parsed) return parsed;
      parsedRegions.set(update.region, parsed);
    }
    if (fastPath) {
      const modelUpdates = /* @__PURE__ */ new Map();
      for (const [key, incoming] of parsedRows) {
        modelUpdates.set(projection.indexByKey.get(key), captureModelRow(incoming));
      }
      return {
        candidate: { ...projection },
        fastPath,
        modelUpdates,
        parsedRows,
        parsedCards,
        parsedRegions,
        summary: {
          inserted: 0,
          updated,
          deleted: 0,
          projected: 0,
          reordered: false,
          regions: [...parsedRegions.keys()]
        },
        tbody: current.tbody,
        cardMount: current.cardMount
      };
    }
    const finalKeys = new Set(projection.order.filter((key) => !removed.has(key)));
    for (const key of parsedRows.keys()) finalKeys.add(key);
    if (finalKeys.size === 0) {
      return projectionError(
        "projection-mismatch",
        "empty projection boundary requires snapshot"
      );
    }
    if (plan.order === void 0) {
      return projectionError(
        "projection-mismatch",
        "topology-changing delta requires full order"
      );
    }
    const finalOrder = [...plan.order];
    if (finalOrder.length !== finalKeys.size || new Set(finalOrder).size !== finalOrder.length || finalOrder.some((key) => !finalKeys.has(key))) {
      return projectionError("projection-mismatch", "delta order is not the exact final key set");
    }
    if (arraysEqual(plan.order, projection.order)) {
      return projectionError("projection-mismatch", "redundant unchanged order is not allowed");
    }
    const candidateByKey = new Map(projection.byKey);
    const candidateCards = new Map(projection.cardsByKey);
    for (const key of removed) {
      candidateByKey.delete(key);
      candidateCards.delete(key);
    }
    for (const [key, incoming] of parsedRows) {
      const old = projection.byKey.get(key);
      candidateByKey.set(key, old?.isConnected ? old : incoming);
    }
    for (const [key, incoming] of parsedCards) {
      const old = projection.cardsByKey.get(key);
      candidateCards.set(key, old?.isConnected ? old : incoming);
    }
    const rows = finalOrder.map((key) => candidateByKey.get(key));
    const modelByKey = new Map(projection.modelRows.map((model) => [model.key, model]));
    for (const [key, incoming] of parsedRows) modelByKey.set(key, captureModelRow(incoming));
    const modelRows = finalOrder.map((key) => modelByKey.get(key));
    for (const key of finalOrder) {
      const row = candidateByKey.get(key);
      const globalMatches = document.querySelectorAll(`[id="${row.id}"]`);
      if (row.isConnected && globalMatches.length !== 1 || !row.isConnected && globalMatches.length !== 0) {
        return projectionError(
          "fragment-invalid",
          `final row ${key} collides with a document id`
        );
      }
    }
    return {
      candidate: {
        rows,
        byKey: candidateByKey,
        indexByKey: new Map(finalOrder.map((key, index) => [key, index])),
        order: finalOrder,
        cardsByKey: candidateCards,
        fields: projection.fields.map((field) => ({ ...field })),
        modelRows,
        windowed: projection.windowed
      },
      fastPath,
      modelUpdates: /* @__PURE__ */ new Map(),
      parsedRows,
      parsedCards,
      parsedRegions,
      summary: {
        inserted,
        updated,
        deleted,
        projected,
        reordered: !arraysEqual(finalOrder, projection.order),
        regions: [...parsedRegions.keys()]
      },
      tbody: current.tbody,
      cardMount: current.cardMount
    };
  }
  function addParentJournal(entries, parent) {
    if (parent) {
      entries.set(parent, { parent, children: Array.from(parent.childNodes) });
    }
  }
  function addElementJournal(entries, element) {
    entries.push({ state: captureDOMNode(element) });
  }
  function addPlacementJournal(entries, node) {
    entries.push({
      node,
      parent: node.parentNode,
      parentWasConnected: node.parentNode?.isConnected === true,
      nextSibling: node.nextSibling
    });
  }
  function captureDOMNode(node) {
    return {
      node,
      nodeValue: node.nodeValue,
      attributes: node instanceof Element ? Array.from(node.attributes, (attribute) => ({
        name: attribute.name,
        value: attribute.value
      })) : null,
      children: Array.from(node.childNodes, captureDOMNode)
    };
  }
  function addAttributeJournal(entries, element, name) {
    entries.push({ element, name, value: element.getAttribute(name) });
  }
  function createDOMJournal(parsed) {
    const parents = /* @__PURE__ */ new Map();
    const placements = [];
    const elements = [];
    const attributes = [];
    if (!parsed.fastPath || projection.windowed) {
      addParentJournal(parents, parsed.tbody);
    }
    parsed.tbody.querySelectorAll(":scope > tr.ro-vspacer").forEach((spacer) => {
      addElementJournal(elements, spacer);
    });
    if (parsed.cardMount && !parsed.fastPath) {
      addParentJournal(parents, parsed.cardMount);
    }
    for (const key of parsed.parsedRows.keys()) {
      const current = projection.byKey.get(key);
      if (current) {
        if (parsed.fastPath) {
          addPlacementJournal(placements, current);
        }
        addElementJournal(elements, current);
      }
    }
    for (const key of parsed.parsedCards.keys()) {
      const current = projection.cardsByKey.get(key);
      if (current) {
        if (parsed.fastPath) {
          addPlacementJournal(placements, current);
        }
        addElementJournal(elements, current);
      }
    }
    for (const { current } of parsed.parsedRegions.values()) {
      addParentJournal(parents, current.parentNode);
      addElementJournal(elements, current);
    }
    if (!parsed.fastPath) {
      for (const element of [...projection.rows, ...projection.cardsByKey.values()]) {
        addAttributeJournal(attributes, element, "class");
      }
    }
    document.querySelectorAll(".ro-table-wrap").forEach((wrap) => {
      addAttributeJournal(attributes, wrap, "aria-activedescendant");
    });
    const status = document.getElementById("ro-live-status");
    if (status) {
      addParentJournal(parents, status.parentNode);
      addElementJournal(elements, status);
    }
    const bulk = document.getElementById("ro-bulkbar");
    if (bulk) {
      addParentJournal(parents, bulk.parentNode);
      addElementJournal(elements, bulk);
    }
    return { parents: [...parents.values()], placements, elements, attributes };
  }
  function restoreDOMNode(state) {
    const { node } = state;
    if (node instanceof Element && state.attributes) {
      for (const attribute of Array.from(node.attributes)) node.removeAttribute(attribute.name);
      for (const attribute of state.attributes) {
        node.setAttribute(attribute.name, attribute.value);
      }
    } else {
      node.nodeValue = state.nodeValue;
    }
    if (node instanceof Element) {
      node.replaceChildren(...state.children.map((child) => child.node));
    }
    for (const child of state.children) restoreDOMNode(child);
  }
  function verifyDOMNode(state) {
    const { node } = state;
    if (!(node instanceof Element)) return node.nodeValue === state.nodeValue;
    const attributes = state.attributes;
    if (node.attributes.length !== attributes.length) return false;
    for (const attribute of attributes) {
      if (node.getAttribute(attribute.name) !== attribute.value) return false;
    }
    const children = Array.from(node.childNodes);
    if (children.length !== state.children.length) return false;
    for (let index = 0; index < children.length; index += 1) {
      const childState = state.children[index];
      if (children[index] !== childState.node || !verifyDOMNode(childState)) return false;
    }
    return true;
  }
  function restorePlacementJournal(entries) {
    const byNode = new Map(entries.map((entry) => [entry.node, entry]));
    const restored = /* @__PURE__ */ new Set();
    const restore = (entry) => {
      if (restored.has(entry.node)) return;
      if (!entry.parent) {
        entry.node.parentNode?.removeChild(entry.node);
      } else {
        if (entry.parentWasConnected && !entry.parent.isConnected) {
          throw new Error();
        }
        if (entry.nextSibling) {
          const dependency = byNode.get(entry.nextSibling);
          if (dependency) restore(dependency);
        }
        entry.parent.insertBefore(entry.node, entry.nextSibling);
      }
      restored.add(entry.node);
    };
    for (const entry of entries) restore(entry);
    if (entries.some(
      ({ node, parent, nextSibling }) => node.parentNode !== parent || node.nextSibling !== nextSibling
    )) {
      throw new Error();
    }
  }
  function restoreDOMJournal(journal) {
    if (journal.parents.some(
      ({ parent }) => parent instanceof Element && parent.isConnected === false
    )) {
      throw new Error();
    }
    for (const { parent, children } of journal.parents) parent.replaceChildren(...children);
    restorePlacementJournal(journal.placements);
    for (const { state } of journal.elements) restoreDOMNode(state);
    for (const { element, name, value } of journal.attributes) {
      if (value === null) element.removeAttribute(name);
      else element.setAttribute(name, value);
    }
    for (const { parent, children } of journal.parents) {
      const restored = Array.from(parent.childNodes);
      if (restored.length !== children.length || restored.some((child, index) => child !== children[index])) {
        throw new Error();
      }
    }
    if (journal.elements.some(({ state }) => !verifyDOMNode(state))) {
      throw new Error();
    }
  }
  function resolveMorph(override) {
    if (override) return override;
    if (typeof Idiomorph === "undefined" || typeof Idiomorph.morph !== "function") return null;
    return (current, incoming) => {
      Idiomorph.morph(current, incoming, {
        morphStyle: "outerHTML",
        ignoreActiveValue: true
      });
    };
  }
  function canonicalMorphClone(element) {
    const clone = element.cloneNode(true);
    for (const current of [clone, ...Array.from(clone.querySelectorAll("*"))]) {
      current.classList.remove("is-selected", "kfocus", "ro-row-filtered", "ro-cell-changed");
      if (current.getAttribute("class") === "") current.removeAttribute("class");
    }
    return clone;
  }
  function morphLandedCanonical(current, incoming) {
    return canonicalMorphClone(current).isEqualNode(canonicalMorphClone(incoming));
  }
  function runScopedMorph(morph, current, incoming) {
    const parent = current.parentNode;
    const next = current.nextSibling;
    const connected = current.isConnected;
    const outcome = morph(current, incoming);
    if (!connected) {
      if (parent) {
        parent.insertBefore(current, next);
      } else {
        current.remove();
      }
    }
    if (current.isConnected !== connected || current.parentNode !== parent || current.nextSibling !== next || outcome === false || !morphLandedCanonical(current, incoming)) {
      throw new Error();
    }
  }
  function updateLiveStatus(summary) {
    const status = document.getElementById("ro-live-status");
    if (!status) return;
    const changed = summary.inserted + summary.updated + summary.deleted + summary.projected;
    const regionCount = summary.regions.length;
    const parts = [];
    if (changed > 0) parts.push(`${changed} row${changed === 1 ? "" : "s"}`);
    if (summary.reordered) parts.push("order changed");
    if (regionCount > 0) {
      parts.push(`${regionCount} region${regionCount === 1 ? "" : "s"}`);
    }
    status.textContent = `Live update: ${parts.join(", ")}`;
  }
  function applyListProjectionDeltaTransaction(plan, options) {
    let parsed;
    try {
      parsed = prepareDelta(plan);
    } catch {
      return {
        ok: false,
        error: projectionError("projection-mismatch", "Live delta preflight failed")
      };
    }
    if ("code" in parsed) return { ok: false, error: parsed };
    const morphNeeded = [...parsed.parsedRows.keys()].some(
      (key) => parsed.candidate.byKey.get(key) === projection.byKey.get(key)
    ) || parsed.parsedRegions.size > 0;
    const morph = resolveMorph(options.morph);
    if (morphNeeded && !morph) {
      return {
        ok: false,
        error: projectionError("morph-failed", "Idiomorph is unavailable")
      };
    }
    const oldProjection = projection;
    const oldPrepared = prepared;
    const oldRoot = projectionRoot;
    const oldRevision = projectionRevision;
    const oldFields = rowModel.fields;
    const oldModelRows = rowModel.rows;
    const oldVisibleKeys = rowModel.visibleKeys;
    const modelPatchJournal = [];
    let journal;
    try {
      journal = createDOMJournal(parsed);
    } catch {
      return {
        ok: false,
        error: projectionError("projection-mismatch", "Live delta journal could not be built")
      };
    }
    let mutationPhase = "morph";
    try {
      for (const [key, incoming] of parsed.parsedRows) {
        const current = oldProjection.byKey.get(key);
        if (current && parsed.candidate.byKey.get(key) === current) {
          runScopedMorph(morph, current, incoming);
        }
      }
      for (const [key, incoming] of parsed.parsedCards) {
        const current = oldProjection.cardsByKey.get(key);
        if (current) {
          runScopedMorph(morph, current, incoming);
        }
      }
      for (const { current, incoming } of parsed.parsedRegions.values()) {
        runScopedMorph(morph, current, incoming);
      }
      if (!oldProjection.windowed && !parsed.fastPath) {
        parsed.tbody.replaceChildren(...parsed.candidate.rows);
        parsed.cardMount.replaceChildren(
          ...parsed.candidate.order.map(
            (key) => parsed.candidate.cardsByKey.get(key)
          )
        );
      }
      for (const [index, model] of parsed.modelUpdates) {
        modelPatchJournal.push({ index, model: parsed.candidate.modelRows[index] });
        parsed.candidate.modelRows[index] = model;
      }
      projection = parsed.candidate;
      projectionRoot = document.getElementById("resource-list-content");
      prepared = null;
      projectionRevision = oldRevision + 1;
      publishModel(projection);
      mutationPhase = "reconcile";
      options.reconcile();
      updateLiveStatus(parsed.summary);
      return {
        ok: true,
        summary: parsed.summary
      };
    } catch {
      let rollbackFailed = false;
      try {
        restoreDOMJournal(journal);
      } catch {
        rollbackFailed = true;
      }
      projection = oldProjection;
      for (const patch of modelPatchJournal) oldProjection.modelRows[patch.index] = patch.model;
      prepared = oldPrepared;
      projectionRoot = oldRoot;
      projectionRevision = oldRevision;
      rowModel.fields = oldFields;
      rowModel.rows = oldModelRows;
      rowModel.visibleKeys = oldVisibleKeys;
      try {
        options.restoreExternalState();
      } catch {
        rollbackFailed = true;
      }
      if (rollbackFailed) {
        return {
          ok: false,
          error: projectionError(
            "rollback-failed",
            "Live delta rollback could not restore the original mounts",
            true
          )
        };
      }
      return {
        ok: false,
        error: projectionError(
          mutationPhase === "morph" ? "morph-failed" : "reconcile-failed",
          mutationPhase === "morph" ? "Live delta DOM morph failed and was rolled back" : "Live delta reconcile failed and was rolled back"
        )
      };
    }
  }

  // internal/assets/src/js/list-etag.ts
  var LIST_CONTENT_ID = "resource-list-content";
  var ETAG_DATA_KEY = "roEtag";
  var PATH_DATA_KEY = "roEtagPath";
  function eventDetail(event) {
    return Object(event.detail);
  }
  function currentListContent() {
    return document.getElementById(LIST_CONTENT_ID);
  }
  function validETag(value) {
    return typeof value === "string" && /^(?:W\/)?"[\x21\x23-\x7e]*"$/.test(value);
  }
  function tableRequestKey(value) {
    if (typeof value !== "string" || value.length === 0) {
      return null;
    }
    try {
      const url = new URL(value, window.location.href);
      if (url.origin !== window.location.origin || !url.pathname.endsWith("/_table")) {
        return null;
      }
      return `${url.pathname}${url.search}`;
    } catch {
      return null;
    }
  }
  function headerRecord(value) {
    return Object(value);
  }
  function headerValue(headers, wanted) {
    const lowerWanted = wanted.toLowerCase();
    for (const [name, value] of Object.entries(headerRecord(headers))) {
      if (name.toLowerCase() === lowerWanted && typeof value === "string") {
        return value;
      }
    }
    return null;
  }
  function deleteHeader(headers, unwanted) {
    const record = headerRecord(headers);
    const lowerUnwanted = unwanted.toLowerCase();
    for (const name of Object.keys(record)) {
      if (name.toLowerCase() === lowerUnwanted) {
        delete record[name];
      }
    }
  }
  function responseHeader(xhr, name) {
    const candidate = Object(xhr);
    if (typeof candidate.getResponseHeader !== "function") {
      return null;
    }
    try {
      const value = candidate.getResponseHeader.call(xhr, name);
      return typeof value === "string" ? value : null;
    } catch {
      return null;
    }
  }
  function clearContentValidator(content) {
    delete content.dataset[ETAG_DATA_KEY];
    delete content.dataset[PATH_DATA_KEY];
  }
  function readContentValidator(content) {
    const etag = content.dataset[ETAG_DATA_KEY];
    const path = content.dataset[PATH_DATA_KEY];
    if (!validETag(etag) || tableRequestKey(path) !== path) {
      clearContentValidator(content);
      return null;
    }
    return { etag, path };
  }
  function writeContentValidator(content, validator) {
    content.dataset[PATH_DATA_KEY] = validator.path;
    content.dataset[ETAG_DATA_KEY] = validator.etag;
  }
  function configureListValidatorRequest(event) {
    const detail = eventDetail(event);
    const content = currentListContent();
    if (!content) {
      return;
    }
    const sourceIsContent = detail.elt === content;
    const targetIsContent = detail.target === content;
    if (!sourceIsContent && !targetIsContent) {
      return;
    }
    const headers = headerRecord(detail.headers);
    deleteHeader(headers, "If-None-Match");
    if (!sourceIsContent || detail.target !== void 0 && !targetIsContent || headerValue(headers, "RO-No-Push") !== "true" || headerValue(headers, "HX-Preloaded") === "true") {
      return;
    }
    if (content.childElementCount === 0) {
      clearContentValidator(content);
      return;
    }
    const validator = readContentValidator(content);
    const requestKey = tableRequestKey(detail.path);
    if (!validator || requestKey !== validator.path) {
      return;
    }
    headers["If-None-Match"] = validator.etag;
  }
  function rememberListValidator(event) {
    const detail = eventDetail(event);
    const content = currentListContent();
    if (!content || detail.target !== content) {
      return;
    }
    if (detail.roLivePush === true) {
      clearContentValidator(content);
      return;
    }
    const xhr = Object(detail.xhr);
    if (xhr.status !== 200) {
      return;
    }
    const pathInfo = Object(detail.pathInfo);
    const path = tableRequestKey(pathInfo.finalRequestPath);
    const etag = responseHeader(detail.xhr, "ETag");
    if (!path || !validETag(etag)) {
      clearContentValidator(content);
      return;
    }
    writeContentValidator(content, { etag, path });
  }
  function clearListValidator() {
    const content = currentListContent();
    if (content) {
      clearContentValidator(content);
    }
  }
  function suppressListNotModified(event) {
    const detail = eventDetail(event);
    const content = currentListContent();
    const xhr = Object(detail.xhr);
    if (!content || detail.target !== content || xhr.status !== 304) {
      return false;
    }
    detail.shouldSwap = false;
    detail.isError = true;
    const validator = readContentValidator(content);
    const config = Object(detail.requestConfig);
    const pathInfo = Object(detail.pathInfo);
    if (config.elt !== content || !validator || headerValue(config.headers, "RO-No-Push") !== "true" || headerValue(config.headers, "If-None-Match") !== validator.etag || tableRequestKey(pathInfo.finalRequestPath) !== validator.path || responseHeader(detail.xhr, "ETag") !== validator.etag) {
      return false;
    }
    detail.isError = false;
    return true;
  }

  // internal/assets/src/js/live-policy.ts
  function effectivePollSeconds(mode, intervalSeconds, liveFallbackSeconds2) {
    if (intervalSeconds > 0) {
      return intervalSeconds;
    }
    return mode === "Live" ? liveFallbackSeconds2 : 0;
  }
  function refreshDelaySeconds(effectiveSeconds, failureStage) {
    const baseSeconds = Math.max(effectiveSeconds, 0);
    if (failureStage <= 1) {
      return baseSeconds;
    }
    const factor = failureStage === 2 ? 2 : 4;
    return Math.min(baseSeconds * factor, 60);
  }
  function nextFailureStage(stage) {
    return Math.min(stage + 1, 3);
  }
  function shouldDiscardPush(facts) {
    return facts.frameGeneration !== facts.currentGeneration || facts.liveStreamBase !== facts.openedStreamBase;
  }

  // internal/assets/src/js/filters.ts
  function getHtmx() {
    return window.htmx;
  }
  var roRowModel = listProjectionRowModel();
  function captureRowModelFromDocument() {
    const content = document.getElementById("resource-list-content");
    if (content && !virtualizerActive()) {
      ensureListProjection(content);
    }
  }
  var FILTER_HIDE_CLASS = "ro-row-filtered";
  var appliedLiveFilter = null;
  function takeLiveNameFilterCheckpoint() {
    return { applied: appliedLiveFilter };
  }
  function restoreLiveNameFilterCheckpoint(checkpoint) {
    appliedLiveFilter = checkpoint.applied;
  }
  function applyLiveNameFilter() {
    const content = document.getElementById("resource-list-content");
    if (!content) {
      return;
    }
    const input = document.getElementById("ro-filter-input");
    const draft = input ? input.value : "";
    const revision = listProjectionRevision();
    if (appliedLiveFilter?.content === content && appliedLiveFilter.draft === draft && appliedLiveFilter.revision === revision) {
      return;
    }
    const visible = liveNameMatchKeys(roRowModel.rows, draft);
    setListProjectionVisibleKeys(visible);
    content.querySelectorAll("tbody tr[data-key], .ro-cardlist > .ro-pcard[data-key]").forEach((item) => {
      item.classList.toggle(
        FILTER_HIDE_CLASS,
        !!visible && !visible.has(item.dataset.key)
      );
    });
    virtualizeOnFilterChange();
    appliedLiveFilter = { content, draft, revision };
  }
  function issueFilterNavigation(href) {
    const content = document.getElementById("resource-list-content");
    const input = document.getElementById("ro-filter-input");
    const htmx2 = getHtmx();
    if (!content || !input || !htmx2) {
      window.location.assign(href);
      return;
    }
    const u = new URL(href, window.location.href);
    const partial = `${u.pathname.replace(/\/+$/, "")}/_table${u.search}`;
    const request = htmx2.ajax("GET", partial, {
      source: input,
      target: "#resource-list-content",
      swap: "morph"
    });
    void request?.catch(() => {
    });
  }
  function commitFilterChip(draft) {
    const text = trimFilterWhitespace(draft);
    const parsed = splitFilterDraft(text);
    if (!parsed) {
      return;
    }
    if (!filterFieldKnown(roRowModel.fields, parsed.field)) {
      showFilterFieldHint();
      return;
    }
    const raw = encodeURIComponent(text).replace(/%2C/gi, ",");
    const search = window.location.search;
    const href = `${window.location.pathname + (search ? `${search}&` : "?")}f=${raw}`;
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
    const names = filterSuggestionFields(roRowModel.fields).slice(0, 3).map((f) => f.text);
    el.textContent = `no such field — try ${names.join(", ")}…`;
    el.hidden = false;
  }
  function hideFilterFieldHint() {
    const el = document.getElementById("ro-filter-error");
    if (el) {
      el.hidden = true;
    }
  }
  var filterACItems = [];
  var filterACActive = 0;
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
    filterACActive = 0;
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
      row.className = `ro-ac-item${idx === 0 ? " active" : ""}`;
      row.dataset.roAction = "pick-suggestion";
      row.setAttribute("role", "option");
      row.setAttribute("aria-selected", idx === 0 ? "true" : "false");
      row.dataset.acIndex = String(idx);
      const name = document.createElement("span");
      name.className = "ac-name";
      name.textContent = item.label;
      row.appendChild(name);
      const hint = document.createElement("span");
      hint.className = "ac-hint";
      hint.textContent = item.hint;
      row.appendChild(hint);
      row.addEventListener("mousemove", () => setFilterACActive(idx));
      ac.appendChild(row);
    });
    ac.hidden = false;
  }
  function setFilterACActive(index) {
    filterACActive = Math.max(0, Math.min(filterACItems.length - 1, index));
    const ac = document.getElementById("ro-filter-ac");
    if (!ac) {
      return;
    }
    ac.querySelectorAll('[data-ro-action="pick-suggestion"]').forEach((el) => {
      const on = Number(el.dataset.acIndex) === filterACActive;
      el.classList.toggle("active", on);
      el.setAttribute("aria-selected", on ? "true" : "false");
    });
  }
  function moveFilterACActive(delta) {
    setFilterACActive((filterACActive + delta + filterACItems.length) % filterACItems.length);
  }
  function updateFilterAC() {
    const input = document.getElementById("ro-filter-input");
    if (!input) {
      return;
    }
    const draft = input.value;
    if (!trimFilterWhitespace(draft)) {
      closeFilterAC();
      return;
    }
    const parsed = splitFilterDraft(draft);
    if (!parsed) {
      openFilterAC(rankFieldSuggestions(roRowModel.fields, draft));
      return;
    }
    if (parsed.op !== ":" || !filterFieldKnown(roRowModel.fields, parsed.field)) {
      closeFilterAC();
      return;
    }
    openFilterAC(rankValueSuggestions(roRowModel.fields, roRowModel.rows, parsed));
  }
  function acceptFilterAC(commitValues) {
    const input = document.getElementById("ro-filter-input");
    const item = filterACItems[filterACActive];
    if (!input || !item) {
      return;
    }
    input.value = item.insert;
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
      if (filterACOpen() && filterACItems.length > 0) {
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
  var filtersBindings = [
    // Chips editor: a chip's ✕ is a real link (no-JS fallback) whose href is
    // the server-built removal URL; intercept it to ride the v2 partial loop
    // (morph + canonical push) instead of a full navigation.
    {
      event: "click",
      selector: '#ro-filter-field [data-ro-action="remove-chip"]',
      handler: (event, matched) => {
        event.preventDefault();
        const href = matched.getAttribute("href");
        if (href) {
          issueFilterNavigation(href);
        }
        return true;
      },
      stop: true
    },
    // Autocomplete row: clicking accepts it (a complete value commits the chip, a
    // field fills `field:` and opens the value suggestions).
    {
      event: "click",
      selector: '#ro-filter-ac [data-ro-action="pick-suggestion"]',
      handler: (event, matched) => {
        event.preventDefault();
        setFilterACActive(Number(matched.dataset.acIndex) || 0);
        acceptFilterAC(true);
        const input = document.getElementById("ro-filter-input");
        if (input) {
          input.focus();
        }
        return true;
      },
      stop: true
    },
    // Clicking the editor field anywhere (the padding, a chip's text) lands the
    // caret in the input -- the whole field reads as one input.
    {
      event: "click",
      selector: "#ro-filter-field",
      handler: (event, matched) => {
        const input = document.getElementById("ro-filter-input");
        if (input && event.target !== input) {
          input.focus();
        }
        void matched;
        return true;
      },
      stop: true
    },
    // C5: a click anywhere outside the editor dismisses the dropdown
    // (esc-equivalent). Independent of the others (listener-inventory C5). No
    // selector (it keys off the closest() escape).
    {
      event: "click",
      handler: (event) => {
        if (!event.target.closest("#ro-filter-field")) {
          closeFilterAC();
        }
      }
    },
    // Chips editor: every keystroke re-runs the live name match (model-
    // driven, NO request) and the autocomplete; a fresh draft clears any
    // unknown-field hint.
    {
      event: "input",
      selector: "#ro-filter-input",
      handler: () => {
        hideFilterFieldHint();
        applyLiveNameFilter();
        updateFilterAC();
        return true;
      },
      stop: true
    },
    // The editor keydown protocol (the focus-routed half of compound case 4):
    // #ro-filter-input owns ⏎ commit/accept, Tab accept, esc dismiss, arrows, and
    // ⌫-on-empty pop. No selector -- it keys off the focused target id, exactly
    // like the still-resident monolith keydown listener it replaces.
    {
      event: "keydown",
      handler: (event) => {
        if (event.target.id === "ro-filter-input") {
          handleFilterInputKeydown(event);
        }
      }
    }
  ];

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
      if (fr?.id && wrap.contains(fr)) {
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
  var bulkOverCapToasted;
  function takeRowStateCheckpoint() {
    return {
      selected: Array.from(rowSelection.entries()),
      focus: rowFocusKey,
      bulkOverCapToasted
    };
  }
  function applyLiveRowDeletions(keys) {
    let changed = false;
    for (const key of keys) changed = rowSelection.delete(key) || changed;
    if (rowFocusKey !== null && keys.has(rowFocusKey)) {
      rowFocusKey = null;
      changed = true;
    }
    if (changed) updateBulkBar();
  }
  function restoreRowStateCheckpoint(checkpoint) {
    rowSelection.clear();
    for (const [key, entry] of checkpoint.selected) rowSelection.set(key, entry);
    rowFocusKey = checkpoint.focus;
    bulkOverCapToasted = checkpoint.bulkOverCapToasted;
  }
  function updateBulkBar() {
    const bar = document.getElementById("ro-bulkbar");
    if (!bar) {
      return;
    }
    const count = rowSelection.size;
    const label = document.getElementById("ro-bulk-count");
    if (label) {
      label.textContent = `${count} selected`;
    }
    bar.classList.toggle("is-open", count > 0);
    bar.toggleAttribute("inert", count === 0);
    const download = document.getElementById("ro-bulk-download");
    if (download && bar.dataset.bulkHref) {
      const over = count > BULK_NAMES_MAX;
      download.disabled = over;
      download.title = over ? `Over the ${BULK_NAMES_MAX}-object bulk download cap` : "";
      if (over && !bulkOverCapToasted) {
        roToast(`Download refused: ${count} selected (max ${BULK_NAMES_MAX})`);
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
      ta.readOnly = true;
      ta.style.position = "fixed";
      ta.style.top = "-1000px";
      document.body.appendChild(ta);
      try {
        ta.select();
        return document.execCommand("copy");
      } catch {
        return false;
      } finally {
        ta.remove();
      }
    };
    if (navigator.clipboard?.writeText) {
      navigator.clipboard.writeText(text).then(
        () => done(true),
        () => done(fallback())
      );
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
        if (target?.closest("a, button, input, select, textarea, label")) {
          return;
        }
        toggleRowSelection(matched);
      }
    }
  ];

  // internal/assets/src/js/live-protocol.ts
  var LIVE_V2_LIMITS = Object.freeze({
    frameBytes: 16 * 1024 * 1024,
    deltaBytes: 256 * 1024,
    fragmentBytes: 128 * 1024,
    generationLength: 64,
    snapshotBytes: 16 * 1024 * 1024,
    keyLength: 2 * 1024,
    operations: 2e4,
    revisionLength: 128,
    resourceVersionLength: 256,
    schemaLength: 128,
    screenLength: 8 * 1024
  });
  var BASE_FIELDS = /* @__PURE__ */ new Set(["v", "kind", "g", "seq", "screen", "rev", "rv", "schema"]);
  var ROOT_PATH = "$";
  var textEncoder = new TextEncoder();
  var decodedEnvelopeTokens = /* @__PURE__ */ new WeakSet();
  function wireError(code, message, fatal = false) {
    return { code, message, fatal };
  }
  function isRecord(value) {
    return typeof value === "object" && value !== null && !Array.isArray(value);
  }
  function freezeWireValue(value) {
    if (Object(value) !== value) return;
    for (const child of Object.values(value)) freezeWireValue(child);
    Object.freeze(value);
  }
  function sealDecodedEnvelope(value) {
    freezeWireValue(value);
    decodedEnvelopeTokens.add(value);
    return value;
  }
  function own(record, key) {
    return Object.hasOwn(record, key);
  }
  function hasControlCharacters(value) {
    for (const character of value) {
      const code = character.codePointAt(0);
      if (code <= 31 || code >= 127 && code <= 159) return true;
    }
    return false;
  }
  function isGeneration(value) {
    for (const character of value) {
      const code = character.charCodeAt(0);
      if (code >= 48 && code <= 57 || code >= 65 && code <= 90 || code >= 97 && code <= 122 || character === "." || character === "_" || character === "~" || character === "-") {
        continue;
      }
      return false;
    }
    return true;
  }
  function hasDuplicateJSONMembers(source) {
    const contexts = [];
    let stringState = null;
    for (let offset = 0; offset < source.length; offset += 1) {
      const characterCode = source.charCodeAt(offset);
      if (stringState) {
        if (stringState.kind === "value") {
          if (stringState.mode === "escape") stringState.mode = "content";
          else if (characterCode === 92) stringState.mode = "escape";
          else if (characterCode === 34) stringState = null;
          continue;
        }
        if (stringState.mode === "escape") {
          stringState.mode = "content";
          continue;
        }
        if (characterCode === 92) {
          stringState.mode = "escape";
          continue;
        }
        if (characterCode !== 34) continue;
        const { keyContext, rawStart } = stringState;
        stringState = null;
        const key = JSON.parse(source.slice(rawStart, offset + 1));
        if (keyContext.keys.has(key)) return true;
        keyContext.keys.add(key);
        keyContext.expectsKey = false;
        continue;
      }
      const context = contexts.at(-1);
      if (characterCode === 34) {
        stringState = context?.kind === "object" && context.expectsKey ? {
          kind: "key",
          mode: "content",
          keyContext: context,
          rawStart: offset
        } : { kind: "value", mode: "content" };
        continue;
      }
      if (characterCode === 123) {
        contexts.push({ kind: "object", expectsKey: true, keys: /* @__PURE__ */ new Set() });
        continue;
      }
      if (characterCode === 91) {
        contexts.push({ kind: "array" });
        continue;
      }
      if (characterCode === 125 || characterCode === 93) {
        contexts.pop();
        continue;
      }
      if (characterCode === 44) {
        if (context?.kind === "object") context.expectsKey = true;
      }
    }
    return false;
  }
  function rejectUnknownFields(record, allowed, path) {
    const unknown = Object.keys(record).find((key) => !allowed.has(key));
    return unknown ? wireError("unexpected-field", `${path}.${unknown} is not allowed`) : null;
  }
  function boundedString(value, path, max) {
    if (typeof value !== "string" || value.length === 0) {
      return wireError("invalid-field", `${path} must be a non-empty string`);
    }
    if (value.length > max || textEncoder.encode(value).byteLength > max) {
      return wireError("limit-exceeded", `${path} exceeds ${max} bytes`);
    }
    if (hasControlCharacters(value)) {
      return wireError("invalid-field", `${path} contains forbidden characters`);
    }
    return value;
  }
  function htmlString(value, path, maxBytes) {
    if (typeof value !== "string" || value.length === 0) {
      return wireError("invalid-field", `${path} must be non-empty HTML`);
    }
    if (value.length > maxBytes || textEncoder.encode(value).byteLength > maxBytes) {
      return wireError("limit-exceeded", `${path} exceeds the HTML limit`);
    }
    return value;
  }
  function decodeBase(record) {
    if (record.v !== 2) {
      return wireError("unsupported-version", "v must be exactly 2");
    }
    if (!Number.isSafeInteger(record.seq) || record.seq < 1) {
      return wireError("invalid-field", "seq must be a positive safe integer");
    }
    const g = boundedString(record.g, "g", LIVE_V2_LIMITS.generationLength);
    if (typeof g !== "string") return g;
    if (!isGeneration(g)) {
      return wireError("invalid-field", "g contains forbidden characters");
    }
    const screen = boundedString(record.screen, "screen", LIVE_V2_LIMITS.screenLength);
    if (typeof screen !== "string") return screen;
    if (own(record, "rev")) {
      const rev = boundedString(record.rev, "rev", LIVE_V2_LIMITS.revisionLength);
      if (typeof rev !== "string") return rev;
    }
    if (own(record, "rv")) {
      const rv = boundedString(record.rv, "rv", LIVE_V2_LIMITS.resourceVersionLength);
      if (typeof rv !== "string") return rv;
    }
    if (own(record, "schema")) {
      const schema = boundedString(record.schema, "schema", LIVE_V2_LIMITS.schemaLength);
      if (typeof schema !== "string") return schema;
    }
    return null;
  }
  function decodeSnapshot(record) {
    const allowed = /* @__PURE__ */ new Set([...BASE_FIELDS, "snapshot"]);
    const unknown = rejectUnknownFields(record, allowed, ROOT_PATH);
    if (unknown) return { ok: false, error: unknown };
    if (!own(record, "rev") || !own(record, "schema")) {
      return {
        ok: false,
        error: wireError("invalid-field", "snapshot rev and schema are required")
      };
    }
    if (!isRecord(record.snapshot)) {
      return { ok: false, error: wireError("invalid-field", "snapshot must be an object") };
    }
    const nestedUnknown = rejectUnknownFields(record.snapshot, /* @__PURE__ */ new Set(["html"]), "snapshot");
    if (nestedUnknown) return { ok: false, error: nestedUnknown };
    const html = htmlString(record.snapshot.html, "snapshot.html", LIVE_V2_LIMITS.snapshotBytes);
    if (typeof html !== "string") return { ok: false, error: html };
    return {
      ok: true,
      value: sealDecodedEnvelope(record)
    };
  }
  function decodeRemove(value) {
    if (!Array.isArray(value) || value.length > LIVE_V2_LIMITS.operations) {
      return wireError(
        Array.isArray(value) ? "limit-exceeded" : "invalid-field",
        "delta.remove must be a bounded array"
      );
    }
    const seen = /* @__PURE__ */ new Set();
    const result = [];
    for (let index = 0; index < value.length; index += 1) {
      const item = value[index];
      if (!isRecord(item)) return wireError("invalid-field", `delta.remove[${index}]`);
      const unknown = rejectUnknownFields(
        item,
        /* @__PURE__ */ new Set(["key", "cause"]),
        `delta.remove[${index}]`
      );
      if (unknown) return unknown;
      const key = boundedString(item.key, `delta.remove[${index}].key`, LIVE_V2_LIMITS.keyLength);
      if (typeof key !== "string") return key;
      if (item.cause !== "delete" && item.cause !== "project") {
        return wireError("invalid-field", `delta.remove[${index}].cause is unknown`);
      }
      if (seen.has(key)) return wireError("duplicate", `duplicate remove key ${key}`);
      seen.add(key);
      result.push({ key, cause: item.cause });
    }
    return result;
  }
  function decodeUpsert(value) {
    if (!Array.isArray(value) || value.length > LIVE_V2_LIMITS.operations) {
      return wireError(
        Array.isArray(value) ? "limit-exceeded" : "invalid-field",
        "delta.upsert must be a bounded array"
      );
    }
    const seen = /* @__PURE__ */ new Set();
    const result = [];
    for (let index = 0; index < value.length; index += 1) {
      const item = value[index];
      if (!isRecord(item)) return wireError("invalid-field", `delta.upsert[${index}]`);
      const unknown = rejectUnknownFields(
        item,
        /* @__PURE__ */ new Set(["key", "row", "card"]),
        `delta.upsert[${index}]`
      );
      if (unknown) return unknown;
      const key = boundedString(item.key, `delta.upsert[${index}].key`, LIVE_V2_LIMITS.keyLength);
      if (typeof key !== "string") return key;
      const row = htmlString(
        item.row,
        `delta.upsert[${index}].row`,
        LIVE_V2_LIMITS.fragmentBytes
      );
      if (typeof row !== "string") return row;
      let card;
      if (own(item, "card")) {
        const decoded = htmlString(
          item.card,
          `delta.upsert[${index}].card`,
          LIVE_V2_LIMITS.fragmentBytes
        );
        if (typeof decoded !== "string") return decoded;
        card = decoded;
      }
      if (seen.has(key)) return wireError("duplicate", `duplicate upsert key ${key}`);
      seen.add(key);
      result.push(card === void 0 ? { key, row } : { key, row, card });
    }
    return result;
  }
  function decodeOrder(value) {
    if (!Array.isArray(value) || value.length > LIVE_V2_LIMITS.operations) {
      return wireError(
        Array.isArray(value) ? "limit-exceeded" : "invalid-field",
        "delta.order must be a bounded array"
      );
    }
    const result = [];
    const seen = /* @__PURE__ */ new Set();
    for (let index = 0; index < value.length; index += 1) {
      const key = boundedString(value[index], `delta.order[${index}]`, LIVE_V2_LIMITS.keyLength);
      if (typeof key !== "string") return key;
      if (seen.has(key)) return wireError("duplicate", `duplicate order key ${key}`);
      seen.add(key);
      result.push(key);
    }
    return result;
  }
  function decodeRegions(value) {
    if (!Array.isArray(value) || value.length > 3) {
      return wireError(
        Array.isArray(value) ? "limit-exceeded" : "invalid-field",
        "delta.regions must be a bounded array"
      );
    }
    const seen = /* @__PURE__ */ new Set();
    const result = [];
    for (let index = 0; index < value.length; index += 1) {
      const item = value[index];
      if (!isRecord(item)) return wireError("invalid-field", `delta.regions[${index}]`);
      const unknown = rejectUnknownFields(
        item,
        /* @__PURE__ */ new Set(["region", "html"]),
        `delta.regions[${index}]`
      );
      if (unknown) return unknown;
      if (item.region !== "count" && item.region !== "phase" && item.region !== "found") {
        return wireError("invalid-field", `delta.regions[${index}].region is unknown`);
      }
      const html = htmlString(
        item.html,
        `delta.regions[${index}].html`,
        LIVE_V2_LIMITS.fragmentBytes
      );
      if (typeof html !== "string") return html;
      if (seen.has(item.region)) {
        return wireError("duplicate", `duplicate region ${item.region}`);
      }
      seen.add(item.region);
      result.push({ region: item.region, html });
    }
    return result;
  }
  function decodeDelta(record) {
    const allowed = /* @__PURE__ */ new Set([...BASE_FIELDS, "delta"]);
    const unknown = rejectUnknownFields(record, allowed, ROOT_PATH);
    if (unknown) return { ok: false, error: unknown };
    if (!own(record, "rev") || !own(record, "schema") || !isRecord(record.delta)) {
      return {
        ok: false,
        error: wireError("invalid-field", "delta, envelope rev, and schema are required")
      };
    }
    const delta = record.delta;
    const nestedUnknown = rejectUnknownFields(
      delta,
      /* @__PURE__ */ new Set(["base", "rev", "remove", "upsert", "order", "regions"]),
      "delta"
    );
    if (nestedUnknown) return { ok: false, error: nestedUnknown };
    const base = boundedString(delta.base, "delta.base", LIVE_V2_LIMITS.revisionLength);
    if (typeof base !== "string") return { ok: false, error: base };
    const rev = boundedString(delta.rev, "delta.rev", LIVE_V2_LIMITS.revisionLength);
    if (typeof rev !== "string") return { ok: false, error: rev };
    if (record.rev !== rev) {
      return {
        ok: false,
        error: wireError("invalid-field", "envelope.rev must equal delta.rev")
      };
    }
    if (base === rev) {
      return { ok: false, error: wireError("no-op", "delta base and revision must differ") };
    }
    const result = { base, rev };
    if (own(delta, "remove")) {
      const remove = decodeRemove(delta.remove);
      if (!Array.isArray(remove)) return { ok: false, error: remove };
      result.remove = remove;
    }
    if (own(delta, "upsert")) {
      const upsert = decodeUpsert(delta.upsert);
      if (!Array.isArray(upsert)) return { ok: false, error: upsert };
      result.upsert = upsert;
    }
    const removeKeys = new Set(result.remove?.map((item) => item.key));
    const overlap = result.upsert?.find((item) => removeKeys.has(item.key));
    if (overlap) {
      return {
        ok: false,
        error: wireError("duplicate", `key ${overlap.key} is removed and upserted`)
      };
    }
    if (own(delta, "order")) {
      const order = decodeOrder(delta.order);
      if (!Array.isArray(order)) return { ok: false, error: order };
      result.order = order;
    }
    if (own(delta, "regions")) {
      const regions = decodeRegions(delta.regions);
      if (!Array.isArray(regions)) return { ok: false, error: regions };
      result.regions = regions;
    }
    if ((result.remove?.length || 0) === 0 && (result.upsert?.length || 0) === 0 && (result.order?.length || 0) === 0 && (result.regions?.length || 0) === 0) {
      return { ok: false, error: wireError("no-op", "delta has no semantic operations") };
    }
    return {
      ok: true,
      value: sealDecodedEnvelope({
        ...record,
        delta: result
      })
    };
  }
  function decodeTerminal(record) {
    const allowed = /* @__PURE__ */ new Set([...BASE_FIELDS, "reason"]);
    const unknown = rejectUnknownFields(record, allowed, ROOT_PATH);
    if (unknown) return { ok: false, error: unknown };
    if (record.reason !== "idle" && record.reason !== "auth" && record.reason !== "watch-failed" && record.reason !== "shutdown") {
      return { ok: false, error: wireError("invalid-field", "terminal reason is unknown") };
    }
    return {
      ok: true,
      value: sealDecodedEnvelope(record)
    };
  }
  function decodeLiveV2Envelope(frame) {
    try {
      if (typeof frame !== "string" || frame.length > LIVE_V2_LIMITS.frameBytes) {
        return { ok: false, error: wireError("limit-exceeded", "Live v2 frame is too large") };
      }
      const parsed = JSON.parse(frame);
      if (hasDuplicateJSONMembers(frame)) {
        return {
          ok: false,
          error: wireError("duplicate", "JSON object member names must be unique")
        };
      }
      if (!isRecord(parsed)) {
        return { ok: false, error: wireError("invalid-frame", "frame root must be an object") };
      }
      const maxFrameBytes = parsed.kind === "delta" ? LIVE_V2_LIMITS.deltaBytes : LIVE_V2_LIMITS.frameBytes;
      if (frame.length > maxFrameBytes) {
        return { ok: false, error: wireError("limit-exceeded", "Live v2 frame is too large") };
      }
      const frameByteLength = textEncoder.encode(frame).byteLength;
      if (frameByteLength > maxFrameBytes) {
        return { ok: false, error: wireError("limit-exceeded", "Live v2 frame is too large") };
      }
      const baseError = decodeBase(parsed);
      if (baseError) return { ok: false, error: baseError };
      if (parsed.kind === "snapshot") return decodeSnapshot(parsed);
      if (parsed.kind === "delta") return decodeDelta(parsed);
      if (parsed.kind === "terminal") return decodeTerminal(parsed);
      return { ok: false, error: wireError("unknown-kind", "kind is unknown") };
    } catch {
      return { ok: false, error: wireError("invalid-frame", "frame is not valid JSON") };
    }
  }
  function validateCursor(envelope, cursor) {
    if (envelope.g !== cursor.g) {
      return wireError("generation-mismatch", "delta generation does not match the cursor");
    }
    if (envelope.screen !== cursor.screen) {
      return wireError("screen-mismatch", "delta screen does not match the cursor");
    }
    if (envelope.seq !== cursor.seq + 1) {
      return wireError("sequence-gap", "delta sequence is not the cursor successor");
    }
    if (envelope.delta.base !== cursor.rev) {
      return wireError("base-mismatch", "delta base does not match the cursor revision");
    }
    if (envelope.schema !== cursor.schema) {
      return wireError("schema-mismatch", "delta schema does not match the cursor schema");
    }
    return null;
  }
  function decodeApplyInput(input) {
    if (typeof input === "object" && input !== null && decodedEnvelopeTokens.has(input)) {
      return { ok: true, value: input };
    }
    if (typeof input === "string") return decodeLiveV2Envelope(input);
    return {
      ok: false,
      error: wireError(
        "invalid-frame",
        "Live v2 apply input must be a raw frame or an opaque decoder result"
      )
    };
  }
  function applyLiveV2Delta(input, cursor, hooks = {}) {
    const decoded = decodeApplyInput(input);
    if (!decoded.ok) return decoded;
    const envelope = decoded.value;
    if (envelope.kind !== "delta") {
      return { ok: false, error: wireError("not-delta", "only delta envelopes can be applied") };
    }
    const cursorError = validateCursor(envelope, cursor);
    if (cursorError) return { ok: false, error: cursorError };
    const plan = {
      remove: envelope.delta.remove || [],
      upsert: envelope.delta.upsert || [],
      order: envelope.delta.order,
      regions: envelope.delta.regions || []
    };
    const deletedKeys = new Set(
      plan.remove.filter((operation) => operation.cause === "delete").map((operation) => operation.key)
    );
    const virtualizerCheckpoint = takeVirtualizerCheckpoint();
    const filterCheckpoint = takeLiveNameFilterCheckpoint();
    const rowStateCheckpoint = deletedKeys.size > 0 ? takeRowStateCheckpoint() : null;
    const result = applyListProjectionDeltaTransaction(plan, {
      morph: hooks.morph,
      reconcile: () => {
        hooks.beforeReconcile?.();
        applyLiveRowDeletions(deletedKeys);
        applyLiveNameFilter();
        if (!virtualizerActive()) reapplyRowState();
        hooks.afterReconcile?.();
      },
      restoreExternalState: () => {
        let failed = false;
        try {
          restoreVirtualizerCheckpoint(virtualizerCheckpoint);
        } catch {
          failed = true;
        }
        try {
          restoreLiveNameFilterCheckpoint(filterCheckpoint);
        } catch {
          failed = true;
        }
        if (rowStateCheckpoint) {
          try {
            restoreRowStateCheckpoint(rowStateCheckpoint);
          } catch {
            failed = true;
          }
        }
        if (failed) throw new Error();
      }
    });
    if (!result.ok) return result;
    const nextCursor = {
      g: envelope.g,
      seq: envelope.seq,
      screen: envelope.screen,
      rev: envelope.rev,
      schema: envelope.schema
    };
    if (envelope.rv !== void 0) nextCursor.rv = envelope.rv;
    else if (cursor.rv !== void 0) nextCursor.rv = cursor.rv;
    return {
      ok: true,
      cursor: nextCursor,
      summary: result.summary
    };
  }

  // internal/assets/src/js/bounded-byte-buffer.ts
  function ensureBoundedByteBufferCapacity(buffer, retainedBytes, appendedBytes, hardLimit) {
    const required = retainedBytes + appendedBytes;
    if (required <= buffer.byteLength) return buffer;
    const capacity = Math.min(
      hardLimit,
      buffer.byteLength + Math.max(buffer.byteLength, appendedBytes)
    );
    const grown = new Uint8Array(capacity);
    grown.set(buffer.subarray(0, retainedBytes));
    return grown;
  }

  // internal/assets/src/js/live-sse.ts
  var LIVE_SSE_DATA_CEILING_BYTES = 16777216;
  var LIVE_SSE_FRAMED_CEILING_BYTES = 16778240;
  var LIVE_SSE_LIMITS = Object.freeze({
    dataBytes: LIVE_SSE_DATA_CEILING_BYTES,
    // All nonblank field/comment bytes in one uncommitted event. This prevents
    // ignored extension/comment lines from multiplying bounded data work.
    eventBytes: LIVE_SSE_FRAMED_CEILING_BYTES,
    eventNameBytes: 64,
    lines: 32,
    // A legal max-size one-line payload still carries `data:` plus optional
    // whitespace. Keep a small, fixed framing allowance separate from the
    // exact aggregate data ceiling.
    lineBytes: LIVE_SSE_FRAMED_CEILING_BYTES
  });
  var LiveSSEError = class extends Error {
    code;
    constructor(code, message) {
      super(message);
      this.name = "LiveSSEError";
      this.code = code;
    }
  };
  var fatalUTF8 = new TextDecoder("utf-8", { fatal: true });
  function decodeLine(bytes) {
    try {
      return fatalUTF8.decode(bytes);
    } catch {
      throw new LiveSSEError("invalid-utf8", "SSE field line is not valid UTF-8");
    }
  }
  var LiveSSEParser = class {
    #limits;
    #lineBuffer = new Uint8Array();
    #lineBytes = 0;
    #pendingDelimiter = "none";
    #eventName = null;
    #eventBytes = 0;
    #dataLines = [];
    #dataBytes = 0;
    #lines = 0;
    #fatal = null;
    constructor(limits = {}) {
      this.#limits = { ...LIVE_SSE_LIMITS, ...limits };
    }
    push(chunk) {
      if (this.#fatal) throw this.#fatal;
      const events = [];
      try {
        this.#consume(chunk, events);
        return events;
      } catch (error) {
        this.#fatal = error instanceof LiveSSEError ? error : new LiveSSEError("invalid-utf8", "SSE framing failed");
        throw this.#fatal;
      }
    }
    #consume(chunk, events) {
      let start = 0;
      let index = 0;
      while (index !== chunk.byteLength) {
        const byteIndex = index;
        const byte = chunk[byteIndex];
        index += 1;
        if (this.#pendingDelimiter === "cr") {
          this.#pendingDelimiter = "none";
          if (byte === 10) {
            start = index;
            continue;
          }
        }
        if (byte !== 10 && byte !== 13) continue;
        this.#appendLinePart(chunk.subarray(start, byteIndex));
        this.#completeLine(events);
        this.#pendingDelimiter = byte === 13 ? "cr" : "none";
        start = index;
      }
      this.#appendLinePart(chunk.subarray(start));
    }
    #appendLinePart(part) {
      if (part.byteLength > this.#limits.lineBytes - this.#lineBytes) {
        throw new LiveSSEError("line-too-large", "SSE field line exceeds its byte limit");
      }
      const required = this.#lineBytes + part.byteLength;
      this.#lineBuffer = ensureBoundedByteBufferCapacity(
        this.#lineBuffer,
        this.#lineBytes,
        part.byteLength,
        this.#limits.lineBytes
      );
      this.#lineBuffer.set(part, this.#lineBytes);
      this.#lineBytes = required;
    }
    #completeLine(events) {
      const bytes = this.#lineBuffer.subarray(0, this.#lineBytes);
      this.#lineBytes = 0;
      if (bytes.byteLength === 0) {
        if (this.#dataLines.length > 0) {
          events.push({
            name: this.#eventName,
            data: this.#dataLines.join("\n"),
            dataBytes: this.#dataBytes
          });
        }
        this.#resetEvent();
        return;
      }
      if (bytes.byteLength > this.#limits.eventBytes - this.#eventBytes) {
        throw new LiveSSEError("event-too-large", "SSE event framing exceeds its byte limit");
      }
      this.#eventBytes += bytes.byteLength;
      this.#lines += 1;
      if (this.#lines > this.#limits.lines) {
        throw new LiveSSEError("too-many-lines", "SSE event has too many nonblank lines");
      }
      const colon = bytes.indexOf(58);
      const fieldEnd = colon === -1 ? bytes.byteLength : colon;
      let valueStart = colon === -1 ? bytes.byteLength : colon + 1;
      if (bytes[valueStart] === 32) valueStart += 1;
      const field = decodeLine(bytes.subarray(0, fieldEnd));
      const valueBytes = bytes.subarray(valueStart);
      if (field === "event") {
        if (valueBytes.byteLength > this.#limits.eventNameBytes) {
          throw new LiveSSEError(
            "event-name-too-large",
            "SSE event name exceeds its byte limit"
          );
        }
        this.#eventName = decodeLine(valueBytes);
        return;
      }
      if (field !== "data") {
        decodeLine(valueBytes);
        return;
      }
      const joinedBytes = this.#dataBytes + valueBytes.byteLength + (this.#dataLines.length ? 1 : 0);
      if (joinedBytes > this.#limits.dataBytes) {
        throw new LiveSSEError("data-too-large", "SSE event data exceeds its byte limit");
      }
      this.#dataLines.push(decodeLine(valueBytes));
      this.#dataBytes = joinedBytes;
    }
    #resetEvent() {
      this.#eventName = null;
      this.#eventBytes = 0;
      this.#dataLines = [];
      this.#dataBytes = 0;
      this.#lines = 0;
    }
  };

  // internal/assets/src/js/live-url.ts
  var CLIENT_GENERATION_HEX_LENGTH = 32;
  var CLIENT_GENERATION_UUID_LENGTH = 36;
  var CLIENT_GENERATION_UUID_DASHES = /* @__PURE__ */ new Set([8, 13, 18, 23]);
  var ASCII_HEX_DIGITS = "0123456789abcdefABCDEF";
  function isClientLiveGeneration(value) {
    if (typeof value !== "string" || value.length !== CLIENT_GENERATION_HEX_LENGTH && value.length !== CLIENT_GENERATION_UUID_LENGTH) {
      return false;
    }
    const uuid = value.length === CLIENT_GENERATION_UUID_LENGTH;
    return Array.from(value).every(
      (character, index) => uuid && CLIENT_GENERATION_UUID_DASHES.has(index) ? character === "-" : ASCII_HEX_DIGITS.includes(character)
    );
  }
  function mintLiveGeneration(cryptoSource = window.crypto) {
    try {
      const uuid = cryptoSource.randomUUID();
      if (isClientLiveGeneration(uuid)) return uuid;
    } catch {
    }
    const bytes = cryptoSource.getRandomValues(new Uint8Array(16));
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  }
  function decodedQueryKey(raw) {
    try {
      return decodeURIComponent(raw.replaceAll("+", " "));
    } catch {
      return null;
    }
  }
  function stripLiveGenerationQuery(rawQuery) {
    return rawQuery.split("&").filter((pair) => decodedQueryKey(pair.split("=", 1)[0]) !== "g").join("&");
  }
  function withRawQuery(pathname, rawQuery) {
    return rawQuery === "" ? pathname : `${pathname}?${rawQuery}`;
  }
  function splitRawQuery(path) {
    const queryStart = path.indexOf("?");
    return queryStart === -1 ? [path, ""] : [path.slice(0, queryStart), path.slice(queryStart + 1)];
  }
  function liveStreamBaseForURL(url) {
    const pathname = `${url.pathname.replace(/\/+$/, "")}/_stream`;
    return withRawQuery(pathname, stripLiveGenerationQuery(url.search.slice(1)));
  }
  function liveScreenForBase(base) {
    const [pathname, query] = splitRawQuery(base);
    const screenPath = pathname.endsWith("/_stream") ? pathname.slice(0, -"/_stream".length) : pathname;
    return withRawQuery(screenPath, query);
  }
  function liveRequestURL(base, generation) {
    if (!isClientLiveGeneration(generation)) throw new Error("invalid Live generation");
    return `${base}${base.includes("?") ? "&" : "?"}g=${generation}`;
  }
  function liveStreamBaseFromTableRequest(requestPath) {
    if (typeof requestPath !== "string") return null;
    try {
      const url = new URL(requestPath, window.location.href);
      if (url.origin !== window.location.origin || !url.pathname.endsWith("/_table")) return null;
      return withRawQuery(
        `${url.pathname.slice(0, -"/_table".length)}/_stream`,
        stripLiveGenerationQuery(url.search.slice(1))
      );
    } catch {
      return null;
    }
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
    span.textContent = `${remaining}s`;
  }
  function isListRefreshEvent(event) {
    const detail = event.detail;
    if (!detail || isPreloadRequest(event)) {
      return false;
    }
    const elt = detail.elt;
    if (elt && elt.id === "resource-list-content") {
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
    if (!banner) {
      return;
    }
    banner.hidden = false;
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

  // internal/assets/src/js/live.ts
  function getHtmx2() {
    return window.htmx;
  }
  var liveState = {
    status: "idle",
    abort: null,
    gen: "",
    streamPath: ""
  };
  var counters = {
    connections: 0,
    resyncs: 0,
    fallbacks: 0,
    v1Snapshots: 0,
    v2Snapshots: 0,
    deltas: 0,
    terminals: 0,
    invalidFrames: 0,
    discards: 0,
    rawBytes: 0,
    payloadBytes: 0,
    snapshotBytes: 0,
    deltaBytes: 0,
    inserted: 0,
    updated: 0,
    deleted: 0,
    projected: 0
  };
  var RESYNC_WINDOW_MS = 3e4;
  var MAX_RESYNCS_PER_WINDOW = 2;
  var LIVE_FIRST_FRAME_TIMEOUT_MS = 1e4;
  var activeConnection = null;
  var liveFallbackSecs = 0;
  var completedSnapshotTxns = /* @__PURE__ */ new WeakSet();
  var resyncTimestamps = [];
  var resyncScheduled = false;
  var pendingResync = false;
  var resumeAfterRequests = false;
  var resumeAfterHidden = false;
  var resumeBase = "";
  var liveDiscards = 0;
  var ownedRequests = /* @__PURE__ */ new Map();
  function ownsRequest(xhr, entry) {
    return ownedRequests.get(xhr) === entry;
  }
  function clearResumeIntent() {
    pendingResync = false;
    resyncScheduled = false;
    resumeAfterRequests = false;
    resumeAfterHidden = false;
    resumeBase = "";
  }
  function supportedResumeBase() {
    return liveSupported() ? resumeBase || liveStreamBase() : "";
  }
  function addCounter(name, amount = 1) {
    counters[name] += amount;
    liveDiscards = counters.discards;
  }
  function pruneResyncWindow(now = Date.now()) {
    resyncTimestamps = resyncTimestamps.filter((timestamp) => now - timestamp < RESYNC_WINDOW_MS);
  }
  function currentStats() {
    pruneResyncWindow();
    return {
      ...counters,
      state: liveState.status,
      protocol: activeConnection?.protocol || null,
      seq: activeConnection?.cursor?.seq || 0,
      inFlightRequests: ownedRequests.size,
      resyncsInWindow: resyncTimestamps.length
    };
  }
  function liveFallbackSeconds() {
    return liveFallbackSecs;
  }
  function liveSupported() {
    const content = document.getElementById("resource-list-content");
    if (content?.dataset.liveUrl !== "location") return false;
    const option = document.querySelector(
      '[data-ro-action="set-refresh"][data-ro-interval="Live"]'
    );
    return !!option && !option.disabled;
  }
  function liveStreamBase() {
    return liveStreamBaseForURL(new URL(window.location.href));
  }
  function isActive(connection) {
    return activeConnection === connection;
  }
  function connectionToken(source) {
    return Object.freeze({
      ...source,
      cursor: source.cursor ? Object.freeze({ ...source.cursor }) : null
    });
  }
  function replaceConnection(current, protocol, cursor) {
    if (!isActive(current)) return null;
    const next = connectionToken({ ...current, protocol, cursor });
    activeConnection = next;
    return next;
  }
  function abortActiveConnection() {
    const connection = activeConnection;
    activeConnection = null;
    liveState.abort = null;
    connection?.ctrl.abort();
  }
  function liveTeardown() {
    abortActiveConnection();
    liveFallbackSecs = 0;
  }
  function liveResetPage() {
    ownedRequests.clear();
    clearResumeIntent();
    resyncTimestamps = [];
  }
  function liveEngageFallback(banner) {
    abortActiveConnection();
    clearResumeIntent();
    liveState.status = "fallback";
    liveFallbackSecs = document.getElementById("resource-list-content") ? 5 : 0;
    addCounter("fallbacks");
    scheduleRefreshTick();
    if (banner) markListStale();
  }
  function openConnection(base) {
    abortActiveConnection();
    liveFallbackSecs = 0;
    liveState.streamPath = base;
    if (!base) {
      liveEngageFallback(false);
      return;
    }
    if (document.hidden) {
      resumeBase = base;
      resumeAfterHidden = true;
      liveState.status = "hidden";
      scheduleRefreshTick();
      return;
    }
    let generation;
    try {
      generation = mintLiveGeneration();
    } catch {
      liveEngageFallback(false);
      return;
    }
    const ctrl = new AbortController();
    const connection = connectionToken({
      ctrl,
      generation,
      base,
      screen: liveScreenForBase(base),
      protocol: "pending",
      cursor: null
    });
    activeConnection = connection;
    liveState.abort = ctrl;
    liveState.gen = generation;
    liveState.status = "connecting";
    addCounter("connections");
    scheduleRefreshTick();
    void liveConnect(connection);
  }
  function responseHeader2(response, name) {
    try {
      return response.headers.get(name);
    } catch {
      return null;
    }
  }
  function negotiatedProtocol(response, connection) {
    const contentType = responseHeader2(response, "Content-Type");
    if (contentType?.split(";", 1)[0].trim().toLowerCase() !== "text/event-stream") {
      return null;
    }
    const version = responseHeader2(response, "RO-Live-Version");
    const generation = responseHeader2(response, "RO-Live-Generation");
    if (version === null && generation === null) return "v1";
    if (version === "2" && generation === connection.generation) return "v2";
    return null;
  }
  function firstFrameAccepted() {
    return liveState.status === "open-v1" || liveState.status === "open-v2";
  }
  async function liveConnect(initial) {
    let firstFrameTimer = window.setTimeout(() => {
      firstFrameTimer = null;
      const current = activeConnection;
      if (current?.ctrl === initial.ctrl) {
        liveEngageFallback(false);
      }
    }, LIVE_FIRST_FRAME_TIMEOUT_MS);
    const clearFirstFrameTimer = () => {
      if (firstFrameTimer !== null) {
        window.clearTimeout(firstFrameTimer);
        firstFrameTimer = null;
      }
    };
    try {
      await runLiveConnection(initial, clearFirstFrameTimer);
    } finally {
      clearFirstFrameTimer();
    }
  }
  async function runLiveConnection(initial, clearFirstFrameTimer) {
    let response;
    try {
      response = await fetch(liveRequestURL(initial.base, initial.generation), {
        signal: initial.ctrl.signal,
        headers: {
          "RO-Live-Version": "2",
          "RO-Live-Generation": initial.generation
        }
      });
    } catch {
      if (!isActive(initial)) return;
      liveEngageFallback(false);
      return;
    }
    if (!isActive(initial)) return;
    if (response.status !== 200 || !response.body) {
      liveEngageFallback(false);
      return;
    }
    const protocol = negotiatedProtocol(response, initial);
    if (!protocol) {
      rejectProtocol(initial);
      return;
    }
    let connection = replaceConnection(initial, protocol, null);
    if (!connection) return;
    liveState.status = protocol === "v2" ? "syncing-v2" : "syncing-v1";
    const parser = new LiveSSEParser();
    let reader;
    try {
      reader = response.body.getReader();
    } catch {
      if (isActive(connection)) liveEngageFallback(true);
      return;
    }
    try {
      for (; ; ) {
        const result = await reader.read();
        if (!isActive(connection)) return;
        if (result.done) break;
        const value = result.value;
        addCounter("rawBytes", value.byteLength);
        let events;
        try {
          events = parser.push(value);
        } catch {
          addCounter("invalidFrames");
          if (connection.protocol === "v2") rejectProtocol(connection, false);
          else liveEngageFallback(true);
          return;
        }
        for (const event of events) {
          addCounter("payloadBytes", event.dataBytes);
          if (connection.protocol === "v2") {
            handleV2Frame(connection, event.name, event.data, event.dataBytes);
          } else {
            handleV1Frame(connection, event.name, event.data, event.dataBytes);
          }
          const current = activeConnection;
          if (!current || current.ctrl !== connection.ctrl) return;
          connection = current;
          if (firstFrameAccepted()) clearFirstFrameTimer();
        }
      }
    } catch {
    }
    if (isActive(connection)) liveEngageFallback(true);
  }
  function parseJSONValue(text) {
    let value;
    try {
      value = JSON.parse(text);
    } catch {
    }
    return value;
  }
  var TERMINAL_REASONS = /* @__PURE__ */ new Set(["idle", "auth", "watch-failed", "shutdown"]);
  function handleV1Frame(connection, name, text, payloadBytes) {
    if (name !== "ro-table" && name !== "ro-terminal") return;
    const value = parseJSONValue(text);
    const payload = Object(value);
    if (name === "ro-terminal") {
      if (payload.g !== connection.generation || typeof payload.reason !== "string" || !TERMINAL_REASONS.has(payload.reason)) {
        addCounter("invalidFrames");
        return;
      }
      addCounter("terminals");
      liveEngageFallback(true);
      return;
    }
    if (typeof payload.g !== "string" || typeof payload.html !== "string") {
      addCounter("invalidFrames");
      return;
    }
    const currentBase = liveStreamBase();
    const discard = shouldDiscardPush({
      frameGeneration: payload.g,
      currentGeneration: connection.generation,
      liveStreamBase: currentBase,
      openedStreamBase: connection.base
    });
    if (discard) {
      addCounter("discards");
      return;
    }
    if (!swapSnapshot(payload.html, connection, null)) {
      addCounter("invalidFrames");
      liveEngageFallback(true);
      return;
    }
    addCounter("v1Snapshots");
    addCounter("snapshotBytes", payloadBytes);
    liveState.status = "open-v1";
  }
  function validEnvelopeIdentity(envelope, connection) {
    return envelope.g === connection.generation && envelope.screen === connection.screen;
  }
  function snapshotCursor(envelope) {
    const cursor = {
      g: envelope.g,
      seq: envelope.seq,
      screen: envelope.screen,
      rev: envelope.rev,
      schema: envelope.schema
    };
    if (envelope.rv !== void 0) cursor.rv = envelope.rv;
    return cursor;
  }
  function handleV2Frame(connection, name, text, payloadBytes) {
    if (name !== "ro-live") {
      rejectProtocol(connection);
      return;
    }
    const decoded = decodeLiveV2Envelope(text);
    if (!decoded.ok) {
      rejectProtocol(connection);
      return;
    }
    const envelope = decoded.value;
    const cursor = connection.cursor;
    if (!validEnvelopeIdentity(envelope, connection)) {
      rejectProtocol(connection);
      return;
    }
    if (!cursor) {
      if (envelope.kind !== "snapshot" || envelope.seq !== 1) {
        rejectProtocol(connection);
        return;
      }
      commitV2Snapshot(connection, envelope, payloadBytes);
      return;
    }
    if (envelope.seq !== cursor.seq + 1) {
      rejectProtocol(connection);
      return;
    }
    if (envelope.kind === "snapshot") {
      commitV2Snapshot(connection, envelope, payloadBytes);
      return;
    }
    if (envelope.kind === "terminal") {
      if (envelope.rev !== cursor.rev || envelope.schema !== cursor.schema) {
        rejectProtocol(connection);
        return;
      }
      addCounter("terminals");
      liveEngageFallback(true);
      return;
    }
    const applied = applyLiveV2Delta(decoded.value, cursor);
    if (!applied.ok) {
      rejectProtocol(connection);
      return;
    }
    if (!replaceConnection(connection, "v2", applied.cursor)) {
      return;
    }
    clearListValidator();
    addCounter("deltas");
    addCounter("deltaBytes", payloadBytes);
    addCounter("inserted", applied.summary.inserted);
    addCounter("updated", applied.summary.updated);
    addCounter("deleted", applied.summary.deleted);
    addCounter("projected", applied.summary.projected);
    liveState.status = "open-v2";
  }
  function commitV2Snapshot(connection, envelope, payloadBytes) {
    const txn = Object.freeze({});
    swapSnapshot(envelope.snapshot.html, connection, txn);
    const completed = completedSnapshotTxns.has(txn);
    if (!completed || !isActive(connection)) {
      rejectProtocol(connection);
      return;
    }
    replaceConnection(connection, "v2", snapshotCursor(envelope));
    addCounter("v2Snapshots");
    addCounter("snapshotBytes", payloadBytes);
    liveState.status = "open-v2";
  }
  function swapSnapshot(html, connection, txn) {
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx2();
    if (!content || !htmx2 || !isActive(connection)) return false;
    clearListValidator();
    const eventInfo = { target: content, roLivePush: true };
    if (txn) eventInfo.roLiveSnapshotTxn = txn;
    try {
      htmx2.swap(content, html, { swapStyle: "morph" }, { contextElement: content, eventInfo });
      return isActive(connection);
    } catch {
      return false;
    }
  }
  function rejectProtocol(connection, countInvalid = true) {
    if (!isActive(connection)) return;
    if (countInvalid) addCounter("invalidFrames");
    const base = connection.base;
    abortActiveConnection();
    requestResync(base);
  }
  function requestResync(base) {
    if (document.hidden) {
      pendingResync = true;
      resumeBase = base;
      liveState.status = "hidden";
      resumeAfterHidden = true;
      return;
    }
    const now = Date.now();
    pruneResyncWindow(now);
    if (resyncTimestamps.length >= MAX_RESYNCS_PER_WINDOW) {
      liveEngageFallback(true);
      return;
    }
    resyncTimestamps.push(now);
    addCounter("resyncs");
    liveState.status = "resyncing";
    resyncScheduled = true;
    queueMicrotask(() => {
      if (!resyncScheduled) return;
      resyncScheduled = false;
      openConnection(base);
    });
  }
  function requestDetail(event) {
    return Object(event.detail);
  }
  function requestPathBase(detail) {
    try {
      const pathInfo = Object(detail.pathInfo);
      return liveStreamBaseFromTableRequest(pathInfo.finalRequestPath) || liveStreamBaseFromTableRequest(pathInfo.requestPath);
    } catch {
      return null;
    }
  }
  function liveBeforeListRequest(event) {
    const detail = requestDetail(event);
    const content = document.getElementById("resource-list-content");
    const xhr = detail.xhr;
    if (!(content && xhr && detail.target === content) || ownedRequests.has(xhr)) return;
    const entry = {
      networkSettled: false,
      sent: false,
      swapCompleted: false
    };
    ownedRequests.set(xhr, entry);
    try {
      xhr.addEventListener("loadend", () => {
        if (ownsRequest(xhr, entry)) noteRequestNetworkSettled(xhr, entry);
      });
    } catch {
    }
    if (!ownsRequest(xhr, entry)) return;
    queueMicrotask(() => {
      if (ownsRequest(xhr, entry) && !entry.sent) finalizeOwnedRequest(xhr, entry);
    });
    const resumable = activeConnection !== null || resyncScheduled || resumeAfterRequests || resumeAfterHidden;
    if (!resumable) return;
    resyncScheduled = false;
    resumeAfterRequests = true;
    resumeBase ||= activeConnection?.base || liveState.streamPath;
    abortActiveConnection();
    if (document.hidden) {
      liveState.status = "hidden";
      resumeAfterHidden = true;
    } else {
      liveState.status = "suspended";
    }
  }
  function liveMarkListRequestSent(event) {
    const xhr = requestDetail(event).xhr;
    const entry = xhr ? ownedRequests.get(xhr) : void 0;
    if (xhr && entry && ownsRequest(xhr, entry)) entry.sent = true;
  }
  function liveAfterListRequest(event) {
    const detail = requestDetail(event);
    const xhr = detail.xhr;
    if (!xhr) return;
    const entry = ownedRequests.get(xhr);
    if (entry && ownsRequest(xhr, entry)) noteRequestNetworkSettled(xhr, entry);
  }
  function requestStatus(xhr) {
    try {
      return xhr.status;
    } catch {
      return 0;
    }
  }
  function noteRequestNetworkSettled(xhr, entry) {
    entry.networkSettled = true;
    const status = requestStatus(xhr);
    if (status === 200 && !entry.swapCompleted) {
      queueMicrotask(() => failOwnedRequestWithoutSwap(xhr, entry));
      return;
    }
    finalizeOwnedRequest(xhr, entry);
  }
  function completeOwnedRequestSwap(xhr, entry, successfulBase) {
    if (!ownsRequest(xhr, entry)) return;
    entry.swapCompleted = true;
    if (successfulBase) {
      resumeBase = successfulBase;
    }
    if (entry.networkSettled) finalizeOwnedRequest(xhr, entry);
  }
  function failOwnedRequestWithoutSwap(xhr, entry) {
    if (!ownsRequest(xhr, entry)) return;
    ownedRequests.delete(xhr);
    if (!resumeAfterRequests) return;
    if (refreshMode() !== "Live") {
      clearResumeIntent();
      liveState.status = "idle";
      liveState.streamPath = "";
      return;
    }
    liveEngageFallback(true);
  }
  function finalizeOwnedRequest(xhr, entry) {
    if (!ownsRequest(xhr, entry)) return;
    ownedRequests.delete(xhr);
    if (ownedRequests.size > 0 || !resumeAfterRequests) return;
    if (document.hidden) {
      liveState.status = "hidden";
      resumeAfterHidden = true;
      return;
    }
    const shouldResync = pendingResync;
    if (refreshMode() !== "Live") {
      clearResumeIntent();
      liveState.status = "idle";
      liveState.streamPath = "";
      return;
    }
    const supportedBase = supportedResumeBase();
    clearResumeIntent();
    if (supportedBase && shouldResync) requestResync(supportedBase);
    else openConnection(supportedBase);
  }
  function liveBeforeListSwapDecision(event) {
    const detail = requestDetail(event);
    const xhr = detail.xhr;
    const entry = xhr ? ownedRequests.get(xhr) : void 0;
    if (!xhr || !entry || !ownsRequest(xhr, entry)) return;
    queueMicrotask(() => {
      if (event.defaultPrevented || detail.shouldSwap === false) {
        completeOwnedRequestSwap(xhr, entry, null);
      }
    });
  }
  function liveListRequestSwapFailed(event) {
    const detail = requestDetail(event);
    const xhr = detail.xhr;
    const entry = xhr ? ownedRequests.get(xhr) : void 0;
    if (xhr && entry && ownsRequest(xhr, entry)) completeOwnedRequestSwap(xhr, entry, null);
  }
  function liveOnListSwap(event) {
    const detail = requestDetail(event);
    const snapshotTxn = detail.roLiveSnapshotTxn;
    if (detail.roLivePush === true) {
      if (typeof snapshotTxn === "object" && snapshotTxn !== null) {
        completedSnapshotTxns.add(snapshotTxn);
      }
      return;
    }
    const xhr = detail.xhr;
    const entry = xhr ? ownedRequests.get(xhr) : void 0;
    if (!xhr || !entry || !ownsRequest(xhr, entry)) return;
    const base = requestPathBase(detail);
    completeOwnedRequestSwap(xhr, entry, base);
  }
  function liveApply(force) {
    if (refreshMode() !== "Live") {
      liveTeardown();
      clearResumeIntent();
      liveState.status = "idle";
      liveState.streamPath = "";
      return;
    }
    const base = liveSupported() ? liveStreamBase() : "";
    if (force) {
      resyncTimestamps = [];
      clearResumeIntent();
    }
    if (!force && liveState.status === "fallback") return;
    if (!force && base === liveState.streamPath && liveState.status !== "idle") return;
    if (ownedRequests.size > 0) {
      liveTeardown();
      resumeAfterRequests = true;
      resumeBase = base;
      liveState.streamPath = base;
      liveState.status = document.hidden ? "hidden" : "suspended";
      resumeAfterHidden = document.hidden;
      return;
    }
    openConnection(base);
  }
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      if (activeConnection || resumeAfterRequests || resyncScheduled) {
        resumeBase ||= activeConnection?.base || liveState.streamPath;
        resumeAfterHidden = true;
        abortActiveConnection();
        liveState.status = "hidden";
      }
      return;
    }
    if (!resumeAfterHidden) return;
    if (refreshMode() !== "Live") {
      clearResumeIntent();
      liveState.status = "idle";
      liveState.streamPath = "";
      return;
    }
    if (ownedRequests.size > 0) {
      liveState.status = "suspended";
      return;
    }
    const shouldResync = pendingResync;
    const supportedBase = supportedResumeBase();
    clearResumeIntent();
    if (supportedBase && shouldResync) requestResync(supportedBase);
    else openConnection(supportedBase);
  });
  window.roLive = {
    discards() {
      return liveDiscards;
    },
    stats() {
      return currentStats();
    }
  };

  // internal/assets/src/js/prefs.ts
  var PREFS_COOKIE = "ro_prefs";
  var PREFS_VERSION_PREFIX = "v1.";
  var PREFS_MAX_ENCODED = 3072;
  var PREFS_COOKIE_MAX_AGE = 31536e3;
  var REFRESH_KEY = "roRefresh";
  function b64urlEncodeUTF8(text) {
    const bytes = new TextEncoder().encode(text);
    const bin = Array.from(bytes, (byte) => String.fromCharCode(byte)).join("");
    return btoa(bin).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
  }
  function b64urlDecodeUTF8(encoded) {
    const compact = encoded.replaceAll("\r", "").replaceAll("\n", "");
    if (!/^[A-Za-z0-9_-]*$/.test(compact)) {
      throw new TypeError();
    }
    const b64 = compact.replaceAll("-", "+").replaceAll("_", "/");
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (char) => char.charCodeAt(0));
    return new TextDecoder("utf-8", { ignoreBOM: true }).decode(bytes);
  }
  function isRecord2(value) {
    return typeof value === "object" && value !== null && !Array.isArray(value);
  }
  function setOwnNamespace(ns, cluster, namespace) {
    Object.defineProperty(ns, cluster, {
      value: namespace,
      enumerable: true,
      configurable: true,
      writable: true
    });
  }
  function compareUTF8(left, right) {
    const mismatch = left.subarray(0, right.length).findIndex((byte, index) => byte !== right[index]);
    if (mismatch !== -1) {
      return left[mismatch] - right[mismatch];
    }
    return left.length - right.length;
  }
  function stringifyPrefsJSON(value) {
    const encoded = JSON.stringify(value);
    return encoded.replaceAll("\u2028", "\\u2028").replaceAll("\u2029", "\\u2029");
  }
  function canonicalNamespaceJSON(ns) {
    const encoder = new TextEncoder();
    const entries = Object.entries(ns).map(([cluster, namespace]) => ({
      cluster,
      namespace,
      encodedCluster: encoder.encode(cluster)
    }));
    entries.sort((left, right) => compareUTF8(left.encodedCluster, right.encodedCluster));
    return `{${entries.map(
      ({ cluster, namespace }) => `${stringifyPrefsJSON(cluster)}:${stringifyPrefsJSON(namespace)}`
    ).join(",")}}`;
  }
  function decodePrefsValue(value) {
    const empty = { kinds: [], refresh: "", ns: {} };
    if (!value?.startsWith(PREFS_VERSION_PREFIX)) {
      return { prefs: empty, ok: false };
    }
    const payload = value.slice(PREFS_VERSION_PREFIX.length);
    try {
      const decoded = JSON.parse(b64urlDecodeUTF8(payload));
      if (!isRecord2(decoded)) {
        return { prefs: empty, ok: false };
      }
      const kinds = [];
      if (Array.isArray(decoded.kinds)) {
        decoded.kinds.forEach((raw) => {
          if (!isRecord2(raw)) {
            return;
          }
          const e = raw;
          if (typeof e.k !== "string") {
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
      if (isRecord2(decoded.ns)) {
        Object.entries(decoded.ns).forEach(([cluster, namespace]) => {
          if (typeof namespace === "string") {
            setOwnNamespace(ns, cluster, namespace);
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
    } catch (_e) {
      return { prefs: empty, ok: false };
    }
  }
  function encodePrefsCandidate(kinds, refresh, ns) {
    const fields = [];
    if (kinds.length > 0) {
      fields.push(`"kinds":${stringifyPrefsJSON(kinds)}`);
    }
    if (refresh) {
      fields.push(`"refresh":${stringifyPrefsJSON(refresh)}`);
    }
    if (Object.keys(ns).length > 0) {
      fields.push(`"ns":${canonicalNamespaceJSON(ns)}`);
    }
    return PREFS_VERSION_PREFIX + b64urlEncodeUTF8(`{${fields.join(",")}}`);
  }
  function encodePrefsValue(prefs) {
    const kinds = prefs.kinds ?? [];
    const refresh = prefs.refresh ?? "";
    const ns = prefs.ns ?? {};
    const evictionBoundary = PREFS_MAX_ENCODED + 1;
    let value = encodePrefsCandidate(kinds, refresh, ns);
    if (value.length < evictionBoundary) {
      return value;
    }
    Array.from({ length: kinds.length }).some((_, evicted) => {
      const kept = kinds.length - evicted - 1;
      value = encodePrefsCandidate(kinds.slice(0, kept), refresh, ns);
      return value.length < evictionBoundary;
    });
    return value;
  }
  function prefsCookieValue() {
    const prefix = `${PREFS_COOKIE}=`;
    return document.cookie.split("; ").find((part) => part.startsWith(prefix))?.slice(prefix.length);
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
    } catch (_e) {
    }
  }
  function prefsTouchKind(prefs, plural) {
    const index = prefs.kinds.findIndex((entry2) => entry2.k === plural);
    const entry = index < 0 ? { k: plural } : prefs.kinds.splice(index, 1)[0];
    prefs.kinds.unshift(entry);
    return entry;
  }
  function roPrefsSetSort(plural, sort) {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).sort = sort;
    writePrefs(prefs);
  }
  function roPrefsSetHiddenColumns(plural, names) {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).hide = names;
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
    setOwnNamespace(prefs.ns, cluster, namespace);
    writePrefs(prefs);
  }

  // internal/assets/src/js/refresh.ts
  function getHtmx3() {
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
  function requestDetail2(event) {
    return Object(event.detail);
  }
  function pruneSettledListRequests(requests) {
    requests.forEach((xhr) => {
      if (xhr.readyState === 4 || xhr.readyState === 0) {
        requests.delete(xhr);
      }
    });
  }
  function isPreloadRequest(event) {
    const config = Object(requestDetail2(event).requestConfig);
    const headers = Object(config.headers);
    return headers["HX-Preloaded"] === "true";
  }
  function isUserListRequest(event) {
    const detail = requestDetail2(event);
    return detail.elt instanceof Element && detail.target instanceof Element && detail.target.id === "resource-list-content" && !isPreloadRequest(event);
  }
  function handleRefreshConfigRequest(event) {
    const detail = requestDetail2(event);
    const content = document.getElementById("resource-list-content");
    const sourceIsContent = content !== null && detail.elt === content;
    const targetIsContent = content !== null && detail.target === content;
    if (sourceIsContent || targetIsContent) {
      const headers = Object(detail.headers);
      for (const name of Object.keys(headers)) {
        if (name.toLowerCase() === "ro-no-push") {
          delete headers[name];
        }
      }
      if (sourceIsContent && (detail.target === void 0 || targetIsContent)) {
        headers["RO-No-Push"] = "true";
      }
    }
    configureListValidatorRequest(event);
  }
  document.addEventListener("htmx:configRequest", handleRefreshConfigRequest);
  function handleRefreshBeforeRequest(event) {
    liveBeforeListRequest(event);
    const detail = requestDetail2(event);
    const elt = Object(detail.elt);
    if (elt.id === "resource-list-content") {
      if (detail.xhr) {
        containerListRequestsInFlight.add(detail.xhr);
      }
      return;
    }
    if (!isUserListRequest(event)) {
      return;
    }
    if (detail.xhr) {
      userListRequestsInFlight.add(detail.xhr);
    }
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx3();
    if (content && htmx2) {
      htmx2.trigger(content, "htmx:abort");
    }
  }
  document.addEventListener("htmx:beforeRequest", handleRefreshBeforeRequest);
  function handleRefreshBeforeSend(event) {
    liveMarkListRequestSent(event);
  }
  document.addEventListener("htmx:beforeSend", handleRefreshBeforeSend);
  function handleRefreshBeforeSwap(event) {
    liveBeforeListSwapDecision(event);
  }
  document.addEventListener("htmx:beforeSwap", handleRefreshBeforeSwap);
  function handleRefreshSwapError(event) {
    liveListRequestSwapFailed(event);
  }
  document.addEventListener("htmx:swapError", handleRefreshSwapError);
  function handleRefreshAfterRequest(event) {
    liveAfterListRequest(event);
    const xhr = requestDetail2(event).xhr;
    userListRequestsInFlight.delete(xhr);
    containerListRequestsInFlight.delete(xhr);
  }
  document.addEventListener("htmx:afterRequest", handleRefreshAfterRequest);
  function parseRefreshSeconds(mode) {
    if (typeof mode !== "string") {
      return 0;
    }
    const unsigned = mode.startsWith("+") ? mode.slice(1) : mode;
    const seconds = Array.from(unsigned).reduce((value, char) => {
      const digit = char.charCodeAt(0) - 48;
      return digit >= 0 && digit <= 9 ? value * 10 + digit : Number.NaN;
    }, 0);
    return Number.isSafeInteger(seconds) ? seconds : 0;
  }
  function refreshMode() {
    const stored = readPrefs().refresh;
    if (stored) {
      return stored;
    }
    let legacy = null;
    try {
      legacy = window.localStorage.getItem(REFRESH_KEY);
    } catch {
    }
    if (!legacy) {
      return "";
    }
    const secs = parseRefreshSeconds(legacy);
    const mode = secs > 0 ? String(secs) : "Off";
    roPrefsSetRefresh(mode);
    return mode;
  }
  function listTableURL() {
    const u = new URL(window.location.href);
    return `${u.pathname.replace(/\/+$/, "")}/_table${u.search}`;
  }
  function requestListRefresh() {
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx3();
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
    const mode = refreshMode();
    return effectivePollSeconds(mode, parseRefreshSeconds(mode), liveFallbackSeconds());
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
    const delayMs = delay * 1e3;
    refreshNextAt = Date.now() + delayMs;
    refreshTimerId = window.setTimeout(() => {
      refreshTimerId = null;
      scheduleRefreshTick();
      fireRefresh();
    }, delayMs);
    updateStaleCountdown();
  }
  function pauseRefresh() {
    if (refreshTimerId !== null) {
      window.clearTimeout(refreshTimerId);
      refreshTimerId = null;
    }
    refreshNextAt = 0;
    updateStaleCountdown();
  }
  function applyRefresh() {
    refreshFailureStage = 0;
    scheduleRefreshTick();
  }
  function syncRefreshUI() {
    const mode = refreshMode();
    const live = mode === "Live";
    const secs = parseRefreshSeconds(mode);
    const label = document.getElementById("refresh-label");
    if (label) {
      if (live) {
        label.textContent = "Live";
      } else if (secs > 0) {
        label.textContent = `${secs}s`;
      } else {
        label.textContent = "Off";
      }
    }
    document.querySelectorAll('[data-ro-action="set-refresh"]').forEach((opt) => {
      const value = opt.dataset.roInterval;
      opt.classList.toggle(
        "is-active",
        value !== void 0 && (live ? value === "Live" : value !== "Live" && parseRefreshSeconds(value) === secs)
      );
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
  function handleRefreshVisibilityChange() {
    if (!document.hidden && effectivePollSeconds2() > 0) {
      fireRefresh();
    }
  }
  document.addEventListener("visibilitychange", handleRefreshVisibilityChange);
  var refreshBindings = [
    // Stale-banner retry: re-fire the (read-only) auto-refresh GET on
    // #resource-list-content through the shared refresh path (the v2 loop derives
    // the `_table` URL from location.href at click time; the v1 multi-type
    // container triggers its baked ro:refresh). On success the morph swaps fresh
    // rows and the afterSwap handler clears the stale dim + re-hides the banner;
    // on another failure the responseError handler keeps it stale. An in-flight
    // container request (a HUNG tick is exactly the state this button exists for)
    // is aborted first -- issuing a second container request would make htmx
    // QUEUE it, and a queued request replays on the next htmx:abort with its stale
    // queue-time URL (no queue may ever form). Pure DOM, GET-only -- the
    // read-only floor is untouched.
    {
      event: "click",
      selector: '[data-ro-action="retry"]',
      stop: true,
      handler: (event) => {
        event.preventDefault();
        const content = document.getElementById("resource-list-content");
        const htmx2 = getHtmx3();
        if (content && htmx2) {
          htmx2.trigger(content, "htmx:abort");
        }
        requestListRefresh();
        return true;
      }
    },
    // Auto-refresh interval option (navbar #refresh-dropdown): persist the chosen
    // mode in the ro_prefs cookie, re-arm the poll, and reflect it in the
    // control. The Live option persists the literal 'Live' and rides
    // the same path: liveApply opens/tears down the stream, applyRefresh then arms
    // the poll chain per the EFFECTIVE seconds (0 while a stream is riding). A
    // disabled Live option (multi-type/multi-cluster page) never fires (the
    // browser suppresses clicks on disabled buttons). The dropdown opens through
    // CSS hover/focus, so there is no open/close handler here -- only the
    // selection. Kept its early-return (stop:true).
    {
      event: "click",
      selector: '[data-ro-action="set-refresh"]',
      stop: true,
      handler: (event, matched) => {
        const option = matched;
        if (option.dataset.roInterval === "Live") {
          roPrefsSetRefresh("Live");
        } else {
          const interval = parseRefreshSeconds(option.dataset.roInterval);
          roPrefsSetRefresh(interval > 0 ? String(interval) : "Off");
        }
        liveApply(true);
        syncRefreshUI();
        applyRefresh();
        option.blur();
        event.preventDefault();
        return true;
      }
    }
  ];

  // internal/assets/src/js/virtualizer-math.ts
  var VIRT_BUFFER_ROWS = 12;
  function windowBounds(tbodyTop, innerHeight, rowH, visibleCount, buffer = VIRT_BUFFER_ROWS) {
    const pitch = rowH || 1;
    const n = visibleCount;
    const first = Math.floor((0 - tbodyTop) / pitch);
    const last = Math.ceil((innerHeight - tbodyTop) / pitch);
    const start = Math.min(n, Math.max(0, first - buffer));
    const end = Math.max(start, Math.min(n, last + buffer));
    return { start, end };
  }
  function spacerHeights(start, end, visibleCount, rowH) {
    return {
      top: start * rowH,
      bottom: Math.max(0, visibleCount - end) * rowH
    };
  }
  function prepareSwapSpacers(priorStart, incomingRowCount, rowH) {
    const start = Math.min(priorStart, incomingRowCount);
    return {
      top: start * rowH,
      bottom: Math.max(0, incomingRowCount - start) * rowH
    };
  }
  function rowOffsetTop(tbodyTop, index, rowH) {
    return tbodyTop + index * rowH;
  }
  function scrollAdjustToReveal(rowTop, rowH, topMin, innerHeight) {
    const topOverflow = rowTop - topMin;
    if (topOverflow < 0) {
      return topOverflow;
    }
    return Math.max(0, rowTop + rowH - innerHeight);
  }
  function clampFocusIndex(current, delta, visibleCount) {
    return Math.max(0, Math.min(visibleCount - 1, current + delta));
  }

  // internal/assets/src/js/virtualizer.ts
  var FILTER_HIDE_CLASS2 = "ro-row-filtered";
  function roRowState() {
    return window.roRowState;
  }
  var virtState = {
    active: false,
    visible: [],
    rowH: 0,
    start: 0,
    end: 0,
    table: null,
    tbody: null,
    topSpacer: null,
    bottomSpacer: null,
    pinnedWidths: [],
    pendingScrollY: null
  };
  var historyRecoveryPending = null;
  function virtualizerActive() {
    return virtState.active && virtState.tbody?.isConnected === true;
  }
  function takeVirtualizerCheckpoint() {
    return {
      // Filter reconciliation replaces `visible`; it never mutates this
      // array or the width pins in place. Retaining the references makes the
      // common one-row Live transaction O(1) before its intentional rewindow.
      state: { ...virtState },
      historyRecovery: historyRecoveryPending
    };
  }
  function restoreVirtualizerCheckpoint(checkpoint) {
    Object.assign(virtState, checkpoint.state);
    historyRecoveryPending = checkpoint.historyRecovery;
  }
  function virtReset() {
    virtState.active = false;
    virtState.visible = [];
    virtState.rowH = 0;
    virtState.start = 0;
    virtState.end = 0;
    virtState.table = null;
    virtState.tbody = null;
    virtState.topSpacer = null;
    virtState.bottomSpacer = null;
    virtState.pinnedWidths = [];
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
    const rendered = virtState.tbody.querySelectorAll(
      ":scope > tr[data-key]"
    );
    if (rendered.length === 0) {
      return 0;
    }
    const first = rendered[0].getBoundingClientRect();
    const last = rendered[rendered.length - 1].getBoundingClientRect();
    const pitch = (last.bottom - first.top) / rendered.length;
    return Math.max(0, pitch);
  }
  function virtFallbackRowHeight() {
    let py = 9;
    let lh = 18;
    try {
      const cs = window.getComputedStyle(document.documentElement);
      py = parseFloat(cs.getPropertyValue("--row-py")) || py;
      const cell = virtState.tbody?.querySelector("td");
      if (cell) {
        lh = parseFloat(window.getComputedStyle(cell).lineHeight) || lh;
      }
    } catch {
    }
    return py * 2 + lh + 1;
  }
  function virtApplyPins() {
    const ths = virtState.table.querySelectorAll("thead th");
    if (virtState.pinnedWidths.length !== ths.length) {
      return false;
    }
    ths.forEach((th, i) => {
      th.style.width = `${virtState.pinnedWidths[i]}px`;
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
    virtState.visible = listProjectionVisibleRows();
  }
  function virtRenderWindow() {
    const s = virtState;
    const tbody = s.tbody;
    const rect = tbody.getBoundingClientRect();
    const bounds = windowBounds(rect.top, window.innerHeight, s.rowH, s.visible.length);
    s.start = bounds.start;
    s.end = bounds.end;
    const heights = spacerHeights(s.start, s.end, s.visible.length, s.rowH);
    s.topSpacer.firstElementChild.style.height = `${heights.top}px`;
    s.bottomSpacer.firstElementChild.style.height = `${heights.bottom}px`;
    const slice = s.visible.slice(s.start, s.end);
    slice.forEach((tr) => {
      tr.classList.remove(FILTER_HIDE_CLASS2);
    });
    tbody.replaceChildren(s.topSpacer, ...slice, s.bottomSpacer);
    reapplyRowState();
  }
  function virtBindMounts() {
    const content = document.getElementById("resource-list-content");
    const wrap = content?.querySelector(".ro-table-wrap.ro-windowed");
    const table = wrap?.querySelector("table.ro-table") ?? null;
    const tbody = table?.tBodies.item(0) ?? null;
    virtState.table = table;
    virtState.tbody = tbody;
    return tbody !== null;
  }
  function virtualizeInit() {
    const content = document.getElementById("resource-list-content");
    const wrap = content?.querySelector(".ro-table-wrap.ro-windowed");
    if (!content) {
      resetListProjection();
      virtReset();
      return;
    }
    if (!wrap) {
      ensureListProjection(content);
      virtReset();
      return;
    }
    const table = wrap.querySelector("table.ro-table");
    const tbody = table?.tBodies.item(0) ?? null;
    if (!tbody) {
      ensureListProjection(content);
      virtReset();
      return;
    }
    if (tbody.querySelector(":scope > tr.ro-vspacer")) {
      if (virtState.active && virtState.tbody === tbody) {
        return;
      }
      if (historyRecoveryPending?.content === content && historyRecoveryPending.tbody === tbody) {
        return;
      }
      virtReset();
      ensureListProjection(content);
      historyRecoveryPending = { content, tbody };
      clearListValidator();
      requestListRefresh();
      return;
    }
    ensureListProjection(content);
    const rows = listProjectionRows();
    if (rows.length === 0) {
      virtReset();
      return;
    }
    historyRecoveryPending = null;
    virtReset();
    virtState.table = table;
    virtState.tbody = tbody;
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
    virtState.pendingScrollY = null;
    const incoming = prepareListProjectionSwap(fragment);
    if (!incoming.windowed || incoming.rows.length === 0) {
      return;
    }
    const wrap = fragment.querySelector(".ro-table-wrap.ro-windowed");
    const tbody = wrap ? wrap.querySelector("table.ro-table tbody") : null;
    if (!tbody) {
      return;
    }
    virtState.pendingScrollY = window.scrollY;
    const rowH = virtState.rowH || virtFallbackRowHeight();
    const priorStart = virtState.active ? virtState.start : 0;
    const heights = prepareSwapSpacers(priorStart, incoming.rows.length, rowH);
    const topSpacer = virtMakeSpacer();
    const bottomSpacer = virtMakeSpacer();
    topSpacer.firstElementChild.style.height = `${heights.top}px`;
    bottomSpacer.firstElementChild.style.height = `${heights.bottom}px`;
    tbody.replaceChildren(topSpacer, bottomSpacer);
  }
  function virtualizeAfterSwap() {
    historyRecoveryPending = null;
    const wasActive = virtState.active;
    const previousByKey = commitListProjectionSwap();
    if (!previousByKey || !listProjectionWindowed() || listProjectionRows().length === 0) {
      virtReset();
      return;
    }
    if (!virtBindMounts()) {
      resetListProjection();
      virtReset();
      return;
    }
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
    virtFlashChangedCells(previousByKey);
  }
  function virtFlashChangedCells(prior) {
    if (prior.size === 0 || window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      return;
    }
    virtState.tbody.querySelectorAll(":scope > tr[data-key]").forEach((tr) => {
      const old = prior.get(tr.dataset.key);
      if (!old) {
        return;
      }
      Array.from(tr.children).forEach((newCell, index) => {
        const oldCell = old.children.item(index);
        if (oldCell && newCell.tagName === "TD" && oldCell.textContent !== newCell.textContent) {
          newCell.classList.remove("ro-cell-changed");
          void newCell.offsetWidth;
          newCell.classList.add("ro-cell-changed");
        }
      });
    });
  }
  function virtualizeOnFilterChange() {
    if (!virtualizerActive() || listProjectionSwapPending()) {
      return;
    }
    virtComputeVisible();
    virtRenderWindow();
  }
  function virtMoveFocus(delta) {
    const list = virtState.visible;
    if (list.length === 0) {
      return false;
    }
    const focusKey = roRowState().focusedKey();
    const current = list.findIndex((row) => row.dataset.key === focusKey);
    const next = clampFocusIndex(current, delta, list.length);
    virtualizeScrollToIndex(next);
    roRowState().setFocus(list[next].dataset.key);
    return true;
  }
  function virtualizeScrollToIndex(index) {
    const rect = virtState.tbody.getBoundingClientRect();
    const rowTop = rowOffsetTop(rect.top, index, virtState.rowH);
    const topbar = document.querySelector("header.ro-topbar");
    const topMin = topbar ? topbar.getBoundingClientRect().bottom : 0;
    const delta = scrollAdjustToReveal(rowTop, virtState.rowH, topMin, window.innerHeight);
    if (delta !== 0) {
      window.scrollBy(0, delta);
    }
    virtRenderWindow();
  }
  function virtRows() {
    return virtualizerActive() ? Array.from(listProjectionRows()) : [];
  }
  function virtVisible() {
    return virtState.visible;
  }
  function virtRowByKey(key) {
    return virtualizerActive() ? listProjectionRowByKey(key) : null;
  }
  var virtScrollScheduled = false;
  function virtOnScroll() {
    if (!virtualizerActive()) {
      return;
    }
    const rect = virtState.tbody.getBoundingClientRect();
    const bounds = windowBounds(
      rect.top,
      window.innerHeight,
      virtState.rowH,
      virtState.visible.length
    );
    if (bounds.start !== virtState.start || bounds.end !== virtState.end) {
      virtRenderWindow();
    }
  }
  window.addEventListener(
    "scroll",
    () => {
      if (!virtState.active || virtScrollScheduled) {
        return;
      }
      virtScrollScheduled = true;
      window.requestAnimationFrame(() => {
        virtScrollScheduled = false;
        virtOnScroll();
      });
    },
    { passive: true }
  );
  window.addEventListener("resize", virtOnScroll);
  var fontReady = document.fonts?.ready;
  if (fontReady && typeof fontReady.then === "function") {
    void fontReady.then(() => {
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
      const tr = listProjectionRowByKey(key);
      const index = tr ? virtState.visible.indexOf(tr) : -1;
      if (index === -1) {
        return false;
      }
      virtualizeScrollToIndex(index);
      return true;
    }
  };

  // internal/assets/src/js/morph.ts
  var idiomorph = typeof Idiomorph !== "undefined" && typeof Idiomorph.morph === "function" ? Idiomorph : void 0;
  if (idiomorph?.defaults?.callbacks && !window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
    const PRIOR = /* @__PURE__ */ new WeakMap();
    idiomorph.defaults.callbacks.beforeNodeMorphed = (oldNode) => {
      if (oldNode.nodeName !== "TD") {
        return;
      }
      PRIOR.set(oldNode, oldNode.textContent);
    };
    idiomorph.defaults.callbacks.afterNodeMorphed = (oldNode) => {
      if (!PRIOR.has(oldNode)) {
        return;
      }
      const el = oldNode;
      const before = PRIOR.get(oldNode);
      PRIOR.delete(oldNode);
      if (before !== el.textContent) {
        el.classList.remove("ro-cell-changed");
        void el.offsetWidth;
        el.classList.add("ro-cell-changed");
      }
    };
  }
  if (typeof htmx !== "undefined" && typeof htmx.defineExtension === "function" && idiomorph) {
    htmx.defineExtension("ro-morph", {
      isInlineSwap: (swapStyle) => swapStyle === "morph",
      handleSwap: (swapStyle, target, fragment) => {
        if (swapStyle !== "morph") {
          return false;
        }
        if (target.id === "resource-list-content") {
          prepareListProjectionSwap(fragment);
          virtualizePrepareSwap(fragment);
        }
        return idiomorph.morph(target, fragment.children, {
          morphStyle: "innerHTML",
          ignoreActiveValue: true
        });
      }
    });
  }

  // internal/assets/src/js/bulk-actions.ts
  var bulkCopyResetTimer = 0;
  function bulkCopyNames(button) {
    const entries = roRowState2().selectedEntries();
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
    if (!bar?.dataset.bulkHref) {
      return;
    }
    const entries = roRowState2().selectedEntries();
    if (entries.length === 0 || entries.length > BULK_NAMES_MAX) {
      return;
    }
    const cluster = bar.dataset.bulkCluster;
    const names = entries.map((entry) => {
      if (bar.dataset.bulkAllns === "true" && cluster && entry.key.startsWith(`${cluster}/`)) {
        return entry.key.slice(cluster.length + 1);
      }
      return entry.name;
    });
    window.location.assign(`${bar.dataset.bulkHref}&names=${encodeURIComponent(names.join(","))}`);
  }
  function roRowState2() {
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

  // internal/assets/src/js/columns.ts
  function getHtmx4() {
    return window.htmx;
  }
  var colsPopOpenFlag = false;
  function colsPopOpen() {
    return colsPopOpenFlag;
  }
  function setColsPopOpen(open) {
    colsPopOpenFlag = open;
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
    colsPopOpenFlag = !!pop && pop.classList.contains("is-open");
  }
  function commitColumnVisibility(pop) {
    if (!pop) {
      return;
    }
    const plural = pop.dataset.plural || "";
    if (!plural) {
      return;
    }
    const hidden = [];
    pop.querySelectorAll('[data-ro-action="toggle-column"]').forEach((toggle) => {
      const check = toggle.querySelector(".ro-check");
      if (!toggle.disabled && check && !check.checked && toggle.dataset.col) {
        hidden.push(toggle.dataset.col);
      }
    });
    roPrefsSetHiddenColumns(plural, hidden);
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx4();
    if (content && htmx2) {
      htmx2.trigger(content, "htmx:abort");
    }
    requestListRefresh();
  }
  function popFormMergedHref(form) {
    const owned = /* @__PURE__ */ new Set();
    const fields = [];
    Array.prototype.slice.call(form.elements).forEach((el) => {
      if (el.tagName !== "INPUT" || el.type === "hidden" || !el.name) {
        return;
      }
      owned.add(el.name);
      if (el.value) {
        fields.push(`${el.name}=${encodeURIComponent(el.value)}`);
      }
    });
    return mergeColParams(window.location.pathname, window.location.search, owned, fields);
  }
  var columnsBindings = [
    // Column-visibility popover: the ⊞ title-row button toggles the popover
    // open/closed. Open state is derived from the DOM (a boosted body swap
    // renders it closed). NOT stop:true -- C4's own [data-ro-cols-toggle] guard
    // (the outside-click binding below) keeps the double-fire single, not a stop
    // signal (listener-inventory C1/C4: both see the same click, no propagation
    // stop between them).
    {
      event: "click",
      selector: "[data-ro-cols-toggle]",
      handler: (event) => {
        event.preventDefault();
        const pop = document.getElementById("ro-cols-pop");
        setColsPopOpen(!!pop && !pop.classList.contains("is-open"));
      }
    },
    // A column checkbox row: flip the checkbox optimistically, then commit the
    // COMPLETE hidden set (as the user now sees it) to the ro_prefs cookie and
    // re-render through the container's own programmatic path -- cookie-state,
    // not URL-state: RO-No-Push, zero history entries. The identity row
    // is a disabled <button>, so its clicks never fire.
    {
      event: "click",
      selector: '[data-ro-action="toggle-column"]',
      handler: (event, matched) => {
        event.preventDefault();
        const toggle = matched;
        const check = toggle.querySelector(".ro-check");
        if (check) {
          check.checked = !check.checked;
        }
        commitColumnVisibility(toggle.closest(".ro-pop"));
        return true;
      },
      stop: true
    },
    // C4: a click outside the popover (and not on its ⊞ opener) closes it -- the
    // same dismissal contract the autocomplete dropdown uses. The
    // [data-ro-cols-toggle] escape: when the ⊞ toggle is clicked WHILE open, the
    // toggle binding above already set colsPopOpen=false (closed), and this
    // guard makes this binding a no-op so it does NOT re-toggle (no double-fire /
    // no reopen). No selector (it keys off the flag + the closest() escapes).
    {
      event: "click",
      handler: (event) => {
        if (!colsPopOpenFlag) {
          return;
        }
        const t = event.target;
        if (t.closest("#ro-cols-pop") || t.closest("[data-ro-cols-toggle]")) {
          return;
        }
        setColsPopOpen(false);
      }
    },
    // form.ro-pop-form (the popover's labelcols/selector form): intercept and
    // MERGE into the live query, riding the v2 loop exactly like a chip commit
    // (issueFilterNavigation falls back to a plain navigation when the loop is
    // unavailable). The native submit would rebuild the query from the round-trip
    // hidden inputs alone and wipe every `?f=` chip.
    {
      event: "submit",
      selector: "form.ro-pop-form",
      handler: (event, matched) => {
        event.preventDefault();
        const popForm = matched;
        issueFilterNavigation(popFormMergedHref(popForm));
        return true;
      },
      stop: true
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
      const item = menu.querySelector(`[data-ro-action="${action}"]`);
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
    bind("open", tr.dataset.href);
    bind("yaml", tr.dataset.yaml);
    bind("logs", tr.dataset.logs);
    bind("download", tr.dataset.download);
    const key = tr.dataset.key;
    const name = tr.dataset.name || (key ? lastKeySegment(key) : void 0);
    if (name) {
      menu.dataset.name = name;
    } else {
      delete menu.dataset.name;
    }
    menu.style.left = `${Math.max(8, Math.min(x, window.innerWidth - CTX_CLAMP_W))}px`;
    menu.style.top = `${Math.max(8, Math.min(y, window.innerHeight - CTX_CLAMP_H))}px`;
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
      selector: "#ro-ctxmenu [data-ro-action]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const item = matched;
        const menu = item.closest("#ro-ctxmenu");
        closeRowMenu();
        if (item.dataset.roAction === "copy") {
          const name = menu?.dataset.name;
          if (name !== void 0) {
            roCopyText(name, () => {
            });
          }
        } else {
          const href = item.dataset.href;
          if (href) {
            window.location.assign(href);
          }
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

  // internal/assets/src/js/keyboard.ts
  var PALETTE_ID = "ro-palette";
  function roRowState3() {
    return window.roRowState;
  }
  function keyboardTargetIsTextEntry(target) {
    if (!(target instanceof HTMLElement)) {
      return false;
    }
    return target.matches("input, textarea, select") || target.isContentEditable;
  }
  function keyboardSurfaceBusy() {
    const palette = document.getElementById(PALETTE_ID);
    if (palette?.classList.contains("open")) {
      return true;
    }
    const menu = document.getElementById("ro-ctxmenu");
    if (menu?.classList.contains("is-open")) {
      return true;
    }
    const nsDropdown = document.getElementById("namespace-dropdown");
    if (nsDropdown?.classList.contains("is-active")) {
      return true;
    }
    return colsPopOpen();
  }
  function visibleKeyRows() {
    return Array.from(
      document.querySelectorAll("#resource-list-content tbody tr[data-key]")
    ).filter((tr) => !tr.classList.contains("ro-row-filtered"));
  }
  function moveRowFocus(delta) {
    if (virtualizerActive()) {
      return virtMoveFocus(delta);
    }
    const rows = visibleKeyRows();
    if (rows.length === 0) {
      return false;
    }
    const focusKey = roRowState3().focusedKey();
    const current = rows.findIndex((tr) => tr.dataset.key === focusKey);
    const next = Math.max(0, Math.min(rows.length - 1, current + delta));
    roRowState3().setFocus(rows[next].dataset.key);
    rows[next].scrollIntoView({ block: "nearest" });
    return true;
  }
  function openFocusedRow() {
    const key = roRowState3().focusedKey();
    if (!key) {
      return false;
    }
    let row = visibleKeyRows().find((tr) => tr.dataset.key === key) ?? null;
    if (!row && virtualizerActive()) {
      const tr = virtRowByKey(key);
      if (tr && virtVisible().includes(tr)) {
        row = tr;
      }
    }
    if (!row?.dataset.href) {
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
    if (prior && document.contains(prior)) {
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
        if (event.target === kbdOverlayEl()) {
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
          if (target instanceof Element && target.closest("a, button, summary")) {
            return;
          }
          if (openFocusedRow()) {
            e.preventDefault();
          }
        }
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
    // Logs Follow toggle: the active accent "Following" sticks the stream
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
    // Logs display toggles: CLIENT-SIDE only, no refetch. The timestamps
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

  // internal/assets/src/js/yaml-folds.ts
  function yamlEffectiveIndent(text) {
    const stripped = text.replace(/^\n+/, "");
    const indent = stripped.length - stripped.replace(/^ +/, "").length;
    const rest = stripped.slice(indent);
    if (rest === "-" || rest.startsWith("- ") || rest.startsWith("-	")) {
      return indent + 2;
    }
    return indent;
  }
  function planYamlFolds(indents, isBlank) {
    const bodyCounts = new Array(indents.length).fill(0);
    const ownersByLine = Array.from({ length: indents.length }, () => []);
    const active = [];
    let previous;
    indents.forEach((indent, index) => {
      if (isBlank[index]) {
        return;
      }
      const firstClosed = active.findIndex((owner) => owner.indent >= indent);
      if (firstClosed !== -1) {
        active.splice(firstClosed);
      }
      if (previous && previous.indent < indent) {
        active.push(previous);
      }
      active.forEach((owner) => {
        ownersByLine[index].push(owner.index);
        bodyCounts[owner.index] += 1;
      });
      previous = { index, indent };
    });
    return { bodyCounts, ownersByLine };
  }
  function yamlCodeText(codeCell) {
    const controlSelector = '[data-ro-action="toggle-fold"], [data-ro-fold-control]';
    if (!codeCell.querySelector(controlSelector)) {
      return codeCell.textContent;
    }
    const clone = codeCell.cloneNode(true);
    clone.querySelectorAll(controlSelector).forEach((el) => {
      el.remove();
    });
    return clone.textContent;
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
      const owners = line.dataset.foldOf.split(" ");
      if (owners.indexOf(id) !== -1) {
        line.classList.toggle("ro-line-folded", folded);
      }
    });
  }
  function injectFoldControls(lineSpan, bodyCount) {
    const toggle = document.createElement("button");
    toggle.type = "button";
    toggle.className = "ro-fold-toggle";
    toggle.dataset.roAction = "toggle-fold";
    toggle.setAttribute("aria-expanded", "true");
    toggle.setAttribute("aria-label", "Toggle block");
    toggle.dataset.fold = lineSpan.id;
    const note = document.createElement("span");
    note.className = "ro-fold-note";
    note.dataset.roFoldControl = "note";
    const lineWord = bodyCount === 1 ? "line" : "lines";
    note.textContent = ` … ${bodyCount} ${lineWord}`;
    const anchor = lineSpan.querySelector("a");
    if (anchor?.nextSibling) {
      lineSpan.insertBefore(toggle, anchor.nextSibling);
    } else if (anchor) {
      lineSpan.appendChild(toggle);
    } else {
      lineSpan.insertBefore(toggle, lineSpan.firstChild);
    }
    const last = lineSpan.lastChild;
    if (last?.nodeType === Node.TEXT_NODE && last.data.includes("\n")) {
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
        const texts = lines.map((line) => line.textContent);
        const indents = texts.map(yamlEffectiveIndent);
        const isBlank = texts.map((text) => text.trim() === "");
        const { bodyCounts, ownersByLine } = planYamlFolds(indents, isBlank);
        lines.forEach((line, index) => {
          const owners = ownersByLine[index];
          if (owners.length === 0) {
            return;
          }
          const element = line;
          const ownerIds = owners.map((owner) => lines[owner].id).join(" ");
          element.dataset.foldOf = ownerIds;
        });
        lines.forEach((line, index) => {
          if (bodyCounts[index] > 0) {
            injectFoldControls(line, bodyCounts[index]);
          }
        });
      } catch (_e) {
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
    // data-ro-action="toggle-fold" (NESTED YAML block fold): toggle the deeper-indented child
    // lines of a `key:`/`- key:` block in place. Matched BEFORE the section-fold
    // + gutter-anchor handlers (registration order) so a nested-fold click never
    // collapses the whole section or jumps a line anchor. The monolith called
    // preventDefault + stopPropagation + return; we keep stopPropagation (inert
    // for document siblings per the inventory, but preserved 1:1) and stop:true
    // mirrors the early return.
    {
      event: "click",
      selector: '[data-ro-action="toggle-fold"]',
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

  // internal/assets/src/js/misc-ui.ts
  function collapseSectionsFromHash() {
    parseCollapsedNames(document.location.hash).forEach((name) => {
      document.querySelectorAll(`main .collapsible[data-name="${CSS.escape(name)}"]`).forEach((el) => {
        el.classList.add("is-collapsed");
      });
    });
  }
  var miscBindings = [
    // Mobile hamburger: a delegated click on [data-ro-action="toggle-sidebar"] reveals/hides the
    // sidebar by toggling `.is-active` on `.ro-sidebar`. No-op when no sidebar is
    // present (e.g. the Clusters entry page). Kept its early-return (stop:true).
    {
      event: "click",
      selector: '[data-ro-action="toggle-sidebar"]',
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
    // data-ro-action="copy" (per-section YAML copy): copy THIS section's raw YAML to the
    // clipboard via navigator.clipboard.writeText -- CSP-clean. The raw text is
    // read from the section's Pygments `td.code` cell (the gutter lives in a
    // separate `td.linenos`), with any injected fold controls stripped first
    // (yamlCodeText) so the copy is the full source YAML in any fold state. The
    // button briefly flips its label to "copied". Matched (and stop:true) BEFORE
    // the section-fold binding so a copy click never toggles the section fold.
    {
      event: "click",
      selector: '[data-ro-action="copy"]',
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const copyBtn = matched;
        const section = copyBtn.closest(".collapsible");
        const codeCell = section?.querySelector(".highlighttable td.code");
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
        if (navigator.clipboard?.writeText && text) {
          navigator.clipboard.writeText(text).then(
            () => done(true),
            () => done(false)
          );
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
          window.history.replaceState(
            null,
            document.title,
            window.location.pathname + window.location.search
          );
        }
        return true;
      }
    },
    // Namespace switch: picking a namespace in the topbar dropdown records it
    // as this cluster's last-used namespace in the ro_prefs cookie (server-read
    // only, for cluster-entry hrefs -- never a redirect). The click is
    // deliberately NOT prevented; the boosted navigation proceeds. The cookie
    // write rides the prefs.ts surface directly (the same seam legacy uses).
    // Kept its early-return (stop:true).
    {
      event: "click",
      selector: '#namespace-dropdown [data-ro-action="pick-namespace"]',
      stop: true,
      handler: (_event, matched) => {
        const href = matched.getAttribute("href");
        const hrefMatch = href ? /^\/clusters\/([^/]+)\/namespaces\/([^/]+)\//.exec(href) : null;
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
    // #namespace-searchbox input: filter the [data-ro-action="pick-namespace"] links by
    // case-insensitive substring. Terminal branch in the monolith input listener
    // (no branch followed it), reproduced as stop:true.
    {
      event: "input",
      selector: "#namespace-searchbox",
      stop: true,
      handler: (_event, matched) => {
        const filterText = matched.value.toLowerCase();
        document.querySelectorAll('[data-ro-action="pick-namespace"]').forEach((element) => {
          const text = element.innerText.toLowerCase();
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
        const firstVisible = Array.from(
          document.querySelectorAll('[data-ro-action="pick-namespace"]')
        ).find((element) => !element.classList.contains("is-hidden"));
        firstVisible?.click();
        return true;
      }
    },
    // In-cell +N overflow (label/selector chips and data keys): the `.ro-chip.more[data-ro-more]` button
    // toggles `.expanded` on its OWN `.ro-chips` strip, revealing the `.xtra` chips
    // in place (the button face flips +N <-> "less" in CSS). Delegated so it
    // survives every morph; aria-expanded mirrors the state. A refresh morph
    // re-renders the strip collapsed (server truth) -- expansion is a transient
    // peek, not persisted state. Was a trailing branch of the monolith big click
    // listener (C1); kept its early-return (stop:true).
    {
      event: "click",
      selector: "[data-ro-more]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const chips = matched.closest(".ro-chips");
        if (chips) {
          const expanded = chips.classList.toggle("expanded");
          matched.setAttribute("aria-expanded", expanded ? "true" : "false");
        }
        return true;
      }
    },
    // Long-annotation toggle: a >120-char annotation renders as a
    // collapsed `key · size` button + a hidden scrollable <pre> payload. The
    // delegated click flips the [hidden] attribute on the sibling .anno-pre,
    // mirrors the state into aria-expanded, and rotates the chevron via the .open
    // class -- CSP-clean and morph-safe (server truth re-renders collapsed; a
    // transient peek, like the chip overflow above). C1 trailing branch;
    // early-return (stop:true).
    {
      event: "click",
      selector: "[data-ro-annolong]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const annoToggle = matched;
        const pre = annoToggle.parentElement ? annoToggle.parentElement.querySelector(".anno-pre") : null;
        if (pre) {
          const open = pre.hidden !== false;
          pre.hidden = !open;
          annoToggle.setAttribute("aria-expanded", open ? "true" : "false");
          annoToggle.classList.toggle("open", open);
        }
        return true;
      }
    },
    // data-ro-action="toggle-tools": toggle `is-active` on the control itself and on the element
    // named by its `data-target`. C1 trailing branch; early-return (stop:true).
    {
      event: "click",
      selector: '[data-ro-action="toggle-tools"]',
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const toggle = matched;
        toggle.classList.toggle("is-active");
        const targetEl = toggle.dataset.target ? document.getElementById(toggle.dataset.target) : null;
        if (targetEl) {
          targetEl.classList.toggle("is-active");
        }
        return true;
      }
    },
    // Search-button enable (change): a checkbox carries `data-ro-toggle-button="<id>"`.
    // The named button is enabled iff any checkbox sharing that same value is
    // checked, else disabled. Was the lead branch of the monolith change listener;
    // early-return (stop:true).
    {
      event: "change",
      selector: "input[data-ro-toggle-button]",
      stop: true,
      handler: (_event, matched) => {
        const buttonId = matched.dataset.roToggleButton;
        const button = buttonId ? document.getElementById(buttonId) : null;
        if (button) {
          const anyChecked = document.querySelectorAll(`input[data-ro-toggle-button="${buttonId}"]:checked`).length > 0;
          button.disabled = !anyChecked;
        }
        return true;
      }
    },
    // form.tools-form (the v1 multi-type tools form): on submit, blank the `name`
    // of empty inputs so they do not become empty query parameters in the
    // resulting GET URL. Sole branch of the monolith submit listener; it did NOT
    // early-return (the form still submits), so this binding does NOT stop.
    {
      event: "submit",
      selector: "form.tools-form",
      handler: (_event, matched) => {
        const form = matched;
        Array.prototype.slice.call(form.getElementsByTagName("input")).forEach((input) => {
          if (input.name && !input.value) {
            input.name = "";
          }
        });
      }
    }
  ];

  // internal/assets/src/js/palette-rank.ts
  var WORD_SEPARATORS = " -_./:";
  function isAsciiUppercase(character) {
    return character >= "A" && character <= "Z";
  }
  function roFuzzyScore(query, text) {
    const source = text;
    const q = query.toLowerCase();
    const t = source.toLowerCase();
    if (!q) {
      return 0;
    }
    const first = t.indexOf(q[0]);
    if (first === -1) {
      return -1;
    }
    let from = first + 1;
    for (let i = 1; i < q.length; i++) {
      const at = t.indexOf(q[i], from);
      if (at === -1) {
        return -1;
      }
      from = at + 1;
    }
    const gaps = from - first - q.length;
    let tier = 2;
    if (gaps === 0) {
      if (first === 0) {
        tier = 0;
      } else {
        const separatorBoundary = WORD_SEPARATORS.includes(t[first - 1]);
        const camelHump = isAsciiUppercase(source[first]) && !isAsciiUppercase(source[first - 1]);
        if (separatorBoundary || camelHump) {
          tier = 1;
        }
      }
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
    return entry.href ? `href:${entry.href}` : `action:${entry.action}`;
  }
  function dedupeRecents(prior, entry, max) {
    const kept = prior.filter((it) => paletteRecentTarget(it) !== paletteRecentTarget(entry));
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
    const q = query.trim();
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

  // internal/assets/src/js/palette.ts
  var PALETTE_ID2 = "ro-palette";
  window.roFuzzy = roFuzzyScore;
  function asPropertyRecord(value) {
    return Object(value);
  }
  function nonEmptyString(value) {
    if (typeof value !== "string") {
      return void 0;
    }
    const trimmed = value.trim();
    return trimmed.length > 0 ? trimmed : void 0;
  }
  function feedEntryLabel2(entry, kindEntry) {
    return kindEntry ? nonEmptyString(entry.kind) ?? nonEmptyString(entry.plural) : nonEmptyString(entry.name) ?? nonEmptyString(entry.label);
  }
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
    try {
      const data = asPropertyRecord(JSON.parse(el.textContent));
      const records = (value, kindEntries = false) => Array.isArray(value) ? value.map(asPropertyRecord).filter((entry) => feedEntryLabel2(entry, kindEntries) !== void 0).filter(
        (entry) => paletteHrefSafe(entry.href) !== "" || nonEmptyString(entry.action) !== void 0
      ) : [];
      return {
        currentCluster: nonEmptyString(data.currentCluster) ?? null,
        currentNamespace: nonEmptyString(data.currentNamespace) ?? null,
        clusters: records(data.clusters),
        namespaces: records(data.namespaces),
        kinds: records(data.kinds, true),
        actions: records(data.actions)
      };
    } catch {
      return empty;
    }
  }
  function paletteHrefSafe(href, pageHref = window.location.href) {
    if (typeof href !== "string") {
      return "";
    }
    const trimmed = href.trim();
    try {
      const page = new URL(pageHref);
      const parsed = new URL(trimmed, page);
      const networkProtocol = parsed.protocol === "http:" || parsed.protocol === "https:";
      return networkProtocol && parsed.origin === page.origin ? trimmed : "";
    } catch {
      return "";
    }
  }
  var PALETTE_RECENTS_KEY = "ro-pref-recents";
  var PALETTE_RECENTS_MAX = 5;
  function readPaletteRecents() {
    try {
      const raw = window.localStorage.getItem(PALETTE_RECENTS_KEY);
      if (!raw) {
        return [];
      }
      const list = JSON.parse(raw);
      const recents = [];
      const seen = /* @__PURE__ */ new Set();
      for (const value of list) {
        const record = asPropertyRecord(value);
        const label = nonEmptyString(record.label);
        if (!label) {
          continue;
        }
        const href = paletteHrefSafe(record.href);
        const action = nonEmptyString(record.action);
        if (!href && !action) {
          continue;
        }
        const recent = { label, href: href || void 0, action };
        const target = paletteRecentTarget(recent);
        if (seen.has(target)) {
          continue;
        }
        seen.add(target);
        recents.push(recent);
        if (recents.length === PALETTE_RECENTS_MAX) {
          break;
        }
      }
      return recents;
    } catch {
      return [];
    }
  }
  function recordPaletteRecent(label, href, action) {
    if (!label || !href && !action) {
      return;
    }
    const entry = { label, href, action };
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
  function createPaletteRow() {
    const row = document.createElement("div");
    row.className = "ro-pal-item";
    row.dataset.roAction = "pick-palette-row";
    row.setAttribute("role", "option");
    return row;
  }
  function appendPaletteLabel(row, text) {
    const label = document.createElement("span");
    label.className = "pal-label";
    label.textContent = text;
    row.appendChild(label);
    return label;
  }
  function buildPaletteRow(entry, key) {
    const row = createPaletteRow();
    const kindRow = key === "kinds";
    const icon = nonEmptyString(entry.icon);
    if (kindRow && icon) {
      const holder = document.createElement("template");
      holder.innerHTML = icon;
      row.appendChild(holder.content);
    }
    const labelText = feedEntryLabel2(entry, kindRow);
    const display = nonEmptyString(entry.display) ?? labelText;
    const label = appendPaletteLabel(row, display);
    if (display !== labelText) {
      row.title = labelText;
    }
    const entryName = nonEmptyString(entry.name);
    const currentScope = key === "clusters" ? paletteScope.cluster : key === "namespaces" ? paletteScope.namespace : null;
    if (entryName && entryName === currentScope) {
      const ctx = document.createElement("span");
      ctx.className = "pal-ctx";
      ctx.textContent = "current";
      label.appendChild(ctx);
    }
    if (kindRow) {
      const meta = document.createElement("span");
      meta.className = "pal-meta";
      meta.textContent = nonEmptyString(entry.group) ?? "core";
      row.appendChild(meta);
      const scope = document.createElement("span");
      const namespaced = entry.namespaced === true;
      scope.className = `pal-scope ${namespaced ? "ns" : "cluster"}`;
      scope.textContent = namespaced ? "namespaced" : "cluster";
      row.appendChild(scope);
    }
    const href = paletteHrefSafe(entry.href);
    if (href) {
      row.dataset.href = href;
    }
    const action = nonEmptyString(entry.action);
    if (action) {
      row.dataset.action = action;
    }
    row.dataset.label = labelText;
    return row;
  }
  function buildEverywhereRow(query) {
    const row = createPaletteRow();
    const glyph = document.querySelector(`#${PALETTE_ID2} .ro-pal-search .ico`);
    if (glyph) {
      row.appendChild(glyph.cloneNode(true));
    }
    const labelText = `Search all clusters for “${query}”`;
    appendPaletteLabel(row, labelText);
    row.dataset.href = `/search?q=${encodeURIComponent(query)}`;
    row.dataset.label = labelText;
    return row;
  }
  var PALETTE_STATUS_TONES = ["ok", "warn", "err", "info", "mute"];
  function harvestPageObjects() {
    const out = [];
    const rows = virtualizerActive() ? virtRows() : document.querySelectorAll("#resource-list-content table.ro-table tbody tr");
    Array.prototype.forEach.call(rows, (tr) => {
      const a = tr.querySelector("td.cell-name a");
      if (!a) {
        return;
      }
      const href = paletteHrefSafe(a.getAttribute("href"));
      const name = a.textContent.trim();
      if (!href || !name) {
        return;
      }
      const object = { name, href };
      const st = tr.querySelector(".cell-status");
      if (st) {
        object.status = st.textContent.trim();
        object.tone = PALETTE_STATUS_TONES.find(
          (candidate) => st.classList.contains(candidate)
        );
      }
      out.push(object);
    });
    return out;
  }
  function buildObjectRow(o) {
    const row = createPaletteRow();
    appendPaletteLabel(row, o.name);
    if (o.status) {
      const st = document.createElement("span");
      st.className = `pal-status${o.tone ? ` ${o.tone}` : ""}`;
      st.textContent = o.status;
      row.appendChild(st);
    }
    row.dataset.href = o.href;
    row.dataset.label = o.name;
    return row;
  }
  function renderPalette(query) {
    const list = document.getElementById("ro-palette-list");
    if (!list) {
      return;
    }
    const data = readPaletteData();
    paletteScope.cluster = data.currentCluster;
    paletteScope.namespace = data.currentNamespace;
    const scope = document.getElementById("ro-palette-scope");
    if (scope) {
      const scopeText = paletteScope.namespace ?? paletteScope.cluster;
      scope.textContent = scopeText ?? "";
      scope.hidden = scopeText === null;
    }
    const q = query;
    list.replaceChildren();
    paletteRows = [];
    const rowFor = (item, key) => {
      switch (key) {
        case "everywhere":
          return buildEverywhereRow(item.query);
        case "objects":
          return buildObjectRow(item);
        default:
          return buildPaletteRow(item, key);
      }
    };
    const groups = buildPaletteGroups(
      q,
      {
        clusters: data.clusters,
        namespaces: data.namespaces,
        kinds: data.kinds,
        actions: data.actions
      },
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
        paletteRows.push({ el: row });
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
    paletteActive = index;
    paintPaletteActive();
  }
  function movePaletteActive(delta) {
    const length = paletteRows.length;
    paletteActive = (paletteActive + delta + length) % length;
    paintPaletteActive();
  }
  function choosePaletteRow(rowEl) {
    const action = rowEl.dataset.action;
    const href = rowEl.dataset.href;
    recordPaletteRecent(rowEl.dataset.label, href, action);
    closePalette();
    if (action === "theme") {
      document.getElementById("btn-theme-toggle")?.click();
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
  var paletteRestoringFocus;
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
    const prior = palettePriorFocus;
    palettePriorFocus = null;
    if (!prior || !document.contains(prior) || palette.contains(prior)) {
      return;
    }
    const focus = prior.focus;
    if (typeof focus !== "function") {
      return;
    }
    paletteRestoringFocus = true;
    try {
      focus.call(prior);
    } finally {
      paletteRestoringFocus = void 0;
    }
  }
  var paletteBindings = [
    // ⌘K palette result row: a click on a result row activates it (navigate or
    // run its named action, then close). FIRST so a click inside the open
    // palette never falls through to a page handler. (C1 head, returned.)
    {
      event: "click",
      selector: '[data-ro-action="pick-palette-row"]',
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        choosePaletteRow(matched);
        return true;
      }
    },
    // The read-only topbar search box ([data-ro-palette-open]) opens the palette on
    // click instead of typing inline. (C1, returned.)
    {
      event: "click",
      selector: "[data-ro-palette-open]",
      stop: true,
      handler: (event) => {
        event.preventDefault();
        openPalette();
        return true;
      }
    },
    // The search page's "Refine · ⌘K" button: open the palette PREFILLED
    // with the query the page searched (server-baked data-query). (C1, returned.)
    {
      event: "click",
      selector: "[data-ro-search-refine]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        openPalette(matched.dataset.query);
        return true;
      }
    },
    // A click on the palette backdrop ITSELF (the dimmed area outside the panel)
    // closes it, like Esc. A click inside the panel does not match. The selector
    // is the backdrop root id; the handler still verifies that the original
    // target is that matched root, so a descendant click does NOT close it.
    {
      event: "click",
      selector: `#${PALETTE_ID2}`,
      stop: true,
      handler: (event, matched) => {
        if (event.target === matched) {
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
        if (!palette?.classList.contains("open")) {
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
      selector: "[data-ro-palette-open]",
      handler: (event) => {
        if (paletteRestoringFocus) {
          return;
        }
        openPalette();
        const t = event.target;
        t.blur();
      }
    }
  ];

  // internal/assets/src/js/bindings.ts
  var bindings = [
    ...contextMenuBindings,
    ...bulkBindings,
    ...rowSelectionBindings,
    ...columnsBindings,
    ...filtersBindings,
    ...paletteBindings,
    ...keyboardBindings,
    ...foldBindings,
    ...logsBindings,
    // misc-ui's click bindings keep their relative monolith order: copy is
    // registered before the section-fold binding (copy stop:true short-circuits
    // a copy click), so a copy click never folds its section. misc-ui now also
    // carries the trailing presentation toggles ([data-ro-more] / [data-ro-annolong] /
    // [data-ro-action="toggle-tools"]) and the v1 form glue (the data-ro-toggle-button
    // change + the tools-form submit) lifted out of the dismantled legacy.js.
    ...miscBindings,
    // refresh-domain tails LAST: the retry + set-refresh hooks were
    // the monolith big click listener's own trailing branches, so registering
    // them after the migrated leaves preserves the C1 order -- every leaf
    // front-ran the monolith, and these ran at its end. Neither co-matches any
    // selector above, so the position is observationally free; LAST documents
    // their monolith origin.
    ...refreshBindings
  ];

  // internal/assets/src/js/events.ts
  function closestElement(event, selector) {
    const target = event.target;
    const element = target instanceof Element ? target : target?.parentElement;
    return element?.closest(selector) ?? null;
  }
  function dispatch(bindings2, event) {
    for (const binding of bindings2) {
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

  // internal/assets/src/js/register-bindings.ts
  registerBindings(bindings);

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
    const input = form?.querySelector('input[name="theme"]');
    if (input) {
      input.value = PREFERS_DARK.matches ? "light" : "dark";
    }
  }
  PREFERS_DARK.addEventListener("change", syncThemeTogglePostTarget);

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
    content.replaceChildren(...Array.from(template.children, (node) => node.cloneNode(true)));
  });
  function clearListSkeleton() {
    const content = document.getElementById("resource-list-content");
    if (!content) {
      return;
    }
    content.querySelectorAll(":scope > .ro-skel").forEach((skeleton) => {
      skeleton.remove();
    });
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

  // internal/assets/src/js/init.ts
  window.roToast = showToast;
  function suppressRedundantActiveNavigation(event) {
    const detail = Object(event.detail);
    const source = detail.elt;
    const trigger = detail.triggeringEvent;
    if (detail.boosted !== true || detail.target !== document.body || detail.verb !== "get" || !(source instanceof HTMLAnchorElement) || !(trigger instanceof MouseEvent) || trigger.type !== "click" || trigger.button !== 0 || trigger.altKey || trigger.ctrlKey || trigger.metaKey || trigger.shiftKey) {
      return;
    }
    if (!source.classList.contains("is-active") && !source.parentElement?.classList.contains("is-active")) {
      return;
    }
    const rawHref = source.getAttribute("href");
    if (!rawHref || rawHref.includes("#") || typeof detail.path !== "string") {
      return;
    }
    try {
      const current = new URL(window.location.href);
      const resolvesToCurrentLocation = (candidate) => {
        const resolved = new URL(candidate, current);
        return resolved.origin === current.origin && resolved.pathname === current.pathname && resolved.search === current.search && resolved.hash === current.hash;
      };
      if (resolvesToCurrentLocation(rawHref) && resolvesToCurrentLocation(detail.path)) {
        event.preventDefault();
      }
    } catch {
    }
  }
  document.addEventListener("htmx:configRequest", suppressRedundantActiveNavigation);
  function handleSortPreferenceRequest(event) {
    const detail = Object(event.detail);
    const rawCfg = Object(detail.requestConfig);
    const cfg = {
      ...rawCfg,
      headers: Object(rawCfg.headers)
    };
    const target = Object(detail.target);
    if (target.id !== "resource-list-content") {
      return;
    }
    if (cfg.headers["RO-No-Push"] || cfg.headers["HX-Preloaded"] === "true") {
      return;
    }
    const elt = Object(detail.elt);
    let sortHeader = null;
    try {
      sortHeader = elt.closest("thead th");
    } catch {
    }
    if (!sortHeader) {
      return;
    }
    let plural;
    let sort;
    try {
      const requestURL = new URL(String(cfg.path), window.location.href);
      const rawSort = requestURL.searchParams.get("sort");
      if (!rawSort) {
        return;
      }
      const pathMatch = /\/([^/]+)\/_table$/.exec(requestURL.pathname);
      if (!pathMatch) {
        return;
      }
      sort = rawSort;
      plural = decodeURIComponent(pathMatch[1]);
    } catch {
      return;
    }
    roPrefsSetSort(plural, sort);
  }
  document.addEventListener("htmx:beforeRequest", handleSortPreferenceRequest);
  document.addEventListener("htmx:afterSwap", (event) => {
    const bodySwapped = event.target === document.body;
    if (bodySwapTicket && bodySwapped) {
      if (bodySwapTicket.phase === "swap") {
        completeBodySwap();
      } else {
        reloadCurrentHistoryEntry();
      }
    }
    if (isListRefreshEvent(event)) {
      rememberListValidator(event);
      noteRefreshRecovery();
      clearListStale();
      reapplyRowState();
      applyLiveNameFilter();
      const filterInput = document.getElementById("ro-filter-input");
      if (filterInput && document.activeElement === filterInput && filterInput.value) {
        updateFilterAC();
      }
      if (colsPopOpen()) {
        setColsPopOpen(true);
      }
      virtualizeAfterSwap();
      liveOnListSwap(event);
    }
    if (bodySwapped) {
      runInit();
    }
  });
  document.addEventListener("htmx:beforeSwap", (event) => {
    const detail = event.detail;
    if (suppressListNotModified(event)) {
      noteRefreshRecovery();
      clearListStale();
      return;
    }
    if (detail && detail.target === document.body) {
      if (bodySwapTicket || bodyReloading) {
        event.preventDefault();
        reloadCurrentHistoryEntry();
        return;
      }
      const status = detail.xhr?.status;
      if (typeof status === "number" && status >= 400 && status <= 599) {
        detail.shouldSwap = true;
      }
      const ticket = claimBodySwap("normal", "swap", null);
      closeRowMenu();
      clearRowState();
      clearListStale();
      liveTeardown();
      pauseRefresh();
      liveResetPage();
      liveState.status = "idle";
      liveState.streamPath = "";
      queueMicrotask(() => {
        if (!event.defaultPrevented && detail.shouldSwap !== false) return;
        reloadFailedBodySwap(ticket);
      });
    }
  });
  var bodySwapTicket = null;
  var bodyReloading;
  function clearBodySwap() {
    bodySwapTicket = null;
  }
  function completeBodySwap() {
    clearBodySwap();
    bodyReloading = void 0;
  }
  function retireCurrentScreenForBodySwap() {
    liveTeardown();
    pauseRefresh();
    liveResetPage();
    liveState.status = "idle";
    liveState.streamPath = "";
  }
  function reloadCurrentHistoryEntry() {
    if (bodyReloading) return;
    if (!bodySwapTicket) retireCurrentScreenForBodySwap();
    clearBodySwap();
    bodyReloading = true;
    window.history.go(0);
  }
  function reloadFailedBodySwap(ticket) {
    if (!ticket || bodySwapTicket !== ticket) return;
    reloadCurrentHistoryEntry();
  }
  function claimBodySwap(kind, phase, xhr) {
    const ticket = { kind, phase, xhr };
    bodySwapTicket = ticket;
    return ticket;
  }
  function beginHistoryBodySwap(event) {
    if (bodySwapTicket || bodyReloading) {
      event.preventDefault();
      reloadCurrentHistoryEntry();
      return;
    }
    const miss = event.type === "htmx:historyCacheMiss";
    const detail = Object(event.detail);
    const xhr = miss && detail.xhr instanceof EventTarget ? detail.xhr : null;
    const ticket = claimBodySwap(miss ? "miss" : "hit", miss ? "request" : "swap", xhr);
    retireCurrentScreenForBodySwap();
    queueMicrotask(() => {
      if (event.defaultPrevented) reloadFailedBodySwap(ticket);
    });
    if (miss) {
      if (!xhr) {
        event.preventDefault();
        return;
      }
      xhr.addEventListener("loadend", () => {
        if (ticket.phase === "request") {
          reloadFailedBodySwap(ticket);
        }
      });
    }
  }
  document.addEventListener("htmx:historyCacheHit", beginHistoryBodySwap);
  document.addEventListener("htmx:historyCacheMiss", beginHistoryBodySwap);
  document.addEventListener("htmx:historyCacheMissLoad", (event) => {
    const detail = Object(event.detail);
    const ticket = bodySwapTicket;
    if (ticket?.kind !== "miss" || ticket.phase !== "request" || detail.xhr !== ticket.xhr) {
      event.preventDefault();
      reloadCurrentHistoryEntry();
      return;
    }
    ticket.phase = "swap";
  });
  document.addEventListener("htmx:historyCacheMissLoadError", reloadCurrentHistoryEntry);
  document.addEventListener("htmx:swapError", (event) => {
    const detail = Object(event.detail);
    if (event.target === document.body || detail.target === document.body) {
      reloadFailedBodySwap(bodySwapTicket);
    }
  });
  document.addEventListener("htmx:historyRestore", () => {
  });
  window.addEventListener("pageshow", () => {
    completeBodySwap();
  });
  function setupStickyNamespace() {
    document.querySelectorAll(".ro-table-wrap table.ro-table").forEach((table) => {
      const firstCell = table.querySelector("tbody tr:not(.ro-vspacer) td:first-child");
      if (firstCell?.classList.contains("cell-ns")) {
        table.style.setProperty(
          "--ns-col-w",
          `${firstCell.getBoundingClientRect().width}px`
        );
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
    if (bodySwapTicket || bodyReloading) return;
    [
      syncRefreshUI,
      buildYamlFolds,
      collapseSectionsFromHash,
      highlightYamlLine,
      initLogsFollow,
      syncThemeTogglePostTarget,
      setupStickyNamespace,
      // Chips-editor row model: captured from the full server-rendered
      // document. ORDER CONTRACT: this step must stay BEFORE the windowing
      // init that prunes rows from the DOM -- at this point
      // the DOM still IS the complete dataset.
      captureRowModelFromDocument,
      // A new projection deliberately clears stale visibleKeys. Re-derive
      // them from the current draft before windowing so navigation/history
      // cannot carry an old page's filter set into this one.
      applyLiveNameFilter,
      // Virtualization engagement: windows the >threshold
      // table the server marked `.ro-windowed`. AFTER the model capture,
      // per the order contract above.
      virtualizeInit,
      // Columns-popover open flag: re-derived from the fresh DOM so a
      // boosted body swap (rendered closed) never leaves a stale-open flag.
      syncColsPopState,
      // Row state is keyed by OBJECT identity; the store clears when an
      // hx-boost navigation swaps the body (the htmx:beforeSwap hook above),
      // so this init re-paint scrubs any stale is-selected classes a
      // cached/boosted body carried in -- and the bulk bar re-syncs to the
      // same store right after.
      reapplyRowState,
      updateBulkBar,
      // Live opens only after every synchronous body/model repair. In
      // particular, virtualizeInit may detect a history-restored viewport
      // slice and synchronously issue the mandatory full `_table` rebuild;
      // its beforeRequest ownership must exist before liveApply decides
      // whether to open or suspend. Keep liveApply immediately BEFORE
      // applyRefresh so the poll chain still arms against the resulting Live
      // state: a riding stream disarms it, a fallback selects 5s.
      liveApply,
      applyRefresh
    ].forEach(runInitStep);
  }
  document.addEventListener("DOMContentLoaded", runInit);
  document.addEventListener("htmx:afterSettle", setupStickyNamespace);
  window.addEventListener("resize", setupStickyNamespace);
})();
