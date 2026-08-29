// @vitest-environment jsdom

import { describe, expect, test, vi } from 'vitest';

import {
    isClientLiveGeneration,
    liveStreamBaseForURL,
    liveStreamBaseFromTableRequest,
    mintLiveGeneration,
} from './live-url.js';

describe('Live generation', () => {
    test.each([
        '123e4567-e89b-12d3-a456-426614174000',
        '00112233445566778899aabbccddeeff',
        'ABCDEF09-ABCD-EF09-ABCD-EF0901234567',
        'FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF',
    ])('accepts client-minted UUID/hex %s', (generation) => {
        expect(isClientLiveGeneration(generation)).toBe(true);
    });

    test.each([
        '',
        '1,2,3,4',
        'token',
        'g with space',
        '0'.repeat(31),
        '0'.repeat(33),
        '00000000-0000-0000-0000-00000000000',
        '00000000-0000-0000-0000-0000000000000',
        'x123e4567-e89b-12d3-a456-426614174000',
        '123e4567-e89b-12d3-a456-426614174000x',
        '00000000_0000-0000-0000-000000000000',
        '00000000-0000-0000-0000-00000000000g',
        '０'.repeat(32),
        42,
        null,
    ])('rejects non UUID/hex generation %j', (generation) =>
        expect(isClientLiveGeneration(generation)).toBe(false),
    );

    test.each([0, 9, 14, 19, 24])(
        'rejects a non-hex byte in UUID group beginning at %i',
        (groupStart) => {
            const generation = [...'00000000-0000-0000-0000-000000000000'];
            generation[groupStart] = 'g';
            expect(isClientLiveGeneration(generation.join(''))).toBe(false);
        },
    );

    test.each([8, 13, 18, 23])('rejects a non-dash UUID separator at %i', (separator) => {
        const generation = [...'00000000-0000-0000-0000-000000000000'];
        generation[separator] = '0';
        expect(isClientLiveGeneration(generation.join(''))).toBe(false);
    });

    test('prefers randomUUID and falls back to 16 random bytes', () => {
        const uuid = '123e4567-e89b-12d3-a456-426614174000';
        const source = {
            randomUUID: vi.fn(() => uuid),
            getRandomValues: vi.fn(),
        } as unknown as Crypto;
        expect(mintLiveGeneration(source)).toBe(uuid);
        expect(source.getRandomValues).not.toHaveBeenCalled();

        const fallback = {
            randomUUID: vi.fn(() => {
                throw new Error('unsupported');
            }),
            getRandomValues: vi.fn((bytes: Uint8Array) => {
                bytes.forEach((_value, index) => {
                    bytes[index] = index;
                });
                return bytes;
            }),
        } as unknown as Crypto;
        expect(mintLiveGeneration(fallback)).toBe('000102030405060708090a0b0c0d0e0f');

        const invalidUUID = {
            randomUUID: vi.fn(() => 'not-a-uuid'),
            getRandomValues: fallback.getRandomValues,
        } as unknown as Crypto;
        expect(mintLiveGeneration(invalidUUID)).toBe('000102030405060708090a0b0c0d0e0f');
    });
});

describe('Live raw URL identity', () => {
    test('derives the stream path without adding or removing query generation fields', () => {
        const base = liveStreamBaseForURL(
            new URL('https://readout.test/clusters/prod/pods///?g=old&f=a,b&%67=older'),
        );
        expect(base).toBe('/clusters/prod/pods/_stream?g=old&f=a,b&%67=older');
        expect(liveStreamBaseForURL(new URL('https://readout.test/pods'))).toBe('/pods/_stream');
    });

    test('requires a real same-origin _table path', () => {
        expect(liveStreamBaseFromTableRequest('/pods/_table')).toBe('/pods/_stream');
        expect(liveStreamBaseFromTableRequest('?/pods/_table')).toBeNull();
    });

    test('normalizes same-origin table requests to a path-only stream identity', () => {
        window.history.replaceState(null, '', '/');
        expect(
            liveStreamBaseFromTableRequest(
                `${window.location.origin}/clusters/prod/pods/_table?sort=Name&%67=x#ignored`,
            ),
        ).toBe('/clusters/prod/pods/_stream?sort=Name&%67=x');
        expect(liveStreamBaseFromTableRequest('./pods/_table?x=%ZZ&g=old#ignored')).toBe(
            '/pods/_stream?x=%ZZ&g=old',
        );
    });

    test('rejects foreign, non-table, malformed, and non-string request URLs', () => {
        expect(liveStreamBaseFromTableRequest('https://foreign.invalid/pods/_table')).toBeNull();
        expect(liveStreamBaseFromTableRequest('/pods?next=/_table')).toBeNull();
        expect(liveStreamBaseFromTableRequest('?_table')).toBeNull();
        expect(liveStreamBaseFromTableRequest('http://[invalid')).toBeNull();
        expect(liveStreamBaseFromTableRequest(7)).toBeNull();
    });
});
