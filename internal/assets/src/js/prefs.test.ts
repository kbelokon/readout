// prefs.test.ts -- Vitest pins the JS prefs codec to the SAME golden
// fixtures the Go codec uses (internal/web/testdata/prefs_golden, the SINGLE
// source -- no copies). This is the JS half of the Go<->JS seam: if the two
// codecs drift (key order, eviction victims, HTML escaping, the cap), BOTH
// test stacks (prefs_golden_test.go and this file) go red.
//
// Run: `npm test`.

import { Buffer } from 'node:buffer';
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, test, vi } from 'vitest';

import { decodePrefsValue, encodePrefsValue, PREFS_MAX_ENCODED, type Prefs } from './prefs.js';

const here = dirname(fileURLToPath(import.meta.url));
// js -> src -> assets -> internal -> repo root, then into the Go testdata dir.
const goldenDir = join(here, '..', '..', '..', '..', 'internal', 'web', 'testdata', 'prefs_golden');

interface EncodeFixture {
    doc: string;
    payload: Prefs;
    encoded: string;
    evicted?: string[];
    kept?: string[];
}

interface DecodeCase {
    why: string;
    value: string;
    want_ok: boolean;
}

interface CorruptFixture {
    doc: string;
    decode_cases: DecodeCase[];
}

function loadFixture<T>(name: string): T {
    return JSON.parse(readFileSync(join(goldenDir, name), 'utf8')) as T;
}

function wireValue(payload: unknown): string {
    return `v1.${Buffer.from(JSON.stringify(payload)).toString('base64url')}`;
}

// Enumerate the golden fixtures off disk so a NEW fixture is picked up without
// touching this file -- exactly like the Go golden test globs the directory.
const fixtureFiles = readdirSync(goldenDir)
    .filter((f) => f.endsWith('.json'))
    .sort();

// Sanity: the directory the Go test reads is the one we read.
test('golden fixtures discovered', () => {
    expect(
        fixtureFiles.length,
        `expected >=7 golden fixtures, found ${fixtureFiles.length}`,
    ).toBeGreaterThanOrEqual(7);
    expect(fixtureFiles, 'corrupt-decode fixture missing').toContain('07_corrupt_decode.json');
});

test('empty preferences have the canonical minimal wire representation', () => {
    expect(encodePrefsValue({ kinds: [], refresh: '', ns: {} })).toBe('v1.e30');
});

test('base64url is unpadded, URL-safe, and round-trips both replacement characters', () => {
    const prefs: Prefs = { kinds: [], refresh: '࠾࠿', ns: {} };
    const encoded = encodePrefsValue(prefs);

    expect(encoded).toMatch(/^v1\.[A-Za-z0-9_-]+$/);
    expect(encoded).toContain('-');
    expect(encoded).toContain('_');
    expect(decodePrefsValue(encoded)).toStrictEqual({ prefs, ok: true });

    const doublePadding: Prefs = { kinds: [{ k: '¿' }], refresh: '', ns: {} };
    expect(decodePrefsValue(encodePrefsValue(doublePadding))).toStrictEqual({
        prefs: doublePadding,
        ok: true,
    });
});

test('decoder accepts canonical unpadded payloads requiring one or two padding bytes', () => {
    expect(decodePrefsValue('v1.eyJyZWZyZXNoIjoieHgifQ')).toStrictEqual({
        prefs: { kinds: [], refresh: 'xx', ns: {} },
        ok: true,
    });
    expect(decodePrefsValue('v1.eyJyZWZyZXNoIjoieHh4In0')).toStrictEqual({
        prefs: { kinds: [], refresh: 'xxx', ns: {} },
        ok: true,
    });
});

test('decoder ignores CR/LF inside RawURLEncoding input exactly like Go', () => {
    for (const value of ['v1.\r\ne30', 'v1.e\r\n30', 'v1.e\n3\r0\n']) {
        expect(decodePrefsValue(value), JSON.stringify(value)).toStrictEqual({
            prefs: { kinds: [], refresh: '', ns: {} },
            ok: true,
        });
    }
    expect(decodePrefsValue('v1.e\t30')).toStrictEqual({
        prefs: { kinds: [], refresh: '', ns: {} },
        ok: false,
    });
});

test('sparse preferences without kinds encode only their present fields', () => {
    expect(encodePrefsValue({ refresh: 'Live' })).toBe(wireValue({ refresh: 'Live' }));
    expect(decodePrefsValue(encodePrefsValue({ refresh: 'Live' }))).toStrictEqual({
        prefs: { kinds: [], refresh: 'Live', ns: {} },
        ok: true,
    });
});

test('an under-cap payload needs at most one wire encoding', () => {
    const encodeBinary = vi.spyOn(globalThis, 'btoa');
    const prefs: Prefs = {
        kinds: [
            { k: 'pods', sort: 'Name' },
            { k: 'deployments', hide: ['Ready'] },
            { k: 'services' },
        ],
        refresh: '30',
        ns: { prod: 'default' },
    };

    expect(encodePrefsValue(prefs)).toBe(wireValue(prefs));
    expect(encodeBinary.mock.calls.length).toBeLessThanOrEqual(1);
});

test('oversized eviction encodes candidates lazily and stops at the first fitting prefix', () => {
    const encodeBinary = vi.spyOn(globalThis, 'btoa');
    const prefs: Prefs = {
        kinds: [
            { k: 'pods', sort: 'Name' },
            { k: 'oversized-tail', hide: ['x'.repeat(5000)] },
        ],
        refresh: '30',
        ns: {},
    };

    const encoded = encodePrefsValue(prefs);

    expect(decodePrefsValue(encoded).prefs.kinds).toStrictEqual([{ k: 'pods', sort: 'Name' }]);
    expect(encodeBinary.mock.calls.length).toBeLessThanOrEqual(2);
});

test('eviction rejects a candidate one byte boundary above the cap before stopping', () => {
    const encodeBinary = vi.spyOn(globalThis, 'btoa');
    const boundary = structuredClone(loadFixture<EncodeFixture>('04_at_cap_boundary.json').payload);
    const hide = boundary.kinds[0]?.hide;
    expect(hide).toBeDefined();
    if (!hide) {
        throw new Error('boundary fixture has no hidden column');
    }
    hide[0] += 'x';
    const boundaryWire = wireValue({ kinds: boundary.kinds, refresh: boundary.refresh });
    expect(boundaryWire).toHaveLength(PREFS_MAX_ENCODED + 1);

    const encoded = encodePrefsValue({
        kinds: [...boundary.kinds, { k: 'oversized-tail', hide: ['y'.repeat(5000)] }],
        refresh: boundary.refresh,
        ns: boundary.ns ?? {},
    });

    expect(encoded.length).toBeLessThanOrEqual(PREFS_MAX_ENCODED);
    expect(decodePrefsValue(encoded).prefs.kinds).toStrictEqual([]);
    expect(encodeBinary.mock.calls.length).toBeLessThanOrEqual(3);
});

test('non-object top-level JSON is structural corruption', () => {
    for (const payload of [null, [], 42, 'prefs']) {
        expect(decodePrefsValue(wireValue(payload)), JSON.stringify(payload)).toStrictEqual({
            prefs: { kinds: [], refresh: '', ns: {} },
            ok: false,
        });
    }
});

test('invalid kind records and fields are dropped without losing valid siblings', () => {
    const encoded = wireValue({
        kinds: [
            null,
            42,
            [],
            { k: 7 },
            { k: 'pods', sort: 9, hide: ['Node', 3] },
            { k: 'services', sort: 'Name', hide: ['Cluster IP'] },
        ],
    });

    expect(decodePrefsValue(encoded)).toStrictEqual({
        prefs: {
            kinds: [{ k: 'pods' }, { k: 'services', sort: 'Name', hide: ['Cluster IP'] }],
            refresh: '',
            ns: {},
        },
        ok: true,
    });
});

test('namespace maps keep only string values and reject scalar or array containers', () => {
    expect(
        decodePrefsValue(wireValue({ ns: { good: 'default', numeric: 7, missing: null } })),
    ).toStrictEqual({
        prefs: { kinds: [], refresh: '', ns: { good: 'default' } },
        ok: true,
    });
    for (const ns of ['default', ['default'], 7]) {
        expect(decodePrefsValue(wireValue({ ns }))).toStrictEqual({
            prefs: { kinds: [], refresh: '', ns: {} },
            ok: true,
        });
    }
});

test('special namespace keys round-trip as own properties without changing prototypes', () => {
    const namespaces = Object.fromEntries([
        ['__proto__', 'proto-ns'],
        ['constructor', 'constructor-ns'],
        ['prototype', 'prototype-ns'],
        ['toString', 'string-ns'],
        ['hasOwnProperty', 'own-ns'],
    ]);

    const encoded = encodePrefsValue({ kinds: [], refresh: '', ns: namespaces });
    const { prefs, ok } = decodePrefsValue(encoded);

    expect(ok).toBe(true);
    expect(Object.keys(prefs.ns)).toHaveLength(Object.keys(namespaces).length);
    expect(Object.getPrototypeOf(prefs.ns)).toBe(Object.prototype);
    for (const [cluster, namespace] of Object.entries(namespaces)) {
        expect(Object.hasOwn(prefs.ns, cluster), cluster).toBe(true);
        expect(prefs.ns[cluster], cluster).toBe(namespace);
    }
    expect(Object.getOwnPropertyDescriptor(prefs.ns, '__proto__')).toStrictEqual({
        value: 'proto-ns',
        enumerable: true,
        configurable: true,
        writable: true,
    });
});

test('a single oversized kind is fully evicted without mutating the caller', () => {
    const prefs: Prefs = {
        kinds: [{ k: 'pods', sort: 'Name', hide: ['x'.repeat(5000)] }],
        refresh: '30',
        ns: {},
    };
    const before = structuredClone(prefs);
    const encoded = encodePrefsValue(prefs);

    expect(encoded.length).toBeLessThanOrEqual(PREFS_MAX_ENCODED);
    expect(decodePrefsValue(encoded)).toStrictEqual({
        prefs: { kinds: [], refresh: '30', ns: {} },
        ok: true,
    });
    expect(prefs).toStrictEqual(before);
});

test('oversized non-kind data returns the final no-kinds candidate', () => {
    const refresh = 'x'.repeat(5000);
    const encoded = encodePrefsValue({
        kinds: [{ k: 'pods', sort: 'Name' }],
        refresh,
        ns: {},
    });

    expect(encoded).toBe(wireValue({ refresh }));
    expect(encoded.length).toBeGreaterThan(PREFS_MAX_ENCODED);
    expect(decodePrefsValue(encoded)).toStrictEqual({
        prefs: { kinds: [], refresh, ns: {} },
        ok: true,
    });
});

test('the first reachable value above the cap is evicted', () => {
    const boundary = structuredClone(loadFixture<EncodeFixture>('04_at_cap_boundary.json').payload);
    const hide = boundary.kinds[0]?.hide;
    expect(hide).toBeDefined();
    if (!hide) {
        throw new Error('boundary fixture has no hidden column');
    }
    hide[0] += 'x';
    const fullWire = wireValue({ kinds: boundary.kinds, refresh: boundary.refresh });
    expect(fullWire).toHaveLength(PREFS_MAX_ENCODED + 1);

    const encoded = encodePrefsValue(boundary);
    expect(encoded.length).toBeLessThanOrEqual(PREFS_MAX_ENCODED);
    expect(decodePrefsValue(encoded).prefs.kinds).toStrictEqual([]);
});

for (const file of fixtureFiles) {
    if (file.startsWith('07_')) {
        // Decode-direction corrupt fixture: every malformed value must yield
        // empty prefs with ok=false, never a throw.
        const fx = loadFixture<CorruptFixture>(file);
        test(`${file}: corrupt values decode to empty prefs`, () => {
            for (const dc of fx.decode_cases) {
                const { prefs, ok } = decodePrefsValue(dc.value);
                // want_ok is the GO oracle (json.Unmarshal is all-or-nothing).
                // The "mistyped inner field" case is the ONE documented Go<->JS
                // divergence (prefs.go decodePrefs comment): Go rejects the
                // whole payload, but the JS reader is field-level lenient -- it
                // DROPS the mistyped field and keeps the well-typed rest, so the
                // next JS write self-heals the cookie (it never stays
                // SSR-invisible). So for that case JS yields ok=true with the
                // bad field stripped; for every structural-corruption case JS
                // matches Go: ok=false and empty prefs.
                if (dc.why === 'mistyped inner field (all-or-nothing)') {
                    expect(ok, `${dc.why}: JS self-heals (ok=true, bad field dropped)`).toBe(true);
                    expect(
                        prefs,
                        `${dc.why}: JS keeps the well-typed kind, drops the mistyped sort`,
                    ).toStrictEqual({ kinds: [{ k: 'pods' }], refresh: '', ns: {} });
                    continue;
                }
                expect(ok, `${dc.why}: ok mismatch`).toBe(dc.want_ok);
                expect(ok, `${dc.why}: structural corruption is ok=false`).toBe(false);
                expect(prefs, `${dc.why}: corrupt value must decode to empty prefs`).toStrictEqual({
                    kinds: [],
                    refresh: '',
                    ns: {},
                });
            }
        });
        continue;
    }

    // Encode-direction fixture: encodePrefsValue over the FULL payload (the
    // pre-eviction set) must reproduce `encoded` byte-for-byte. For the
    // over-cap fixtures (05/06) `encoded` is the POST-eviction value, so a
    // matching string also proves JS evicts the SAME victims as Go.
    const fx = loadFixture<EncodeFixture>(file);
    test(`${file}: encodePrefsValue reproduces the golden wire value`, () => {
        const got = encodePrefsValue(fx.payload);
        expect(got).toBe(fx.encoded);
    });

    if (fx.evicted && fx.kept) {
        // Hoist the narrowed values into consts so the nested test() closure
        // keeps the non-null narrowing (the closure cannot see the outer
        // `if (fx.evicted && fx.kept)` guard, which is why the `!` lived here).
        const evicted = fx.evicted;
        const kept = fx.kept;
        // Cross-check the eviction outcome decodes back to exactly the kept
        // kinds, in order -- the dropped tail entries are gone.
        test(`${file}: post-eviction value keeps exactly ${kept.join(',')}`, () => {
            const { prefs, ok } = decodePrefsValue(fx.encoded);
            expect(ok, 'post-eviction value must decode cleanly').toBe(true);
            expect(prefs.kinds.map((k) => k.k)).toStrictEqual(kept);
            for (const dropped of evicted) {
                expect(
                    prefs.kinds.map((kind) => kind.k),
                    `evicted kind ${dropped} must be absent`,
                ).not.toContain(dropped);
            }
        });
    }
}
