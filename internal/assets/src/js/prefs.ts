// prefs.ts -- the `ro_prefs` preference cookie codec (D9), THE pref write path
// (the server only reads). Extracted from legacy.js as the first typed module.
//
// One compact cookie persists column visibility per plural, sort per plural,
// the auto-refresh mode, and a last-used namespace per cluster, so SSR renders
// the persisted state without a double paint. Wire format (pinned, mirrored by
// internal/web/prefs.go -- the canonical reference): `ro_prefs=v1.<base64url(
// JSON)>`; raw JSON is cookie-unsafe (column names like "Nominated Node" carry
// spaces, JSON carries quotes/commas). Payload shape:
//   { kinds: [{ k, sort?, hide? }...],   // most-recent-first per-plural entries
//     refresh: 'Off'|'5'|...|'Live',     // stringly so Live needs no migration
//     ns: { cluster: namespace } }       // '_all' is a valid value
// Writes happen ONLY on direct user interactions (sort click, column toggle,
// interval pick, namespace switch) -- never because a URL arrived with explicit
// params, and never for programmatic traffic. Attributes: Path=/; SameSite=Lax;
// Max-Age=31536000, Secure on https, NOT HttpOnly (this script writes it). No
// server write path exists -- the read-only edge keeps its GET-only surface.
// Above the 3KB encoded cap, kind entries evict from the array TAIL (least
// recently used; the writers below move a touched entry to the front --
// deterministic, no timestamps).
//
// The encode/decode functions (encodePrefsValue, decodePrefsValue) are PURE
// string<->payload transforms with NO DOM: node:test exercises them directly
// against the SAME golden fixtures the Go codec uses (internal/web/testdata/
// prefs_golden). readPrefs/writePrefs are the thin document.cookie wrappers.

// Prefs is the NORMALIZED decoded payload. The field/key insertion order
// (kinds, refresh, ns) and inner order (k, sort, hide) mirror the Go struct
// field order in prefs.go -- byte-for-byte JSON.stringify identity depends on
// it.
export interface KindPrefs {
    k: string;
    sort?: string;
    hide?: string[];
}

export interface Prefs {
    kinds: KindPrefs[];
    refresh: string;
    ns: Record<string, string>;
}

export const PREFS_COOKIE = 'ro_prefs';
export const PREFS_VERSION_PREFIX = 'v1.';
export const PREFS_MAX_ENCODED = 3072;
export const PREFS_COOKIE_MAX_AGE = 31536000; // one year, in seconds

// REFRESH_KEY is the LEGACY v1 localStorage home of the interval choice. It is
// no longer written; refreshMode() reads it ONCE as a migration fallback into
// the ro_prefs cookie (D9).
export const REFRESH_KEY = 'roRefresh';

// b64urlEncodeUTF8 / b64urlDecodeUTF8: base64url (URL-safe alphabet, no
// padding) over the UTF-8 bytes of a string -- TextEncoder/TextDecoder keep
// multi-byte column names (CRD printer columns) intact through btoa/atob,
// matching Go's base64.RawURLEncoding byte-for-byte. TextEncoder/TextDecoder
// are global in both the browser and Node 24 (no polyfill); btoa/atob are too.
function b64urlEncodeUTF8(text: string): string {
    const bytes = new TextEncoder().encode(text);
    let bin = '';
    for (let i = 0; i < bytes.length; i++) {
        bin += String.fromCharCode(bytes[i]);
    }
    return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function b64urlDecodeUTF8(encoded: string): string {
    const b64 = encoded.replace(/-/g, '+').replace(/_/g, '/');
    const bin = atob(b64 + '===='.slice(b64.length % 4 || 4));
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) {
        bytes[i] = bin.charCodeAt(i);
    }
    return new TextDecoder().decode(bytes);
}

// decodePrefsValue is the PURE decode half: it parses a raw cookie VALUE
// ("v1.<base64url>") into a NORMALIZED prefs object. Lenient by design
// (matching the server's decodePrefs): a missing/foreign-version/corrupt value
// yields empty prefs with ok=false, never a throw -- the next write simply
// starts fresh. ok lets callers (and the corrupt-decode golden fixtures)
// distinguish "no usable cookie" from "decoded to empty".
//
// INNER fields are type-checked one by one, not passed through: the Go reader
// (prefs.go decodePrefs) is all-or-nothing -- json.Unmarshal rejects the WHOLE
// payload when one field is mistyped (e.g. {"kinds":[{"k":"pods","sort":5}]}),
// so a passthrough here would keep perpetuating a cookie SSR can never apply.
// Dropping just the mistyped field means the very next JS write re-encodes a
// clean cookie and the two readers converge again (self-heal).
export function decodePrefsValue(value: string): { prefs: Prefs; ok: boolean } {
    const empty: Prefs = { kinds: [], refresh: '', ns: {} };
    if (!value || value.indexOf(PREFS_VERSION_PREFIX) !== 0) {
        return { prefs: empty, ok: false };
    }
    const payload = value.slice(PREFS_VERSION_PREFIX.length);
    if (!payload) {
        return { prefs: empty, ok: false };
    }
    try {
        const decoded = JSON.parse(b64urlDecodeUTF8(payload));
        if (!decoded || typeof decoded !== 'object') {
            return { prefs: empty, ok: false };
        }
        const kinds: KindPrefs[] = [];
        if (Array.isArray(decoded.kinds)) {
            decoded.kinds.forEach((e: any) => {
                if (!e || typeof e !== 'object' || typeof e.k !== 'string') {
                    return;
                }
                const entry: KindPrefs = { k: e.k };
                if (typeof e.sort === 'string') {
                    entry.sort = e.sort;
                }
                if (Array.isArray(e.hide) && e.hide.every((name: unknown) => typeof name === 'string')) {
                    entry.hide = e.hide;
                }
                kinds.push(entry);
            });
        }
        const ns: Record<string, string> = {};
        if (decoded.ns && typeof decoded.ns === 'object' && !Array.isArray(decoded.ns)) {
            Object.keys(decoded.ns).forEach((cluster) => {
                if (typeof decoded.ns[cluster] === 'string') {
                    ns[cluster] = decoded.ns[cluster];
                }
            });
        }
        return {
            prefs: {
                kinds: kinds,
                refresh: typeof decoded.refresh === 'string' ? decoded.refresh : '',
                ns: ns,
            },
            ok: true,
        };
    } catch (e) {
        return { prefs: empty, ok: false };
    }
}

// encodePrefsValue is the PURE encode half: it renders the cookie VALUE,
// evicting kind entries from the array tail while the encoded value exceeds the
// 3KB cap (the entries are most-recent-first, so the least recently used kinds
// drop first). Never mutates the caller's arrays. The output is pure ASCII, so
// value.length (UTF-16 code units) equals the byte length the Go cap measures.
export function encodePrefsValue(prefs: Prefs): string {
    const out: { kinds?: KindPrefs[]; refresh?: string; ns?: Record<string, string> } = {};
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
        out.kinds = out.kinds.slice(0, -1); // D9 eviction: drop the tail kind
        if (out.kinds.length === 0) {
            delete out.kinds;
        }
        value = PREFS_VERSION_PREFIX + b64urlEncodeUTF8(JSON.stringify(out));
    }
    return value;
}

// --- thin document.cookie wrappers (DOM) ----------------------------------

function prefsCookieValue(): string {
    const parts = document.cookie ? document.cookie.split('; ') : [];
    for (let i = 0; i < parts.length; i++) {
        if (parts[i].indexOf(PREFS_COOKIE + '=') === 0) {
            return parts[i].slice(PREFS_COOKIE.length + 1);
        }
    }
    return '';
}

// readPrefs reads the cookie and decodes it (always returns a usable prefs
// object -- a corrupt/absent cookie self-heals to empty prefs on the next
// write).
export function readPrefs(): Prefs {
    return decodePrefsValue(prefsCookieValue()).prefs;
}

export function writePrefs(prefs: Prefs): void {
    try {
        let cookie = PREFS_COOKIE + '=' + encodePrefsValue(prefs)
            + '; Path=/; SameSite=Lax; Max-Age=' + PREFS_COOKIE_MAX_AGE;
        if (window.location.protocol === 'https:') {
            cookie += '; Secure';
        }
        document.cookie = cookie;
    } catch (e) {
        // cookies unavailable -> the preference just will not persist
    }
}

// prefsTouchKind finds-or-creates the entry for a plural and moves it to the
// FRONT (most-recent-first -- the order tail eviction relies on).
function prefsTouchKind(prefs: Prefs, plural: string): KindPrefs {
    for (let i = 0; i < prefs.kinds.length; i++) {
        if (prefs.kinds[i].k === plural) {
            const entry = prefs.kinds.splice(i, 1)[0];
            prefs.kinds.unshift(entry);
            return entry;
        }
    }
    const fresh: KindPrefs = { k: plural };
    prefs.kinds.unshift(fresh);
    return fresh;
}

// roPrefsSetSort persists a sort param ("Name", "Status:desc", ...) for a
// plural. Called from the sort-header write hook in legacy.js.
export function roPrefsSetSort(plural: string, sort: string): void {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).sort = sort;
    writePrefs(prefs);
}

// roPrefsSetHiddenColumns is the COLUMN-VISIBILITY write surface: the D8
// columns popover commits through it (commitColumnVisibility). names is the
// COMPLETE hidden-column list for the plural as the user sees it -- an EMPTY
// array is an explicit "hide nothing" that the server distinguishes from "no
// preference" (it suppresses the DefaultHiddenColumns config default).
export function roPrefsSetHiddenColumns(plural: string, names: string[]): void {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).hide = Array.isArray(names) ? names : [];
    writePrefs(prefs);
}

// roPrefsSetRefresh persists the auto-refresh mode ('Off', seconds-as-string,
// future 'Live') -- the interval picker writes through it; Unit 27's Live mode
// will too.
export function roPrefsSetRefresh(mode: string): void {
    const prefs = readPrefs();
    prefs.refresh = mode;
    writePrefs(prefs);
}

// roPrefsSetNamespace records the last-used namespace for a cluster ('_all'
// included). Consumed server-side ONLY for cluster-entry href construction
// (the clusters page rows + the palette cluster nav) -- never redirects.
export function roPrefsSetNamespace(cluster: string, namespace: string): void {
    if (!cluster || !namespace) {
        return;
    }
    const prefs = readPrefs();
    prefs.ns[cluster] = namespace;
    writePrefs(prefs);
}
