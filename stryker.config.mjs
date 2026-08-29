if (process.env.READOUT_MUTATION_GUARD !== '1') {
    throw new Error('Run Stryker through the guarded npm mutation scripts');
}
const mutationMode = process.env.READOUT_MUTATION_MODE;
if (mutationMode !== 'dry' && mutationMode !== 'full') {
    throw new Error('The guarded launcher must select dry or full mutation mode');
}
const fullRun = mutationMode === 'full';

/** @type {import('@stryker-mutator/api/core').PartialStrykerOptions} */
export default {
    $schema: './node_modules/@stryker-mutator/core/schema/stryker-schema.json',
    mutate: [
        'internal/assets/src/js/**/*.ts',
        '!internal/assets/src/js/**/*.test.ts',
        '!internal/assets/src/js/test/**',
        '!internal/assets/src/js/types.ts',
    ],
    testRunner: 'vitest',
    vitest: {
        configFile: 'vitest.config.ts',
        related: true,
    },
    checkers: ['typescript'],
    coverageAnalysis: 'perTest',
    tsconfigFile: 'tsconfig.json',
    typescriptChecker: {
        experimentalNativePreview: true,
    },
    checkerNodeArgs: [
        '--import',
        new URL('./scripts/stryker-typescript-hook.mjs', import.meta.url).href,
    ],
    concurrency: 4,
    inPlace: false,
    tempDirName: '.stryker-tmp',
    cleanTempDir: 'always',
    incremental: false,
    ignoreStatic: false,
    logLevel: 'warn',
    fileLogLevel: 'off',
    reporters: fullRun ? ['clear-text', 'json', 'html'] : ['clear-text'],
    jsonReporter: {
        fileName: 'reports/mutation/mutation.json',
    },
    htmlReporter: {
        fileName: 'reports/mutation/index.html',
    },
    thresholds: {
        high: 100,
        low: 100,
        // The attested post-check is the quality gate. Keeping Stryker's
        // own exit threshold at zero lets it finish and publish a complete
        // report when unresolved mutants still need to be investigated.
        break: 0,
    },
    mutator: {
        excludedMutations: [],
    },
};
