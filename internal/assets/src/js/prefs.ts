// prefs.ts -- the `ro_prefs` preference cookie codec, THE pref write path
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
// string<->payload transforms with NO DOM: Vitest exercises them directly
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

export const PREFS_MAX_ENCODED = 3072;
const PREFS_COOKIE_MAX_AGE = 31536000; // one year, in seconds

// b64urlEncodeUTF8 / b64urlDecodeUTF8: base64url (URL-safe alphabet, no
// padding) over the UTF-8 bytes of a string -- TextEncoder/TextDecoder keep
// multi-byte column names (CRD printer columns) intact through btoa/atob,
// matching Go's base64.RawURLEncoding byte-for-byte. TextEncoder/TextDecoder
// are global in both the browser and Node 24 (no polyfill); btoa/atob are too.
function b64urlEncodeUTF8(text: string): string {
    const bytes = new TextEncoder().encode(text);
    const bin = Array.from(bytes, (byte) => String.fromCharCode(byte)).join('');
    return btoa(bin).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
}

function b64urlDecodeUTF8(encoded: string): string {
    // Go's RawURLEncoding accepts only the URL-safe alphabet without `=`
    // padding (while ignoring CR/LF). Browser atob is deliberately more
    // forgiving: it also accepts padded and standard `+/` base64, so validate
    // the wire alphabet before translating it for atob.
    const compact = encoded.replaceAll('\r', '').replaceAll('\n', '');
    if (!/^[A-Za-z0-9_-]*$/.test(compact)) {
        // The public decoder intentionally collapses every wire failure to
        // ok=false, so there is no observable error message to preserve here.
        throw new TypeError();
    }
    const b64 = compact.replaceAll('-', '+').replaceAll('_', '/');
    // atob uses the platform's forgiving base64 decoder: valid unpadded input
    // with a remainder of 2 or 3 is accepted directly. Base64url emitted from
    // real bytes can never have the invalid remainder of 1, so manufacturing
    // padding here added mutation-prone arithmetic without changing behavior.
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (char) => char.charCodeAt(0));
    // Preserve a leading UTF-8 BOM as U+FEFF. TextDecoder's default strips it,
    // while Go passes those bytes to json.Unmarshal, which rejects a BOM before
    // the JSON value. JSON.parse likewise rejects the preserved U+FEFF.
    return new TextDecoder('utf-8', { ignoreBOM: true }).decode(bytes);
}

function isRecord(value: unknown): value is Record<string, unknown> {
    return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// Assignment to an ordinary object's `__proto__` key invokes the legacy
// prototype setter instead of creating a cluster entry. Define every decoded
// or newly-written namespace key as an own data property so special names
// (`__proto__`, `constructor`, `toString`, ...) round-trip without changing the
// record's prototype.
function setOwnNamespace(ns: Record<string, string>, cluster: string, namespace: string): void {
    Object.defineProperty(ns, cluster, {
        value: namespace,
        enumerable: true,
        configurable: true,
        writable: true,
    });
}

// encoding/json orders string map keys with strings.Compare: lexicographically
// over their UTF-8 bytes. JavaScript's default sort compares UTF-16 code units,
// and ordinary-object enumeration imposes a separate numeric-index order, so
// neither can define the canonical `ns` wire order. Sort encoded key bytes and
// render the small map directly to preserve Go's byte-exact map ordering for
// valid Unicode keys, including `__proto__` and numeric-looking cluster names.
function compareUTF8(left: Uint8Array, right: Uint8Array): number {
    // subarray clamps its end to left.length, giving the shared prefix without
    // a mutable min/max choice.
    const mismatch = left
        .subarray(0, right.length)
        .findIndex((byte, index) => byte !== right[index]);
    if (mismatch !== -1) {
        return left[mismatch] - right[mismatch];
    }
    return left.length - right.length;
}

// encoding/json escapes the JSONP-unsafe U+2028/U+2029 code points even when
// Encoder.SetEscapeHTML(false); JSON.stringify leaves them literal. Apply that
// final wire-level difference to every serialized prefs string/container while
// retaining JSON.stringify's desired literal <, >, and & behavior.
function stringifyPrefsJSON(value: string | KindPrefs[]): string {
    // Both members of the closed input union always have a JSON representation.
    const encoded = JSON.stringify(value) as string;
    return encoded.replaceAll('\u2028', '\\u2028').replaceAll('\u2029', '\\u2029');
}

function canonicalNamespaceJSON(ns: Record<string, string>): string {
    const encoder = new TextEncoder();
    const entries = Object.entries(ns).map(([cluster, namespace]) => ({
        cluster,
        namespace,
        encodedCluster: encoder.encode(cluster),
    }));
    entries.sort((left, right) => compareUTF8(left.encodedCluster, right.encodedCluster));
    return `{${entries
        .map(
            ({ cluster, namespace }) =>
                `${stringifyPrefsJSON(cluster)}:${stringifyPrefsJSON(namespace)}`,
        )
        .join(',')}}`;
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
export function decodePrefsValue(value?: string): { prefs: Prefs; ok: boolean } {
    const empty: Prefs = { kinds: [], refresh: '', ns: {} };
    const prefix = 'v1.';
    if (!value?.startsWith(prefix)) {
        return { prefs: empty, ok: false };
    }
    const payload = value.slice(prefix.length);
    try {
        const decoded: unknown = JSON.parse(b64urlDecodeUTF8(payload));
        if (!isRecord(decoded)) {
            return { prefs: empty, ok: false };
        }
        const kinds: KindPrefs[] = [];
        if (Array.isArray(decoded.kinds)) {
            decoded.kinds.forEach((raw: unknown) => {
                if (!isRecord(raw)) {
                    return;
                }
                // raw is an untyped JSON object; narrow each field on read (Go's
                // decodePrefs rejects the WHOLE payload on one mistyped field, so
                // the JS reader keeps only well-typed fields -> the next write
                // self-heals the cookie). The field guards below are the pinned
                // needle contract (prefs_test.go), kept verbatim.
                const e = raw as { k?: unknown; sort?: unknown; hide?: unknown };
                if (typeof e.k !== 'string') {
                    return;
                }
                const entry: KindPrefs = { k: e.k };
                if (typeof e.sort === 'string') {
                    entry.sort = e.sort;
                }
                if (
                    Array.isArray(e.hide) &&
                    e.hide.every((name: unknown) => typeof name === 'string')
                ) {
                    entry.hide = e.hide;
                }
                kinds.push(entry);
            });
        }
        const ns: Record<string, string> = {};
        if (isRecord(decoded.ns)) {
            Object.entries(decoded.ns).forEach(([cluster, namespace]) => {
                if (typeof namespace === 'string') {
                    setOwnNamespace(ns, cluster, namespace);
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
    } catch (_e) {
        return { prefs: empty, ok: false };
    }
}

// encodePrefsValue is the PURE encode half: it renders the cookie VALUE,
// evicting kind entries from the array tail while the encoded value exceeds the
// 3KB cap (the entries are most-recent-first, so the least recently used kinds
// drop first). Never mutates the caller's arrays. The output is pure ASCII, so
// value.length (UTF-16 code units) equals the byte length the Go cap measures.
function encodePrefsCandidate(
    kinds: KindPrefs[],
    refresh: string,
    ns: Record<string, string>,
): string {
    const fields: string[] = [];
    if (kinds.length > 0) {
        fields.push(`"kinds":${stringifyPrefsJSON(kinds)}`);
    }
    if (refresh) {
        fields.push(`"refresh":${stringifyPrefsJSON(refresh)}`);
    }
    if (Object.keys(ns).length > 0) {
        fields.push(`"ns":${canonicalNamespaceJSON(ns)}`);
    }
    const payload = b64urlEncodeUTF8(`{${fields.join(',')}}`);
    return `v1.${payload}`;
}

export function encodePrefsValue(prefs: Partial<Prefs>): string {
    // Golden/wire payloads may omit empty fields even though normalized callers
    // always carry them. Normalize that sparse input before encoding.
    const kinds = prefs.kinds ?? [];
    const refresh = prefs.refresh ?? '';
    const ns = prefs.ns ?? {};
    // Base64url payload lengths jump over 3072 (3071 -> 3073). Express the
    // inclusive 3072 cap as the reachable exclusive boundary 3073, so the
    // boundary remains both precise and behaviorally testable.
    const evictionBoundary = PREFS_MAX_ENCODED + 1;
    let value = encodePrefsCandidate(kinds, refresh, ns);
    if (value.length < evictionBoundary) {
        return value;
    }
    // Only an oversized full payload pays for eviction candidates. `some` is a
    // bounded, lazy walk: it stops at the first fitting head prefix, while an
    // oversized non-kind payload naturally reaches the final no-kinds value.
    Array.from({ length: kinds.length }).some((_, evicted) => {
        const kept = kinds.length - evicted - 1;
        value = encodePrefsCandidate(kinds.slice(0, kept), refresh, ns);
        return value.length < evictionBoundary;
    });
    return value;
}

// --- thin document.cookie wrappers (DOM) ----------------------------------

function prefsCookieValue(): string | undefined {
    const prefix = 'ro_prefs=';
    return document.cookie
        .split('; ')
        .find((part) => part.startsWith(prefix))
        ?.slice(prefix.length);
}

// readPrefs reads the cookie and decodes it (always returns a usable prefs
// object -- a corrupt/absent cookie self-heals to empty prefs on the next
// write).
export function readPrefs(): Prefs {
    return decodePrefsValue(prefsCookieValue()).prefs;
}

export function writePrefs(prefs: Prefs): void {
    try {
        let cookie =
            'ro_prefs=' +
            encodePrefsValue(prefs) +
            '; Path=/; SameSite=Lax; Max-Age=' +
            PREFS_COOKIE_MAX_AGE;
        if (window.location.protocol === 'https:') {
            cookie += '; Secure';
        }
        document.cookie = cookie;
    } catch (_e) {
        // cookies unavailable -> the preference just will not persist
    }
}

// prefsTouchKind finds-or-creates the entry for a plural and moves it to the
// FRONT (most-recent-first -- the order tail eviction relies on).
function prefsTouchKind(prefs: Prefs, plural: string): KindPrefs {
    const index = prefs.kinds.findIndex((entry) => entry.k === plural);
    const entry = index < 0 ? { k: plural } : prefs.kinds.splice(index, 1)[0];
    prefs.kinds.unshift(entry);
    return entry;
}

// roPrefsSetSort persists a sort param ("Name", "Status:desc", ...) for a
// plural. Called from the sort-header write hook in legacy.js.
export function roPrefsSetSort(plural: string, sort: string): void {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).sort = sort;
    writePrefs(prefs);
}

// roPrefsSetHiddenColumns is the COLUMN-VISIBILITY write surface: the
// columns popover commits through it (commitColumnVisibility). names is the
// COMPLETE hidden-column list for the plural as the user sees it -- an EMPTY
// array is an explicit "hide nothing" that the server distinguishes from "no
// preference" (it suppresses the DefaultHiddenColumns config default).
export function roPrefsSetHiddenColumns(plural: string, names: string[]): void {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).hide = names;
    writePrefs(prefs);
}

// roPrefsSetRefresh persists the auto-refresh mode ('Off', seconds-as-string,
// future 'Live') -- the interval picker writes through it; Live mode
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
    setOwnNamespace(prefs.ns, cluster, namespace);
    writePrefs(prefs);
}
