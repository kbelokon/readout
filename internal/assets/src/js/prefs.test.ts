// prefs.test.ts -- Vitest pins the JS prefs codec to the SAME golden
// fixtures the Go codec uses (internal/web/testdata/prefs_golden, the SINGLE
// source -- no copies). This is the JS half of the Go<->JS seam: if the two
// codecs drift (key order, eviction victims, HTML escaping, the cap), BOTH
// test stacks (prefs_golden_test.go and this file) go red.
//
// Run: `npm test`.

import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, test } from 'vitest';

import { decodePrefsValue, encodePrefsValue, type Prefs } from './prefs.js';

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
