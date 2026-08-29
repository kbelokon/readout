import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { once } from 'node:events';
import {
  chmodSync,
  closeSync,
  existsSync,
  linkSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { delimiter, dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { buildSync } from 'esbuild';
import { checkMutationReport, isMutationReportCheckerMain } from './check-mutation-report.mjs';
import { frontendJavaScriptBuildOptions } from './frontend-build-config.mjs';
import { parseMutationCliArguments } from './mutation-cli.mjs';
import { resolveSafeHostExecutable } from './mutation-host.mjs';
import {
  assertImportOnlyModuleSource,
  assertMutationRuntimeEligible,
  assertMutationScopeMatchesRuntimeGraph,
  assertTypeOnlyModuleSource,
  createFullRunAttestation,
  createMutationInputStage,
  fullRunAttestationRelativePath,
  invalidateFullMutationReport,
  mutationHtmlReportRelativePath,
  mutationLockRelativePath,
  mutationLogRelativePath,
  mutationRecoveryRelativePath,
  mutationReportRelativePath,
  mutationStagePath,
  openMutationLogFile,
  publishFullMutationReports,
  removeMutationInputStage,
  snapshotMutationInputs,
  verifyFullRunAttestation,
  writeFullRunAttestation,
} from './mutation-integrity.mjs';
import { acquireMutationLock } from './mutation-lock.mjs';

const fixedFixtureInputs = [
  '.mise.toml',
  'internal/assets/static/readout.js',
  'package-lock.json',
  'package.json',
  'scripts/build-assets.mjs',
  'scripts/check-mutation-report.mjs',
  'scripts/frontend-build-config.mjs',
  'scripts/mutation-child.mjs',
  'scripts/mutation-cli.mjs',
  'scripts/mutation-infrastructure.test.mjs',
  'scripts/mutation-host.mjs',
  'scripts/mutation-integrity.mjs',
  'scripts/mutation-lock.mjs',
  'scripts/run-mutation.mjs',
  'scripts/stryker-typescript-hook.mjs',
  'stryker.config.mjs',
  'tsconfig.json',
  'tsconfig.test.json',
  'vitest.config.ts',
];

function writeFixtureFile(repoRoot, fileName, contents = `${fileName}\n`) {
  const path = join(repoRoot, fileName);
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, contents);
}

function makeFixture(t) {
  const repoRoot = mkdtempSync(join(tmpdir(), 'readout-mutation-guard-'));
  t.after(() => rmSync(repoRoot, { force: true, recursive: true }));
  for (const fileName of fixedFixtureInputs) writeFixtureFile(repoRoot, fileName);
  const nodeVersion = process.versions.node;
  const nextNodeMajor = Number(nodeVersion.split('.')[0]) + 1;
  writeFixtureFile(repoRoot, '.mise.toml', `[tools]\nnode = "${nodeVersion}"\n`);
  writeFixtureFile(
    repoRoot,
    'package.json',
    `${JSON.stringify({ engines: { node: `>=${nodeVersion} <${nextNodeMajor}` } })}\n`,
  );
  writeFixtureFile(repoRoot, 'tsconfig.json', '{}\n');
  writeFixtureFile(repoRoot, 'tsconfig.test.json', '{"extends":"./tsconfig.json"}\n');
  writeFixtureFile(repoRoot, 'internal/assets/src/js/runtime.ts', 'export const answer = 42;\n');
  writeFixtureFile(
    repoRoot,
    'internal/assets/src/js/types.ts',
    'export interface Answer { n: number }\n',
  );
  writeFixtureFile(repoRoot, 'internal/assets/src/js/readout.ts', "import './runtime.js';\n");
  const bundle = buildSync(frontendJavaScriptBuildOptions(repoRoot)).outputFiles[0].text;
  writeFixtureFile(repoRoot, 'internal/assets/static/readout.js', bundle);
  writeFixtureFile(repoRoot, 'internal/assets/src/js/runtime.test.ts', 'void 0;\n');
  writeFixtureFile(repoRoot, 'internal/assets/src/js/test/setup.ts', 'void 0;\n');
  mkdirSync(join(repoRoot, 'node_modules'));
  writeFixtureFile(
    repoRoot,
    'internal/web/testdata/prefs_golden/01_simple.json',
    '{"payload":{}}\n',
  );
  writeFixtureFile(
    repoRoot,
    'internal/web/testdata/live_render_contract.json',
    '{"version":1,"rows":[],"regions":[]}\n',
  );
  return repoRoot;
}

function attestFixture(repoRoot) {
  const startedAt = new Date(Date.now() - 2_000).toISOString();
  const reportBytes = Buffer.from('{"fresh":true}\n');
  const htmlReportBytes = Buffer.from('<!doctype html><title>fresh mutation report</title>\n');
  writeFixtureFile(repoRoot, mutationReportRelativePath, reportBytes);
  writeFixtureFile(repoRoot, mutationHtmlReportRelativePath, htmlReportBytes);
  const completedAt = new Date().toISOString();
  const inputs = snapshotMutationInputs(repoRoot);
  writeFullRunAttestation(
    repoRoot,
    createFullRunAttestation({
      completedAt,
      executionInputs: inputs,
      htmlReportBytes,
      inputs,
      repoRoot,
      reportBytes,
      runId: '00000000-0000-4000-8000-000000000000',
      startedAt,
    }),
  );
}

function attestCheckerFixture(repoRoot, outcomes) {
  const source = readFileSync(join(repoRoot, 'internal/assets/src/js/runtime.ts'), 'utf8');
  const mutants = outcomes.map((outcome, index) => ({
    ...(typeof outcome === 'string' ? { status: outcome } : outcome),
    id: `mutation-${index + 1}`,
    location: {
      end: { column: 32, line: 1 },
      start: { column: 23, line: 1 },
    },
    mutatorName: 'ArithmeticOperator',
  }));
  const report = {
    schemaVersion: '2',
    thresholds: { break: 0, high: 100, low: 100 },
    config: {
      allowEmpty: false,
      checkerNodeArgs: [
        '--import',
        pathToFileURL(join(repoRoot, '.mutation-stage/scripts/stryker-typescript-hook.mjs')).href,
      ],
      checkers: ['typescript'],
      cleanTempDir: 'always',
      coverageAnalysis: 'perTest',
      disableBail: false,
      dryRunOnly: false,
      force: false,
      ignorePatterns: [],
      ignoreStatic: false,
      ignorers: [],
      inPlace: false,
      incremental: false,
      mutate: [
        'internal/assets/src/js/**/*.ts',
        '!internal/assets/src/js/**/*.test.ts',
        '!internal/assets/src/js/test/**',
        '!internal/assets/src/js/types.ts',
      ],
      mutator: { excludedMutations: [] },
      reporters: ['clear-text', 'json', 'html'],
      symlinkNodeModules: true,
      tempDirName: '.stryker-tmp',
      testFiles: [],
      testRunner: 'vitest',
      tsconfigFile: 'tsconfig.json',
      typescriptChecker: { experimentalNativePreview: true },
      vitest: { configFile: 'vitest.config.ts', related: true },
    },
    files: {
      'internal/assets/src/js/runtime.ts': {
        language: 'typescript',
        mutants,
        source,
      },
    },
  };
  const reportBytes = Buffer.from(`${JSON.stringify(report)}\n`);
  const htmlReportBytes = Buffer.from('<!doctype html><title>checker fixture</title>\n');
  const startedAt = new Date(Date.now() - 2_000).toISOString();
  writeFixtureFile(repoRoot, mutationReportRelativePath, reportBytes);
  writeFixtureFile(repoRoot, mutationHtmlReportRelativePath, htmlReportBytes);
  const completedAt = new Date().toISOString();
  const inputs = snapshotMutationInputs(repoRoot);
  writeFullRunAttestation(
    repoRoot,
    createFullRunAttestation({
      completedAt,
      executionInputs: inputs,
      htmlReportBytes,
      inputs,
      repoRoot,
      reportBytes,
      runId: '00000000-0000-4000-8000-000000000001',
      startedAt,
    }),
  );
}

async function waitUntil(predicate, description, timeoutMilliseconds = 5_000) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error(`timed out waiting for ${description}`);
    await new Promise((resolveWait) => setTimeout(resolveWait, 20));
  }
}

function childResult(child, timeoutMilliseconds = 8_000) {
  return new Promise((resolveResult, rejectResult) => {
    const timeout = setTimeout(() => {
      rejectResult(new Error(`child ${child.pid} did not exit within ${timeoutMilliseconds}ms`));
    }, timeoutMilliseconds);
    child.once('error', (error) => {
      clearTimeout(timeout);
      rejectResult(error);
    });
    child.once('close', (code, signal) => {
      clearTimeout(timeout);
      resolveResult({ code, signal });
    });
  });
}

function childExitResult(child, timeoutMilliseconds = 8_000) {
  return new Promise((resolveResult, rejectResult) => {
    const timeout = setTimeout(() => {
      rejectResult(new Error(`child ${child.pid} did not exit within ${timeoutMilliseconds}ms`));
    }, timeoutMilliseconds);
    child.once('error', (error) => {
      clearTimeout(timeout);
      rejectResult(error);
    });
    child.once('exit', (code, signal) => {
      clearTimeout(timeout);
      resolveResult({ code, signal });
    });
  });
}

function processGroupExists(processGroupId) {
  try {
    process.kill(-processGroupId, 0);
    return true;
  } catch (error) {
    if (error.code === 'ESRCH') return false;
    throw error;
  }
}

test('launcher accepts only its mode-aware CLI', () => {
  assert.deepEqual(parseMutationCliArguments(['--mode=full']), {
    concurrency: 4,
    mode: 'full',
    strykerArgs: ['--concurrency', '4'],
  });
  assert.deepEqual(parseMutationCliArguments(['--mode=full', '-c1']), {
    concurrency: 1,
    mode: 'full',
    strykerArgs: ['--concurrency', '1'],
  });
  assert.deepEqual(parseMutationCliArguments(['--mode', 'dry']), {
    concurrency: 1,
    mode: 'dry',
    strykerArgs: ['--dryRunOnly', '--concurrency', '1'],
  });

  for (const args of [
    ['--mode=full', '-c5'],
    ['--mode=full', '-c99'],
    ['--mode=full', 'positional'],
    ['--mode=full', '--unknown'],
    ['--mode=full', '--reporters=json'],
    ['--mode=full', '--dryRunOnly'],
    ['--mode=full', '--mode=dry'],
  ]) {
    assert.throws(() => parseMutationCliArguments(args), Error, args.join(' '));
  }
});

test('canonical full mutation policy uses four workers and non-breaking Stryker reporting', () => {
  const packageJson = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'));
  assert.equal(
    packageJson.scripts['test:mutation:run'],
    'node scripts/run-mutation.mjs --mode=full --concurrency=4',
  );

  const configUrl = new URL('../stryker.config.mjs', import.meta.url).href;
  const probe = spawnSync(
    process.execPath,
    [
      '--input-type=module',
      '--eval',
      `process.env.READOUT_MUTATION_GUARD = '1';
process.env.READOUT_MUTATION_MODE = 'full';
const { default: config } = await import(${JSON.stringify(configUrl)});
process.stdout.write(JSON.stringify({ concurrency: config.concurrency, thresholds: config.thresholds }));`,
    ],
    { encoding: 'utf8' },
  );
  assert.equal(probe.status, 0, probe.stderr);
  assert.deepEqual(JSON.parse(probe.stdout), {
    concurrency: 4,
    thresholds: { break: 0, high: 100, low: 100 },
  });
});

test('canonical launcher has no campaign wall-clock deadline', () => {
  const launcher = readFileSync(new URL('./run-mutation.mjs', import.meta.url), 'utf8');
  assert.doesNotMatch(
    launcher,
    /MUTATION_MAX_MINUTES|maximumRunMilliseconds|wallClockTimer|runtime reached/u,
  );
  assert.match(launcher, /deadline=none/u);
});

test('mutation checker recognizes a symlinked main-module path', (t) => {
  const checkerPath = fileURLToPath(new URL('./check-mutation-report.mjs', import.meta.url));
  const tempRoot = mkdtempSync(join(tmpdir(), 'readout-mutation-checker-main-'));
  const checkerLink = join(tempRoot, 'checker.mjs');
  t.after(() => rmSync(tempRoot, { force: true, recursive: true }));
  symlinkSync(checkerPath, checkerLink);

  assert.equal(isMutationReportCheckerMain(checkerPath), true);
  assert.equal(isMutationReportCheckerMain(checkerLink), true);
  assert.equal(isMutationReportCheckerMain(join(tempRoot, 'missing.mjs')), false);
});

test('attested checker rejects unresolved outcomes without deleting evidence', (t) => {
  const assertEvidenceRemains = (fixtureRoot) => {
    assert.doesNotThrow(() => verifyFullRunAttestation(fixtureRoot));
    for (const path of [
      mutationReportRelativePath,
      mutationHtmlReportRelativePath,
      fullRunAttestationRelativePath,
    ]) {
      assert.equal(existsSync(join(fixtureRoot, path)), true, path);
    }
  };

  const survivorRoot = makeFixture(t);
  attestCheckerFixture(survivorRoot, ['Killed', 'Survived']);
  assert.throws(
    () => checkMutationReport(survivorRoot, () => {}),
    /unresolved mutation statuses: Survived=1/u,
  );
  assertEvidenceRemains(survivorRoot);

  const runtimeErrorRoot = makeFixture(t);
  attestCheckerFixture(runtimeErrorRoot, ['Killed', 'RuntimeError']);
  assert.throws(
    () => checkMutationReport(runtimeErrorRoot, () => {}),
    /unresolved mutation statuses: RuntimeError=1/u,
  );
  assertEvidenceRemains(runtimeErrorRoot);

  const passingRoot = makeFixture(t);
  attestCheckerFixture(passingRoot, ['Killed', 'CompileError']);
  const output = [];
  assert.doesNotThrow(() => checkMutationReport(passingRoot, (line) => output.push(line)));
  assert.ok(
    output.includes(
      'Directly killed: 1/2 all generated mutants ' + '(50.00%; CompileError reported separately)',
    ),
    output.join('\n'),
  );
  assert.ok(output.includes('  CompileError 1'), output.join('\n'));
  assert.doesNotThrow(() => verifyFullRunAttestation(passingRoot));
});

test('attested checker rejects every Timeout outcome', (t) => {
  const repoRoot = makeFixture(t);
  attestCheckerFixture(repoRoot, [
    'Killed',
    { status: 'Timeout', statusReason: 'Hit limit reached (11201/11200)' },
  ]);

  assert.throws(
    () => checkMutationReport(repoRoot, () => {}),
    /unresolved mutation statuses: Timeout=1/u,
  );
});

test('TypeScript AST guards type-only and side-effect-import-only modules', () => {
  assert.doesNotThrow(() =>
    assertTypeOnlyModuleSource(
      'types.ts',
      "import type { A } from './a.js';\nexport interface B { a: A }\ndeclare global { interface Window { b: B } }\n",
    ),
  );
  assert.throws(() => assertTypeOnlyModuleSource('types.ts', 'export const runtime = 1;\n'));
  assert.doesNotThrow(() =>
    assertImportOnlyModuleSource('readout.ts', "import './a.js';\nimport './b.js';\n"),
  );
  assert.throws(() =>
    assertImportOnlyModuleSource('readout.ts', "import { value } from './a.js';\n"),
  );
  assert.throws(() => assertImportOnlyModuleSource('readout.ts', "import './a.js';\nrun();\n"));
});

test('mutation stage is an exact input snapshot isolated from working-tree ABA', (t) => {
  const repoRoot = makeFixture(t);
  const testPath = 'internal/assets/src/js/runtime.test.ts';
  const originalTest = readFileSync(join(repoRoot, testPath));
  const expectedInputs = snapshotMutationInputs(repoRoot);
  const { inputs: stagedInputs, stageRoot } = createMutationInputStage(repoRoot, expectedInputs);

  assert.deepEqual(stagedInputs, expectedInputs);
  assert.equal(lstatSync(join(stageRoot, 'node_modules')).isSymbolicLink(), true);
  assert.equal(
    realpathSync(join(stageRoot, 'node_modules')),
    realpathSync(join(repoRoot, 'node_modules')),
  );

  // The old endpoint-only proof missed this transient working-tree change.
  // The staged test bytes used by Stryker remain the captured bytes throughout.
  writeFixtureFile(repoRoot, testPath, 'throw new Error("transient");\n');
  writeFixtureFile(repoRoot, testPath, originalTest);
  assert.deepEqual(snapshotMutationInputs(repoRoot), expectedInputs);
  assert.deepEqual(readFileSync(join(stageRoot, testPath)), originalTest);
  assert.deepEqual(
    readFileSync(join(stageRoot, 'internal/web/testdata/live_render_contract.json')),
    readFileSync(join(repoRoot, 'internal/web/testdata/live_render_contract.json')),
  );
  assert.deepEqual(snapshotMutationInputs(stageRoot), expectedInputs);

  removeMutationInputStage(repoRoot);
  assert.equal(existsSync(stageRoot), false);
});

test('mutation staging rejects a changed source and removes its partial stage', (t) => {
  const repoRoot = makeFixture(t);
  const expectedInputs = snapshotMutationInputs(repoRoot);
  writeFixtureFile(repoRoot, 'vitest.config.ts', 'export default { changed: true };\n');

  assert.throws(
    () => createMutationInputStage(repoRoot, expectedInputs),
    /mutation input changed while staging: vitest\.config\.ts/u,
  );
  assert.equal(existsSync(mutationStagePath(repoRoot)), false);
});

test('stage cleanup unlinks an exact-path symlink without traversing its target', (t) => {
  const repoRoot = makeFixture(t);
  const externalRoot = mkdtempSync(join(tmpdir(), 'readout-stage-target-'));
  t.after(() => rmSync(externalRoot, { force: true, recursive: true }));
  const sentinelPath = join(externalRoot, 'sentinel');
  writeFileSync(sentinelPath, 'keep\n');
  symlinkSync(externalRoot, mutationStagePath(repoRoot), 'dir');

  removeMutationInputStage(repoRoot);
  assert.equal(existsSync(mutationStagePath(repoRoot)), false);
  assert.equal(readFileSync(sentinelPath, 'utf8'), 'keep\n');
});

test('runtime graph guard rejects runtime code hidden under an excluded path', (t) => {
  const repoRoot = makeFixture(t);
  assert.doesNotThrow(() => assertMutationScopeMatchesRuntimeGraph(repoRoot));

  writeFixtureFile(
    repoRoot,
    'internal/assets/src/js/test/hidden.ts',
    'export const hiddenRuntime = true;\n',
  );
  writeFixtureFile(
    repoRoot,
    'internal/assets/src/js/readout.ts',
    "import './runtime.js';\nimport './test/hidden.js';\n",
  );
  assert.throws(
    () => assertMutationScopeMatchesRuntimeGraph(repoRoot),
    /excluded-from-mutation=\[internal\/assets\/src\/js\/test\/hidden\.ts\]/u,
  );

  writeFixtureFile(repoRoot, 'internal/assets/src/outside.ts', 'export const outside = true;\n');
  writeFixtureFile(
    repoRoot,
    'internal/assets/src/js/readout.ts',
    "import './runtime.js';\nimport '../outside.js';\n",
  );
  assert.throws(
    () => assertMutationScopeMatchesRuntimeGraph(repoRoot),
    /project inputs outside the mutation root: \[internal\/assets\/src\/outside\.ts\]/u,
  );

  writeFixtureFile(repoRoot, 'internal/assets/src/outside.js', 'export const outside = true;\n');
  writeFixtureFile(
    repoRoot,
    'internal/assets/src/js/readout.ts',
    "import './runtime.js';\nimport '../outside.js';\n",
  );
  assert.throws(
    () => assertMutationScopeMatchesRuntimeGraph(repoRoot),
    /project inputs outside the mutation root: \[internal\/assets\/src\/outside\.js\]/u,
  );

  writeFixtureFile(repoRoot, 'internal/assets/src/outside.json', '{"outside":true}\n');
  writeFixtureFile(
    repoRoot,
    'internal/assets/src/js/readout.ts',
    "import outside from '../outside.json';\nvoid outside;\n",
  );
  assert.throws(
    () => assertMutationScopeMatchesRuntimeGraph(repoRoot),
    /project inputs outside the mutation root: \[internal\/assets\/src\/outside\.json\]/u,
  );

  writeFixtureFile(repoRoot, 'internal/assets/src/js/readout.ts', "import './runtime.js';\n");
  writeFixtureFile(repoRoot, 'internal/assets/static/readout.js', 'stale bundle\n');
  assert.throws(
    () => assertMutationScopeMatchesRuntimeGraph(repoRoot),
    /shipped frontend bundle is stale/u,
  );
});

test('shared frontend build recipe is independent of the caller working directory', (t) => {
  const repoRoot = makeFixture(t);
  const callerRoot = mkdtempSync(join(tmpdir(), 'readout-build-caller-'));
  t.after(() => rmSync(callerRoot, { force: true, recursive: true }));
  const moduleUrl = new URL('./frontend-build-config.mjs', import.meta.url).href;
  const esbuildUrl = new URL('../node_modules/esbuild/lib/main.js', import.meta.url).href;
  const script = `
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
const { buildSync } = await import(${JSON.stringify(esbuildUrl)});
const { frontendJavaScriptBuildOptions } = await import(${JSON.stringify(moduleUrl)});
const repoRoot = ${JSON.stringify(repoRoot)};
const built = buildSync(frontendJavaScriptBuildOptions(repoRoot)).outputFiles[0].text;
const shipped = readFileSync(join(repoRoot, 'internal/assets/static/readout.js'), 'utf8');
if (built !== shipped) throw new Error('shared frontend build changed with caller cwd');
`;
  const result = spawnSync(process.execPath, ['--input-type=module', '--eval', script], {
    cwd: callerRoot,
    encoding: 'utf8',
  });
  assert.equal(result.status, 0, result.stderr);
});

test('mutation artifacts never traverse a symlinked reports directory', (t) => {
  const repoRoot = mkdtempSync(join(tmpdir(), 'readout-report-root-'));
  const externalRoot = mkdtempSync(join(tmpdir(), 'readout-report-external-'));
  t.after(() => rmSync(repoRoot, { force: true, recursive: true }));
  t.after(() => rmSync(externalRoot, { force: true, recursive: true }));
  mkdirSync(join(repoRoot, 'reports'));
  for (const fileName of ['full-run.json', 'mutation.json', 'index.html']) {
    writeFileSync(join(externalRoot, fileName), `keep ${fileName}\n`);
  }
  symlinkSync(externalRoot, join(repoRoot, 'reports', 'mutation'), 'dir');

  assert.throws(
    () => invalidateFullMutationReport(repoRoot),
    /mutation artifact directory must be a real directory/u,
  );
  for (const fileName of ['full-run.json', 'mutation.json', 'index.html']) {
    assert.equal(readFileSync(join(externalRoot, fileName), 'utf8'), `keep ${fileName}\n`);
  }
});

test('latest mutation log replaces links without truncating their targets', (t) => {
  const repoRoot = makeFixture(t);
  const externalRoot = mkdtempSync(join(tmpdir(), 'readout-log-target-'));
  t.after(() => rmSync(externalRoot, { force: true, recursive: true }));
  mkdirSync(join(repoRoot, 'reports', 'mutation'), { recursive: true });
  const sentinelPath = join(externalRoot, 'sentinel.log');
  const logPath = join(repoRoot, mutationLogRelativePath);
  writeFileSync(sentinelPath, 'keep external bytes\n');

  symlinkSync(sentinelPath, logPath);
  let opened = openMutationLogFile(repoRoot);
  closeSync(opened.descriptor);
  assert.equal(opened.logPath, logPath);
  assert.equal(readFileSync(sentinelPath, 'utf8'), 'keep external bytes\n');
  assert.equal(lstatSync(logPath).isSymbolicLink(), false);

  rmSync(logPath);
  linkSync(sentinelPath, logPath);
  opened = openMutationLogFile(repoRoot);
  closeSync(opened.descriptor);
  assert.equal(readFileSync(sentinelPath, 'utf8'), 'keep external bytes\n');
  assert.equal(readFileSync(logPath, 'utf8'), '');
});

test('full report publication is explicit, atomic per file, and refuses overwrite', (t) => {
  const repoRoot = makeFixture(t);
  const reportBytes = Buffer.from('{"stage":true}\n');
  const htmlReportBytes = Buffer.from('<!doctype html><title>stage</title>\n');

  publishFullMutationReports(repoRoot, { htmlReportBytes, reportBytes });
  assert.deepEqual(readFileSync(join(repoRoot, mutationReportRelativePath)), reportBytes);
  assert.deepEqual(readFileSync(join(repoRoot, mutationHtmlReportRelativePath)), htmlReportBytes);
  assert.equal(existsSync(join(repoRoot, fullRunAttestationRelativePath)), false);
  assert.throws(
    () => publishFullMutationReports(repoRoot, { htmlReportBytes, reportBytes }),
    /refusing to overwrite an existing full-run artifact/u,
  );
  assert.deepEqual(readFileSync(join(repoRoot, mutationReportRelativePath)), reportBytes);
});

test('checker provenance rejects a stale unattested report', (t) => {
  const repoRoot = makeFixture(t);
  writeFixtureFile(repoRoot, mutationReportRelativePath, '{"stale":true}\n');
  assert.throws(
    () => verifyFullRunAttestation(repoRoot),
    /cannot read a valid full-run attestation/u,
  );
});

test('a full attempt invalidates every prior candidate report', (t) => {
  const repoRoot = makeFixture(t);
  for (const fileName of [
    mutationReportRelativePath,
    mutationHtmlReportRelativePath,
    fullRunAttestationRelativePath,
  ]) {
    writeFixtureFile(repoRoot, fileName, 'stale\n');
  }
  invalidateFullMutationReport(repoRoot);
  for (const fileName of [
    mutationReportRelativePath,
    mutationHtmlReportRelativePath,
    fullRunAttestationRelativePath,
  ]) {
    assert.equal(existsSync(join(repoRoot, fileName)), false, fileName);
  }
});

test('full-attempt invalidation removes proof before a malformed report path can fail', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  const reportPath = join(repoRoot, mutationReportRelativePath);
  rmSync(reportPath);
  mkdirSync(reportPath);
  writeFileSync(join(reportPath, 'cannot-unlink-as-a-file'), 'blocked\n');

  assert.throws(() => invalidateFullMutationReport(repoRoot));
  assert.equal(existsSync(join(repoRoot, fullRunAttestationRelativePath)), false);
});

test('checker provenance rejects a report changed after a successful full run', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  writeFixtureFile(repoRoot, mutationReportRelativePath, '{"replaced":true}\n');
  assert.throws(() => verifyFullRunAttestation(repoRoot), /report digest does not match/u);
});

test('checker provenance requires the attested HTML report', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  rmSync(join(repoRoot, mutationHtmlReportRelativePath));
  assert.throws(() => verifyFullRunAttestation(repoRoot), /ENOENT/u);
});

test('checker provenance binds the staged execution digest and requires stage cleanup', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  const attestationPath = join(repoRoot, fullRunAttestationRelativePath);
  const attestation = JSON.parse(readFileSync(attestationPath, 'utf8'));
  attestation.execution.inputsSha256 = '0'.repeat(64);
  writeFileSync(attestationPath, `${JSON.stringify(attestation)}\n`);
  assert.throws(
    () => verifyFullRunAttestation(repoRoot),
    /execution inputs do not match the attested working-tree inputs/u,
  );

  attestFixture(repoRoot);
  mkdirSync(mutationStagePath(repoRoot));
  assert.throws(() => verifyFullRunAttestation(repoRoot), /mutation stage still exists/u);
});

test('checker provenance rejects changed or stale HTML reports', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  writeFixtureFile(repoRoot, mutationHtmlReportRelativePath, '<html>replaced</html>\n');
  assert.throws(() => verifyFullRunAttestation(repoRoot), /HTML report digest does not match/u);

  attestFixture(repoRoot);
  const staleTime = new Date(Date.now() - 60_000);
  utimesSync(join(repoRoot, mutationHtmlReportRelativePath), staleTime, staleTime);
  assert.throws(() => verifyFullRunAttestation(repoRoot), /HTML report timestamp falls outside/u);
});

test('checker provenance rejects a tampered frontend test', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  writeFixtureFile(repoRoot, 'internal/assets/src/js/runtime.test.ts', 'throw new Error();\n');
  assert.throws(
    () => verifyFullRunAttestation(repoRoot),
    /attested mutation inputs changed.*runtime\.test\.ts/u,
  );
});

test('checker provenance rejects changed external Vitest test data', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  writeFixtureFile(
    repoRoot,
    'internal/web/testdata/prefs_golden/01_simple.json',
    '{"payload":{"changed":true}}\n',
  );
  assert.throws(
    () => verifyFullRunAttestation(repoRoot),
    /attested mutation inputs changed.*prefs_golden\/01_simple\.json/u,
  );
});

test('checker provenance rejects a changed Live render contract', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  writeFixtureFile(
    repoRoot,
    'internal/web/testdata/live_render_contract.json',
    '{"version":1,"rows":[{"changed":true}],"regions":[]}\n',
  );
  assert.throws(
    () => verifyFullRunAttestation(repoRoot),
    /attested mutation inputs changed.*live_render_contract\.json/u,
  );
});

test('checker provenance discovers newly added external Vitest test data', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  writeFixtureFile(
    repoRoot,
    'internal/web/testdata/prefs_golden/02_new_case.json',
    '{"payload":{"new":true}}\n',
  );
  assert.throws(
    () => verifyFullRunAttestation(repoRoot),
    /attested mutation inputs changed: added=\[internal\/web\/testdata\/prefs_golden\/02_new_case\.json\]/u,
  );
});

test('runtime must match the exact mise pin and package engine', (t) => {
  const repoRoot = makeFixture(t);
  const nodeVersion = process.versions.node;
  const major = Number(nodeVersion.split('.')[0]);

  assert.doesNotThrow(() => assertMutationRuntimeEligible(repoRoot));
  writeFixtureFile(repoRoot, '.mise.toml', '[tools]\nnode = "0.0.1"\n');
  assert.throws(
    () => assertMutationRuntimeEligible(repoRoot),
    /does not match the exact mise pin/u,
  );

  writeFixtureFile(repoRoot, '.mise.toml', `[tools]\nnode = "${nodeVersion}"\n`);
  writeFixtureFile(
    repoRoot,
    'package.json',
    `${JSON.stringify({ engines: { node: `<${major}` } })}\n`,
  );
  assert.throws(
    () => assertMutationRuntimeEligible(repoRoot),
    /does not satisfy package\.json engines\.node/u,
  );
});

test('checker provenance rejects a changed mise configuration', (t) => {
  const repoRoot = makeFixture(t);
  attestFixture(repoRoot);
  writeFixtureFile(
    repoRoot,
    '.mise.toml',
    `[tools]\nnode = "${process.versions.node}" # semantically unchanged\n`,
  );
  assert.throws(
    () => verifyFullRunAttestation(repoRoot),
    /attested mutation inputs changed.*\.mise\.toml/u,
  );
});

test('checker provenance rejects a different runtime identity', (t) => {
  const repoRoot = makeFixture(t);
  for (const field of ['node', 'platform', 'arch']) {
    attestFixture(repoRoot);
    const attestationPath = join(repoRoot, fullRunAttestationRelativePath);
    const attestation = JSON.parse(readFileSync(attestationPath, 'utf8'));
    attestation.runtime[field] = `hostile-${field}`;
    writeFileSync(attestationPath, `${JSON.stringify(attestation)}\n`);

    assert.throws(
      () => verifyFullRunAttestation(repoRoot),
      new RegExp(`attestation\\.runtime\\.${field} does not match`, 'u'),
      field,
    );
  }
});

test('host executable lookup ignores relative and repository-local PATH entries', (t) => {
  const repoRoot = mkdtempSync(join(tmpdir(), 'readout-host-path-repo-'));
  const safeRoot = mkdtempSync(join(tmpdir(), 'readout-host-path-safe-'));
  t.after(() => rmSync(repoRoot, { force: true, recursive: true }));
  t.after(() => rmSync(safeRoot, { force: true, recursive: true }));
  const unsafeBin = join(repoRoot, 'bin');
  mkdirSync(unsafeBin);
  for (const path of [join(unsafeBin, 'ps'), join(safeRoot, 'ps')]) {
    writeFileSync(path, '#!/bin/sh\nexit 0\n');
    chmodSync(path, 0o755);
  }

  assert.equal(
    resolveSafeHostExecutable('ps', {
      repoRoot,
      searchPath: ['.', 'bin', unsafeBin, safeRoot].join(delimiter),
    }),
    realpathSync(join(safeRoot, 'ps')),
  );
  assert.throws(
    () =>
      resolveSafeHostExecutable('ps', {
        repoRoot,
        searchPath: ['.', 'bin', unsafeBin].join(delimiter),
      }),
    /cannot locate a safe executable/u,
  );
});

test('concurrent stale-lock recovery publishes exactly one new owner', async (t) => {
  const fixtureRoot = mkdtempSync(join(tmpdir(), 'readout-lock-race-'));
  t.after(() => rmSync(fixtureRoot, { force: true, recursive: true }));
  const lockPath = join(fixtureRoot, mutationLockRelativePath);
  const recoveryPath = join(fixtureRoot, mutationRecoveryRelativePath);
  writeFixtureFile(
    fixtureRoot,
    mutationLockRelativePath,
    `${JSON.stringify({ version: 3, pid: 999_999_999, token: 'stale-token-000000000000' })}\n`,
  );
  const workerPath = join(fixtureRoot, 'lock-worker.mjs');
  writeFixtureFile(
    fixtureRoot,
    'lock-worker.mjs',
    `
import { existsSync, writeFileSync } from 'node:fs';
const [moduleUrl, configJson] = process.argv.slice(2);
const config = JSON.parse(configJson);
const { acquireMutationLock, releaseOwnedMutationLock } = await import(moduleUrl);
const waitArray = new Int32Array(new SharedArrayBuffer(4));
const waitForFile = (path) => {
  while (!existsSync(path)) Atomics.wait(waitArray, 0, 0, 10);
};
writeFileSync(config.readyPath, 'ready');
waitForFile(config.goPath);
try {
  const token = acquireMutationLock({
    isOwnerAlive: () => false,
    lockPath: config.lockPath,
    reclaim: () => {
      writeFileSync(config.reclaimPath, 'claimed', { flag: 'wx' });
      waitForFile(config.releaseRecoveryPath);
    },
    record: { version: 3, pid: process.pid, token: config.token },
    recoveryPath: config.recoveryPath,
  });
  writeFileSync(config.acquiredPath, token);
  waitForFile(config.releaseOwnerPath);
  releaseOwnedMutationLock(config.lockPath, token);
} catch (error) {
  writeFileSync(config.errorPath, error.message);
  process.exitCode = 2;
}
`,
  );
  const shared = {
    goPath: join(fixtureRoot, 'go'),
    lockPath,
    recoveryPath,
    releaseOwnerPath: join(fixtureRoot, 'release-owner'),
    releaseRecoveryPath: join(fixtureRoot, 'release-recovery'),
  };
  const moduleUrl = new URL('./mutation-lock.mjs', import.meta.url).href;
  const workers = ['a', 'b'].map((id) => {
    const config = {
      ...shared,
      acquiredPath: join(fixtureRoot, `acquired-${id}`),
      errorPath: join(fixtureRoot, `error-${id}`),
      readyPath: join(fixtureRoot, `ready-${id}`),
      reclaimPath: join(fixtureRoot, `reclaim-${id}`),
      token: `owner-${id}-0000000000000000`,
    };
    return {
      id,
      process: spawn(process.execPath, [workerPath, moduleUrl, JSON.stringify(config)]),
    };
  });
  t.after(() => {
    for (const worker of workers) {
      if (worker.process.exitCode === null) worker.process.kill('SIGKILL');
    }
  });
  const results = workers.map(({ process: child }) => childResult(child));
  await waitUntil(
    () => workers.every(({ id }) => existsSync(join(fixtureRoot, `ready-${id}`))),
    'both lock contenders',
  );
  writeFileSync(shared.goPath, 'go');
  await waitUntil(
    () => workers.filter(({ id }) => existsSync(join(fixtureRoot, `reclaim-${id}`))).length === 1,
    'one atomic recovery claimant',
  );
  await waitUntil(
    () => workers.filter(({ id }) => existsSync(join(fixtureRoot, `error-${id}`))).length === 1,
    'the losing recovery contender to fail closed',
  );
  assert.equal(existsSync(recoveryPath), true);

  writeFileSync(shared.releaseRecoveryPath, 'release');
  await waitUntil(
    () => workers.filter(({ id }) => existsSync(join(fixtureRoot, `acquired-${id}`))).length === 1,
    'one published lock owner',
  );
  const owner = workers.find(({ id }) => existsSync(join(fixtureRoot, `acquired-${id}`)));
  assert.equal(
    JSON.parse(readFileSync(lockPath, 'utf8')).token,
    `owner-${owner.id}-0000000000000000`,
  );
  writeFileSync(shared.releaseOwnerPath, 'release');
  const exits = await Promise.all(results);
  assert.deepEqual(exits.map(({ code }) => code).sort(), [0, 2]);
  assert.equal(existsSync(lockPath), false);
  assert.equal(existsSync(recoveryPath), false);
});

test('stale recovery never deletes a late publisher and abandoned recovery fails closed', (t) => {
  const fixtureRoot = mkdtempSync(join(tmpdir(), 'readout-lock-late-publisher-'));
  t.after(() => rmSync(fixtureRoot, { force: true, recursive: true }));
  const lockPath = join(fixtureRoot, '.guard.lock');
  const recoveryPath = join(fixtureRoot, '.guard.recovery');
  mkdirSync(fixtureRoot, { recursive: true });
  writeFileSync(
    lockPath,
    `${JSON.stringify({ version: 3, pid: 999_999_999, token: 'stale-0000000000000000' })}\n`,
  );
  const lateCandidatePath = join(fixtureRoot, 'late.candidate');
  const lateRecord = { version: 3, pid: process.pid, token: 'late-00000000000000000' };
  writeFileSync(lateCandidatePath, `${JSON.stringify(lateRecord)}\n`);

  assert.throws(
    () =>
      acquireMutationLock({
        afterStaleUnlinked: () => linkSync(lateCandidatePath, lockPath),
        isOwnerAlive: () => false,
        lockPath,
        reclaim: () => {},
        record: { version: 3, pid: process.pid, token: 'self-00000000000000000' },
        recoveryPath,
      }),
    /another launcher acquired the mutation lock during recovery/u,
  );
  assert.deepEqual(JSON.parse(readFileSync(lockPath, 'utf8')), lateRecord);
  assert.equal(existsSync(recoveryPath), false);

  linkSync(lockPath, recoveryPath);
  assert.throws(
    () =>
      acquireMutationLock({
        isOwnerAlive: () => false,
        lockPath,
        reclaim: () => {},
        record: { version: 3, pid: process.pid, token: 'next-00000000000000000' },
        recoveryPath,
      }),
    /recovery is in progress or abandoned/u,
  );
  assert.deepEqual(JSON.parse(readFileSync(lockPath, 'utf8')), lateRecord);
});

test('mutation child kills its entire group after unexpected launcher disconnect', async (t) => {
  const fixtureRoot = mkdtempSync(join(tmpdir(), 'readout-child-orphan-'));
  t.after(() => rmSync(fixtureRoot, { force: true, recursive: true }));
  writeFixtureFile(fixtureRoot, 'hook.mjs', 'export {};\n');
  writeFixtureFile(
    fixtureRoot,
    'stubborn-worker.mjs',
    `
import { writeFileSync } from 'node:fs';
process.on('SIGTERM', () => {});
writeFileSync(process.argv[2], String(process.pid));
setInterval(() => {}, 1_000);
`,
  );
  writeFixtureFile(
    fixtureRoot,
    'fake-stryker.mjs',
    `
import { spawn } from 'node:child_process';
import { existsSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
process.on('SIGTERM', () => {});
const readyPath = process.argv.at(-1);
const workerReadyPath = readyPath + '.worker';
const worker = spawn(
  process.execPath,
  [fileURLToPath(new URL('./stubborn-worker.mjs', import.meta.url)), workerReadyPath],
  { stdio: 'ignore' },
);
while (!existsSync(workerReadyPath)) await new Promise((resolve) => setTimeout(resolve, 10));
writeFileSync(readyPath, JSON.stringify({ stryker: process.pid, worker: worker.pid }));
process.exit(0);
`,
  );
  const readyPath = join(fixtureRoot, 'ready.json');
  const mutationChildPath = fileURLToPath(new URL('./mutation-child.mjs', import.meta.url));
  const child = spawn(
    process.execPath,
    [
      mutationChildPath,
      join(fixtureRoot, 'hook.mjs'),
      join(fixtureRoot, 'fake-stryker.mjs'),
      readyPath,
    ],
    { detached: true, stdio: ['ignore', 'ignore', 'pipe', 'ipc'] },
  );
  let childStderr = '';
  child.stderr.on('data', (chunk) => {
    childStderr += chunk;
  });
  const processGroupId = child.pid;
  assert.ok(processGroupId && processGroupId > 1);
  t.after(() => {
    if (!processGroupExists(processGroupId)) return;
    try {
      process.kill(-processGroupId, 'SIGKILL');
    } catch (error) {
      if (error.code !== 'ESRCH') throw error;
    }
  });
  const result = childExitResult(child);
  const resultMessage = once(child, 'message');
  await once(child, 'spawn');
  child.send({ type: 'start', processGroupId });
  await waitUntil(() => existsSync(readyPath), 'stubborn mutation workload');
  const workload = JSON.parse(readFileSync(readyPath, 'utf8'));
  assert.deepEqual((await resultMessage)[0], { type: 'result', code: 0, signal: null });

  child.disconnect();
  let exit;
  try {
    exit = await result;
  } catch (error) {
    throw new Error(`${error.message}; mutation-child stderr: ${childStderr}`, { cause: error });
  }
  child.stderr.destroy();
  assert.equal(exit.signal, 'SIGKILL');
  await waitUntil(() => !processGroupExists(processGroupId), 'orphaned mutation group exit');
  for (const pid of [workload.stryker, workload.worker]) {
    assert.throws(
      () => process.kill(pid, 0),
      (error) => error.code === 'ESRCH',
    );
  }
});

test('mutation child preserves a normal successful disconnect', async (t) => {
  const fixtureRoot = mkdtempSync(join(tmpdir(), 'readout-child-success-'));
  t.after(() => rmSync(fixtureRoot, { force: true, recursive: true }));
  writeFixtureFile(fixtureRoot, 'hook.mjs', 'export {};\n');
  writeFixtureFile(fixtureRoot, 'fake-stryker.mjs', 'process.exit(0);\n');
  const child = spawn(
    process.execPath,
    [
      fileURLToPath(new URL('./mutation-child.mjs', import.meta.url)),
      join(fixtureRoot, 'hook.mjs'),
      join(fixtureRoot, 'fake-stryker.mjs'),
    ],
    { detached: true, stdio: ['ignore', 'ignore', 'ignore', 'ipc'] },
  );
  const processGroupId = child.pid;
  assert.ok(processGroupId && processGroupId > 1);
  t.after(() => {
    if (!processGroupExists(processGroupId)) return;
    try {
      process.kill(-processGroupId, 'SIGKILL');
    } catch (error) {
      if (error.code !== 'ESRCH') throw error;
    }
  });
  const result = childResult(child);
  const resultMessage = once(child, 'message');
  await once(child, 'spawn');
  child.send({ type: 'start', processGroupId });
  assert.deepEqual((await resultMessage)[0], { type: 'result', code: 0, signal: null });
  child.send({ type: 'release' });
  assert.deepEqual(await result, { code: 0, signal: null });
  assert.equal(processGroupExists(processGroupId), false);
});
