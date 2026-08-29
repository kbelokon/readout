import { describe, expect, test, vi } from 'vitest';

import {
    isClientLiveGeneration,
    liveRequestURL,
    liveScreenForBase,
    liveStreamBaseForURL,
    liveStreamBaseFromTableRequest,
    mintLiveGeneration,
    stripLiveGenerationQuery,
} from './live-url.js';

describe('Live generation', () => {
    test.each(['123e4567-e89b-12d3-a456-426614174000', '00112233445566778899aabbccddeeff'])(
        'accepts client-minted UUID/hex %s',
        (generation) => {
            expect(isClientLiveGeneration(generation)).toBe(true);
        },
    );

    test.each(['', '1,2,3,4', 'token', 'g with space', 'x'.repeat(65)])(
        'rejects non UUID/hex generation %j',
        (generation) => expect(isClientLiveGeneration(generation)).toBe(false),
    );

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
    });
});

describe('Live raw URL identity', () => {
    test('removes every decoded g key without re-encoding survivors', () => {
        expect(
            stripLiveGenerationQuery('g=one&f=status:Running,Pending&%67=two&bare&x=%ZZ&&G=kept&g'),
        ).toBe('f=status:Running,Pending&bare&x=%ZZ&&G=kept');
    });

    test('derives one legacy query generation and the matching screen', () => {
        const base = liveStreamBaseForURL(
            new URL('https://readout.test/clusters/prod/pods///?g=old&f=a,b&%67=older'),
        );
        expect(base).toBe('/clusters/prod/pods/_stream?f=a,b');
        expect(liveScreenForBase(base)).toBe('/clusters/prod/pods?f=a,b');
        expect(liveRequestURL(base, '00112233445566778899aabbccddeeff')).toBe(
            '/clusters/prod/pods/_stream?f=a,b&g=00112233445566778899aabbccddeeff',
        );
    });

    test('converts only a final table route and cleans its generation keys', () => {
        expect(liveStreamBaseFromTableRequest('/clusters/prod/pods/_table?sort=Name&%67=x')).toBe(
            '/clusters/prod/pods/_stream?sort=Name',
        );
        expect(liveStreamBaseFromTableRequest('/pods?next=/_table')).toBeNull();
        expect(liveStreamBaseFromTableRequest(7)).toBeNull();
    });
});
