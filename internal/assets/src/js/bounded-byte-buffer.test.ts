import { describe, expect, test } from 'vitest';

import { ensureBoundedByteBufferCapacity } from './bounded-byte-buffer.js';

describe('ensureBoundedByteBufferCapacity', () => {
    test('preserves buffer identity below and at its current capacity', () => {
        const buffer = new Uint8Array(8);

        expect(ensureBoundedByteBufferCapacity(buffer, 3, 2, 16)).toBe(buffer);
        expect(ensureBoundedByteBufferCapacity(buffer, 5, 3, 16)).toBe(buffer);
    });

    test('grows geometrically and copies exactly the retained prefix', () => {
        const buffer = Uint8Array.from([1, 2, 3, 4, 5, 6, 7, 8]);

        const grown = ensureBoundedByteBufferCapacity(buffer, 5, 4, 32);

        expect(grown).not.toBe(buffer);
        expect(grown.byteLength).toBe(16);
        expect(Array.from(grown.subarray(0, 5))).toStrictEqual([1, 2, 3, 4, 5]);
        expect(Array.from(grown.subarray(5))).toStrictEqual(Array.from({ length: 11 }, () => 0));
    });

    test('uses a single oversized append as the growth step', () => {
        const buffer = Uint8Array.from([1, 2, 3, 4]);

        const grown = ensureBoundedByteBufferCapacity(buffer, 4, 6, 20);

        expect(grown.byteLength).toBe(10);
        expect(grown.byteLength).toBeGreaterThanOrEqual(4 + 6);
        expect(Array.from(grown.subarray(0, 4))).toStrictEqual([1, 2, 3, 4]);
    });

    test('clamps spare geometric capacity to the hard limit', () => {
        const hardLimit = 12;
        const buffer = Uint8Array.from({ length: 8 }, (_, index) => index + 1);

        const grown = ensureBoundedByteBufferCapacity(buffer, 8, 4, hardLimit);

        expect(grown.byteLength).toBe(12);
        expect(grown.byteLength).toBeLessThanOrEqual(hardLimit);
        expect(Array.from(grown.subarray(0, 8))).toStrictEqual(Array.from(buffer));
    });
});
