import { createHash, randomUUID } from 'node:crypto';
import {
  closeSync,
  existsSync,
  globSync,
  lstatSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  symlinkSync,
  unlinkSync,
  writeFileSync,
} from 'node:fs';
import { dirname, isAbsolute, join, resolve } from 'node:path';
import ts from '@typescript/legacy';
import { buildSync } from 'esbuild';
import { frontendJavaScriptBuildOptions } from './frontend-build-config.mjs';

export const mutationReportRelativePath = 'reports/mutation/mutation.json';
export const mutationHtmlReportRelativePath = 'reports/mutation/index.html';
export const fullRunAttestationRelativePath = 'reports/mutation/full-run.json';
export const mutationLockRelativePath = 'reports/mutation/.guard.lock';
export const mutationRecoveryRelativePath = 'reports/mutation/.guard.recovery';
export const mutationLogRelativePath = 'reports/mutation/latest.log';
export const mutationStageRelativePath = '.mutation-stage';

const fixedMutationInputs = [
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
const mutationTestDataPatterns = [
  // prefs.test.ts enumerates this shared Go/TypeScript codec corpus at module load.
  'internal/web/testdata/prefs_golden/**/*.json',
];

function sha256(value) {
  return createHash('sha256').update(value).digest('hex');
}

function sortedUnique(values) {
  return [...new Set(values)].sort((left, right) => left.localeCompare(right));
}

function assertSafeRelativePath(fileName, context) {
  if (
    typeof fileName !== 'string' ||
    fileName.length === 0 ||
    isAbsolute(fileName) ||
    fileName.includes('\\') ||
    fileName.includes('\0') ||
    fileName.split('/').some((part) => part.length === 0 || part === '.' || part === '..')
  ) {
    throw new Error(`${context} contains an unsafe path: ${fileName}`);
  }
}

function pathEntryExists(path) {
  try {
    lstatSync(path);
    return true;
  } catch (error) {
    if (error.code === 'ENOENT') return false;
    throw error;
  }
}

function ensurePlainDirectory(path, label) {
  try {
    const stats = lstatSync(path);
    if (stats.isSymbolicLink() || !stats.isDirectory()) {
      throw new Error(`${label} must be a real directory, not a symlink or file: ${path}`);
    }
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    mkdirSync(path, { mode: 0o700 });
    const stats = lstatSync(path);
    if (stats.isSymbolicLink() || !stats.isDirectory()) {
      throw new Error(`${label} could not be created as a real directory: ${path}`);
    }
  }
}

export function ensureMutationArtifactDirectory(repoRoot) {
  const resolvedRepoRoot = resolve(repoRoot);
  const reportsRoot = join(resolvedRepoRoot, 'reports');
  const artifactRoot = join(reportsRoot, 'mutation');
  if (resolve(artifactRoot) !== artifactRoot) {
    throw new Error(`refusing unexpected mutation artifact path: ${artifactRoot}`);
  }
  // Create and validate one component at a time. Recursive mkdir/rm through an
  // existing symlink could otherwise escape the repository.
  ensurePlainDirectory(reportsRoot, 'mutation reports parent');
  ensurePlainDirectory(artifactRoot, 'mutation artifact directory');
  return artifactRoot;
}

export function openMutationLogFile(repoRoot) {
  const artifactRoot = ensureMutationArtifactDirectory(repoRoot);
  const logPath = join(artifactRoot, 'latest.log');
  const candidatePath = `${logPath}.${process.pid}.${randomUUID()}.candidate`;
  let descriptor;
  try {
    descriptor = openSync(candidatePath, 'wx', 0o600);
    // Renaming a fresh inode over latest.log replaces a symlink or hard link
    // itself without opening or truncating its external target.
    renameSync(candidatePath, logPath);
    return { descriptor, logPath };
  } catch (error) {
    if (descriptor !== undefined) closeSync(descriptor);
    rmSync(candidatePath, { force: true });
    throw error;
  }
}

export function mutationStagePath(repoRoot) {
  const resolvedRepoRoot = resolve(repoRoot);
  const stageRoot = resolve(resolvedRepoRoot, mutationStageRelativePath);
  if (stageRoot !== join(resolvedRepoRoot, mutationStageRelativePath)) {
    throw new Error(`refusing unexpected mutation stage path: ${stageRoot}`);
  }
  return stageRoot;
}

export function collectMutationRuntimePaths(repoRoot) {
  return globSync('internal/assets/src/js/**/*.ts', { cwd: repoRoot })
    .filter(
      (fileName) =>
        !fileName.endsWith('.test.ts') &&
        fileName !== 'internal/assets/src/js/types.ts' &&
        !fileName.startsWith('internal/assets/src/js/test/'),
    )
    .sort();
}

function buildBundledRuntime(repoRoot) {
  let result;
  try {
    result = buildSync(
      frontendJavaScriptBuildOptions(repoRoot, {
        metafile: true,
        logLevel: 'silent',
      }),
    );
  } catch (error) {
    throw new Error(`cannot derive the shipped frontend module graph: ${error.message}`, {
      cause: error,
    });
  }
  if (result.metafile === undefined) {
    throw new Error('esbuild did not return a frontend module graph');
  }
  const bundledInputs = Object.keys(result.metafile.inputs);
  const outsideMutationRoot = bundledInputs.filter(
    (fileName) =>
      !fileName.startsWith('internal/assets/src/js/') && !fileName.startsWith('node_modules/'),
  );
  if (outsideMutationRoot.length > 0) {
    throw new Error(
      `shipped frontend graph contains project inputs outside the mutation root: ` +
        `[${outsideMutationRoot.sort().join(', ')}]`,
    );
  }
  const projectInputs = bundledInputs
    .filter((fileName) => fileName.startsWith('internal/assets/src/js/'))
    .sort();
  const nonTypeScript = projectInputs.filter((fileName) => !fileName.endsWith('.ts'));
  if (nonTypeScript.length > 0) {
    throw new Error(
      `shipped frontend graph contains non-TypeScript inputs: [${nonTypeScript.join(', ')}]`,
    );
  }
  if (!Array.isArray(result.outputFiles) || result.outputFiles.length !== 1) {
    throw new Error('esbuild did not return exactly one shipped frontend output');
  }
  return { output: result.outputFiles[0].text, paths: projectInputs };
}

export function collectBundledRuntimePaths(repoRoot) {
  return buildBundledRuntime(repoRoot).paths;
}

export function assertMutationScopeMatchesRuntimeGraph(repoRoot) {
  const mutationPaths = collectMutationRuntimePaths(repoRoot);
  const { output, paths: runtimePaths } = buildBundledRuntime(repoRoot);
  const missingFromGraph = mutationPaths.filter((fileName) => !runtimePaths.includes(fileName));
  const excludedFromMutation = runtimePaths.filter((fileName) => !mutationPaths.includes(fileName));
  if (missingFromGraph.length > 0 || excludedFromMutation.length > 0) {
    throw new Error(
      `mutation candidates do not equal the shipped frontend graph: ` +
        `missing-from-graph=[${missingFromGraph.join(', ')}], ` +
        `excluded-from-mutation=[${excludedFromMutation.join(', ')}]`,
    );
  }
  const shippedPath = join(repoRoot, 'internal/assets/static/readout.js');
  if (output !== readFileSync(shippedPath, 'utf8')) {
    throw new Error('shipped frontend bundle is stale relative to its attested build recipe');
  }
  return mutationPaths;
}

export function collectMutationInputPaths(repoRoot) {
  const runtimeSources = globSync('internal/assets/src/js/**/*.ts', { cwd: repoRoot }).filter(
    (fileName) =>
      !fileName.endsWith('.test.ts') && !fileName.startsWith('internal/assets/src/js/test/'),
  );
  const frontendTests = [
    ...globSync('internal/assets/src/js/**/*.test.ts', { cwd: repoRoot }),
    ...globSync('internal/assets/src/js/test/**/*.ts', { cwd: repoRoot }),
  ];
  const frontendTestData = mutationTestDataPatterns.flatMap((pattern) => {
    const matches = globSync(pattern, { cwd: repoRoot });
    if (matches.length === 0) {
      throw new Error(`mutation test-data pattern matched no files: ${pattern}`);
    }
    return matches;
  });
  const paths = sortedUnique([
    ...runtimeSources,
    ...frontendTests,
    ...frontendTestData,
    ...fixedMutationInputs,
  ]);

  for (const fileName of paths) {
    if (!existsSync(join(repoRoot, fileName))) {
      throw new Error(`mutation input is missing: ${fileName}`);
    }
  }
  return paths;
}

export function currentMutationRuntime() {
  return {
    node: process.version,
    platform: process.platform,
    arch: process.arch,
  };
}

function parseExactVersion(value, fieldName) {
  if (
    typeof value !== 'string' ||
    !/^v?(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/u.test(value)
  ) {
    throw new Error(`${fieldName} must be an exact numeric version`);
  }
  return value.replace(/^v/u, '').split('.').map(Number);
}

function compareVersions(left, right) {
  for (let index = 0; index < 3; index += 1) {
    if (left[index] !== right[index]) return left[index] < right[index] ? -1 : 1;
  }
  return 0;
}

function satisfiesSimpleNodeRange(version, range) {
  if (typeof range !== 'string' || range.trim().length === 0) return false;
  const parsedVersion = parseExactVersion(version, 'Node version');
  const comparators = range.trim().split(/\s+/u);
  return comparators.every((comparator) => {
    const match = /^(>=|<=|>|<|=)?(\d+(?:\.\d+){0,2})$/u.exec(comparator);
    if (!match) {
      throw new Error(`package.json engines.node uses an unsupported range: ${range}`);
    }
    const boundaryParts = match[2].split('.').map(Number);
    while (boundaryParts.length < 3) boundaryParts.push(0);
    const comparison = compareVersions(parsedVersion, boundaryParts);
    switch (match[1] ?? '=') {
      case '>=':
        return comparison >= 0;
      case '<=':
        return comparison <= 0;
      case '>':
        return comparison > 0;
      case '<':
        return comparison < 0;
      default:
        return comparison === 0;
    }
  });
}

function readMiseNodePin(repoRoot) {
  const source = readFileSync(join(repoRoot, '.mise.toml'), 'utf8');
  let section = '';
  const pins = [];
  for (const rawLine of source.split(/\r?\n/u)) {
    const line = rawLine.trim();
    if (line.length === 0 || line.startsWith('#')) continue;
    const sectionMatch = /^\[([^\]]+)\](?:\s*#.*)?$/u.exec(line);
    if (sectionMatch) {
      section = sectionMatch[1];
      continue;
    }
    if (section !== 'tools' || !/^node\s*=/u.test(line)) continue;
    const pinMatch = /^node\s*=\s*"([^"]+)"(?:\s*#.*)?$/u.exec(line);
    if (!pinMatch) throw new Error('.mise.toml [tools].node must be an exact quoted version');
    pins.push(pinMatch[1]);
  }
  if (pins.length !== 1) {
    throw new Error(`.mise.toml must contain exactly one [tools].node pin, found ${pins.length}`);
  }
  parseExactVersion(pins[0], '.mise.toml [tools].node');
  return pins[0];
}

export function assertMutationRuntimeEligible(repoRoot, runtime = currentMutationRuntime()) {
  const pin = readMiseNodePin(repoRoot);
  const actual = runtime.node?.replace(/^v/u, '');
  parseExactVersion(actual, 'runtime Node version');
  if (actual !== pin) {
    throw new Error(`runtime Node ${actual} does not match the exact mise pin ${pin}`);
  }

  let packageJson;
  try {
    packageJson = JSON.parse(readFileSync(join(repoRoot, 'package.json'), 'utf8'));
  } catch (error) {
    throw new Error(`cannot read package.json Node engine: ${error.message}`, { cause: error });
  }
  const engine = packageJson?.engines?.node;
  if (!satisfiesSimpleNodeRange(actual, engine)) {
    throw new Error(`runtime Node ${actual} does not satisfy package.json engines.node ${engine}`);
  }
  return { engine, pin };
}

function inputSetDigest(files) {
  return sha256(
    Object.entries(files)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([fileName, digest]) => `${fileName}\0${digest}\n`)
      .join(''),
  );
}

export function snapshotMutationInputs(repoRoot) {
  const files = Object.fromEntries(
    collectMutationInputPaths(repoRoot).map((fileName) => [
      fileName,
      sha256(readFileSync(join(repoRoot, fileName))),
    ]),
  );
  return { files, sha256: inputSetDigest(files) };
}

export function assertMutationInputsEqual(expected, actual, context = 'mutation inputs') {
  const expectedNames = Object.keys(expected.files).sort();
  const actualNames = Object.keys(actual.files).sort();
  const added = actualNames.filter((fileName) => !expectedNames.includes(fileName));
  const removed = expectedNames.filter((fileName) => !actualNames.includes(fileName));
  const changed = expectedNames.filter(
    (fileName) =>
      actual.files[fileName] !== undefined && actual.files[fileName] !== expected.files[fileName],
  );
  if (
    expected.sha256 !== actual.sha256 ||
    added.length > 0 ||
    removed.length > 0 ||
    changed.length > 0
  ) {
    throw new Error(
      `${context} changed: added=[${added.join(', ')}], removed=[${removed.join(', ')}], ` +
        `changed=[${changed.join(', ')}]`,
    );
  }
}

function parseIsoTimestamp(value, fieldName) {
  if (typeof value !== 'string') throw new Error(`${fieldName} must be an ISO timestamp`);
  const milliseconds = Date.parse(value);
  if (!Number.isFinite(milliseconds) || new Date(milliseconds).toISOString() !== value) {
    throw new Error(`${fieldName} must be an ISO timestamp`);
  }
  return milliseconds;
}

function assertDigest(value, fieldName) {
  if (typeof value !== 'string' || !/^[a-f0-9]{64}$/u.test(value)) {
    throw new Error(`${fieldName} must be a SHA-256 digest`);
  }
}

function assertAttestedInputSnapshot(value) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('attestation.inputs must be an object');
  }
  if (value.files === null || typeof value.files !== 'object' || Array.isArray(value.files)) {
    throw new Error('attestation.inputs.files must be an object');
  }
  for (const [fileName, digest] of Object.entries(value.files)) {
    assertSafeRelativePath(fileName, 'attestation');
    assertDigest(digest, `attestation.inputs.files[${JSON.stringify(fileName)}]`);
  }
  assertDigest(value.sha256, 'attestation.inputs.sha256');
  if (inputSetDigest(value.files) !== value.sha256) {
    throw new Error('attestation input-set digest is inconsistent');
  }
}

export function removeMutationInputStage(repoRoot) {
  const stageRoot = mutationStagePath(repoRoot);
  if (!pathEntryExists(stageRoot)) return;
  if (lstatSync(stageRoot).isSymbolicLink()) {
    unlinkSync(stageRoot);
    return;
  }
  rmSync(stageRoot, { recursive: true, force: true });
}

export function createMutationInputStage(repoRoot, expectedInputs) {
  assertAttestedInputSnapshot(expectedInputs);
  const stageRoot = mutationStagePath(repoRoot);
  if (pathEntryExists(stageRoot)) {
    throw new Error(`mutation stage already exists: ${stageRoot}`);
  }
  const nodeModulesPath = join(repoRoot, 'node_modules');
  if (!existsSync(nodeModulesPath) || !statSync(nodeModulesPath).isDirectory()) {
    throw new Error('node_modules is missing; run npm ci before staging mutation inputs');
  }

  mkdirSync(stageRoot, { mode: 0o700 });
  try {
    for (const fileName of Object.keys(expectedInputs.files).sort()) {
      assertSafeRelativePath(fileName, 'mutation input snapshot');
      const sourcePath = join(repoRoot, fileName);
      const sourceBytes = readFileSync(sourcePath);
      if (sha256(sourceBytes) !== expectedInputs.files[fileName]) {
        throw new Error(`mutation input changed while staging: ${fileName}`);
      }
      const targetPath = join(stageRoot, fileName);
      mkdirSync(dirname(targetPath), { recursive: true });
      writeFileSync(targetPath, sourceBytes, {
        flag: 'wx',
        mode: statSync(sourcePath).mode & 0o777,
      });
    }
    symlinkSync(nodeModulesPath, join(stageRoot, 'node_modules'), 'dir');
    const stagedInputs = snapshotMutationInputs(stageRoot);
    assertMutationInputsEqual(expectedInputs, stagedInputs, 'staged mutation inputs');
    return { inputs: stagedInputs, stageRoot };
  } catch (error) {
    removeMutationInputStage(repoRoot);
    throw error;
  }
}

export function invalidateFullMutationReport(repoRoot) {
  ensureMutationArtifactDirectory(repoRoot);
  const resolvedRepoRoot = resolve(repoRoot);
  for (const relativePath of [
    // Remove the proof first. If a malformed report path makes a later unlink
    // fail, no previously attested result can remain usable.
    fullRunAttestationRelativePath,
    mutationReportRelativePath,
    mutationHtmlReportRelativePath,
  ]) {
    rmSync(join(resolvedRepoRoot, relativePath), { force: true });
  }
}

export function publishFullMutationReports(repoRoot, { htmlReportBytes, reportBytes }) {
  ensureMutationArtifactDirectory(repoRoot);
  if (reportBytes.length === 0 || htmlReportBytes.length === 0) {
    throw new Error('full mutation JSON and HTML reports must both be non-empty');
  }
  const resolvedRepoRoot = resolve(repoRoot);
  const reportPath = join(resolvedRepoRoot, mutationReportRelativePath);
  const htmlReportPath = join(resolvedRepoRoot, mutationHtmlReportRelativePath);
  const attestationPath = join(resolvedRepoRoot, fullRunAttestationRelativePath);
  for (const targetPath of [reportPath, htmlReportPath, attestationPath]) {
    if (pathEntryExists(targetPath)) {
      throw new Error(`refusing to overwrite an existing full-run artifact: ${targetPath}`);
    }
  }

  const nonce = `${process.pid}.${randomUUID()}`;
  const candidates = [
    {
      bytes: reportBytes,
      candidatePath: `${reportPath}.${nonce}.candidate`,
      targetPath: reportPath,
    },
    {
      bytes: htmlReportBytes,
      candidatePath: `${htmlReportPath}.${nonce}.candidate`,
      targetPath: htmlReportPath,
    },
  ];
  try {
    for (const { bytes, candidatePath } of candidates) {
      writeFileSync(candidatePath, bytes, { flag: 'wx', mode: 0o600 });
    }
    for (const { candidatePath, targetPath } of candidates) renameSync(candidatePath, targetPath);
  } catch (error) {
    for (const { candidatePath, targetPath } of candidates) {
      rmSync(candidatePath, { force: true });
      rmSync(targetPath, { force: true });
    }
    throw error;
  } finally {
    for (const { candidatePath } of candidates) rmSync(candidatePath, { force: true });
  }
}

export function createFullRunAttestation({
  completedAt,
  executionInputs,
  htmlReportBytes,
  inputs,
  repoRoot,
  reportBytes,
  runId,
  startedAt,
}) {
  assertMutationRuntimeEligible(repoRoot);
  assertAttestedInputSnapshot(inputs);
  assertAttestedInputSnapshot(executionInputs);
  assertMutationInputsEqual(inputs, executionInputs, 'staged execution inputs');
  if (reportBytes.length === 0 || htmlReportBytes.length === 0) {
    throw new Error('full mutation JSON and HTML reports must both be non-empty');
  }
  return {
    schemaVersion: 4,
    runId,
    mode: 'full',
    successful: true,
    startedAt,
    completedAt,
    runtime: currentMutationRuntime(),
    execution: {
      cwd: mutationStageRelativePath,
      inputsSha256: executionInputs.sha256,
    },
    report: {
      path: mutationReportRelativePath,
      sha256: sha256(reportBytes),
    },
    htmlReport: {
      path: mutationHtmlReportRelativePath,
      sha256: sha256(htmlReportBytes),
    },
    inputs,
  };
}

function assertRuntimeMetadata(value) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('attestation.runtime must be an object');
  }
  const current = currentMutationRuntime();
  for (const field of ['node', 'platform', 'arch']) {
    if (typeof value[field] !== 'string' || value[field] !== current[field]) {
      throw new Error(
        `attestation.runtime.${field} does not match this process: ` +
          `${JSON.stringify(value[field])} != ${JSON.stringify(current[field])}`,
      );
    }
  }
}

export function writeFullRunAttestation(repoRoot, attestation) {
  ensureMutationArtifactDirectory(repoRoot);
  const attestationPath = join(resolve(repoRoot), fullRunAttestationRelativePath);
  const candidatePath = `${attestationPath}.${process.pid}.${randomUUID()}.candidate`;
  writeFileSync(candidatePath, `${JSON.stringify(attestation, null, 2)}\n`, {
    encoding: 'utf8',
    flag: 'wx',
    mode: 0o600,
  });
  try {
    renameSync(candidatePath, attestationPath);
  } finally {
    rmSync(candidatePath, { force: true });
  }
}

export function verifyFullRunAttestation(repoRoot) {
  ensureMutationArtifactDirectory(repoRoot);
  if (existsSync(join(repoRoot, mutationLockRelativePath))) {
    throw new Error('mutation guard lock still exists; the full run did not finish cleanly');
  }
  if (existsSync(join(repoRoot, mutationRecoveryRelativePath))) {
    throw new Error('mutation recovery claim still exists; the full run did not finish cleanly');
  }
  if (pathEntryExists(mutationStagePath(repoRoot))) {
    throw new Error('mutation stage still exists; the full run did not finish cleanly');
  }

  const attestationPath = join(repoRoot, fullRunAttestationRelativePath);
  let attestation;
  try {
    attestation = JSON.parse(readFileSync(attestationPath, 'utf8'));
  } catch (error) {
    throw new Error(`cannot read a valid full-run attestation: ${error.message}`, { cause: error });
  }
  if (attestation === null || typeof attestation !== 'object' || Array.isArray(attestation)) {
    throw new Error('full-run attestation must be an object');
  }
  if (attestation.schemaVersion !== 4) throw new Error('unsupported full-run attestation schema');
  if (attestation.mode !== 'full' || attestation.successful !== true) {
    throw new Error('attestation is not from a successful full mutation run');
  }
  assertRuntimeMetadata(attestation.runtime);
  assertMutationRuntimeEligible(repoRoot, attestation.runtime);
  if (
    attestation.execution === null ||
    typeof attestation.execution !== 'object' ||
    Array.isArray(attestation.execution) ||
    attestation.execution.cwd !== mutationStageRelativePath
  ) {
    throw new Error('attestation execution root is invalid');
  }
  assertDigest(attestation.execution.inputsSha256, 'attestation.execution.inputsSha256');
  if (typeof attestation.runId !== 'string' || attestation.runId.length < 16) {
    throw new Error('attestation.runId is invalid');
  }
  const startedAt = parseIsoTimestamp(attestation.startedAt, 'attestation.startedAt');
  const completedAt = parseIsoTimestamp(attestation.completedAt, 'attestation.completedAt');
  if (completedAt < startedAt || completedAt > Date.now() + 60_000) {
    throw new Error('attestation timestamps are inconsistent');
  }
  if (
    attestation.report === null ||
    typeof attestation.report !== 'object' ||
    Array.isArray(attestation.report) ||
    attestation.report.path !== mutationReportRelativePath
  ) {
    throw new Error('attestation report path is invalid');
  }
  assertDigest(attestation.report.sha256, 'attestation.report.sha256');
  if (
    attestation.htmlReport === null ||
    typeof attestation.htmlReport !== 'object' ||
    Array.isArray(attestation.htmlReport) ||
    attestation.htmlReport.path !== mutationHtmlReportRelativePath
  ) {
    throw new Error('attestation HTML report path is invalid');
  }
  assertDigest(attestation.htmlReport.sha256, 'attestation.htmlReport.sha256');
  assertAttestedInputSnapshot(attestation.inputs);
  if (attestation.execution.inputsSha256 !== attestation.inputs.sha256) {
    throw new Error('attestation execution inputs do not match the attested working-tree inputs');
  }

  const reportPath = join(repoRoot, mutationReportRelativePath);
  const reportBytes = readFileSync(reportPath);
  if (sha256(reportBytes) !== attestation.report.sha256) {
    throw new Error('mutation report digest does not match the successful full run');
  }
  const reportModifiedAt = statSync(reportPath).mtimeMs;
  if (reportModifiedAt < startedAt - 1_000 || reportModifiedAt > completedAt + 1_000) {
    throw new Error('mutation report timestamp falls outside the attested full run');
  }

  const htmlReportPath = join(repoRoot, mutationHtmlReportRelativePath);
  const htmlReportBytes = readFileSync(htmlReportPath);
  if (htmlReportBytes.length === 0 || sha256(htmlReportBytes) !== attestation.htmlReport.sha256) {
    throw new Error('mutation HTML report digest does not match the successful full run');
  }
  const htmlReportModifiedAt = statSync(htmlReportPath).mtimeMs;
  if (htmlReportModifiedAt < startedAt - 1_000 || htmlReportModifiedAt > completedAt + 1_000) {
    throw new Error('mutation HTML report timestamp falls outside the attested full run');
  }

  const currentInputs = snapshotMutationInputs(repoRoot);
  assertMutationInputsEqual(attestation.inputs, currentInputs, 'attested mutation inputs');
  return { attestation, htmlReportBytes, reportBytes };
}

function parseTypeScript(fileName, source) {
  const sourceFile = ts.createSourceFile(
    fileName,
    source,
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TS,
  );
  if (sourceFile.parseDiagnostics.length > 0) {
    const diagnostic = sourceFile.parseDiagnostics[0];
    throw new Error(
      `${fileName} cannot be parsed: ${ts.flattenDiagnosticMessageText(diagnostic.messageText, '\n')}`,
    );
  }
  return sourceFile;
}

function hasDeclareModifier(statement) {
  return (
    statement.modifiers?.some((modifier) => modifier.kind === ts.SyntaxKind.DeclareKeyword) ?? false
  );
}

function isTypeOnlyStatement(statement) {
  if (ts.isInterfaceDeclaration(statement) || ts.isTypeAliasDeclaration(statement)) return true;
  if (ts.isImportDeclaration(statement)) return statement.importClause?.isTypeOnly === true;
  if (ts.isExportDeclaration(statement)) return statement.isTypeOnly;
  if (ts.isModuleDeclaration(statement) && hasDeclareModifier(statement)) {
    if (statement.body === undefined) return true;
    if (ts.isModuleBlock(statement.body)) {
      return statement.body.statements.every(isTypeOnlyStatement);
    }
    return isTypeOnlyStatement(statement.body);
  }
  return false;
}

function statementLocation(sourceFile, statement) {
  const location = sourceFile.getLineAndCharacterOfPosition(statement.getStart(sourceFile));
  return `${sourceFile.fileName}:${location.line + 1}:${location.character + 1}`;
}

export function assertTypeOnlyModuleSource(fileName, source) {
  const sourceFile = parseTypeScript(fileName, source);
  for (const statement of sourceFile.statements) {
    if (!isTypeOnlyStatement(statement)) {
      throw new Error(
        `${fileName} is no longer type-only: ${ts.SyntaxKind[statement.kind]} at ` +
          statementLocation(sourceFile, statement),
      );
    }
  }
}

export function assertImportOnlyModuleSource(fileName, source) {
  const sourceFile = parseTypeScript(fileName, source);
  if (sourceFile.statements.length === 0) {
    throw new Error(`${fileName} must contain at least one side-effect import`);
  }
  for (const statement of sourceFile.statements) {
    if (!ts.isImportDeclaration(statement) || statement.importClause !== undefined) {
      throw new Error(
        `${fileName} is no longer side-effect-import-only: ${ts.SyntaxKind[statement.kind]} at ` +
          statementLocation(sourceFile, statement),
      );
    }
  }
}
