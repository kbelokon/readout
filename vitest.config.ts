import { defineConfig } from 'vitest/config';

export default defineConfig({
    test: {
        // Node 26's process-global experimental Web Storage shadows jsdom's
        // per-window implementation unless it is disabled in Vitest workers.
        execArgv: ['--no-experimental-webstorage'],
        include: ['internal/assets/src/js/**/*.test.ts'],
        environment: 'node',
        globals: false,
        isolate: true,
        environmentOptions: {
            jsdom: {
                pretendToBeVisual: true,
                url: 'https://readout.test/',
            },
        },
        setupFiles: ['./internal/assets/src/js/test/setup.ts'],
        clearMocks: true,
        allowOnly: false,
        restoreMocks: true,
        unstubEnvs: true,
        unstubGlobals: true,
        passWithNoTests: false,
        coverage: {
            provider: 'v8',
            reportsDirectory: 'coverage',
            reporter: ['text', 'json-summary', 'lcov'],
            include: ['internal/assets/src/js/**/*.ts'],
            exclude: [
                'internal/assets/src/js/**/*.test.ts',
                'internal/assets/src/js/test/**',
                'internal/assets/src/js/types.ts',
            ],
            // Initial ratchet: keep this close to the measured baseline and only
            // move it upward as uncovered behavior is brought under test.
            thresholds: {
                statements: 93,
                branches: 83,
                functions: 96,
                lines: 93,
            },
        },
    },
});
