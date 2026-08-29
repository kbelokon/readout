// Fail the mutation quality gate on any unresolved or incomplete mutant.

import { readFileSync, realpathSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import {
  assertImportOnlyModuleSource,
  assertMutationScopeMatchesRuntimeGraph,
  assertTypeOnlyModuleSource,
  verifyFullRunAttestation,
} from './mutation-integrity.mjs';

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..');
const requiredMutatePatterns = [
  'internal/assets/src/js/**/*.ts',
  '!internal/assets/src/js/**/*.test.ts',
  '!internal/assets/src/js/test/**',
  '!internal/assets/src/js/types.ts',
];
const zeroMutantFiles = new Set(['internal/assets/src/js/readout.ts']);
const statusOrder = [
  'Killed',
  'CompileError',
  'Survived',
  'NoCoverage',
  'Timeout',
  'RuntimeError',
  'Ignored',
  'Pending',
];
const directlyResolvedStatuses = new Set(['Killed', 'CompileError']);
const schemaVersionPattern = /^[12](?:\.(?:0|[1-9]\d*)){0,2}$/u;

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function assertPosition(position, path) {
  assert(isObject(position), `${path} must be an object`);
  assert(Number.isInteger(position.line) && position.line >= 1, `${path}.line must be >= 1`);
  assert(Number.isInteger(position.column) && position.column >= 1, `${path}.column must be >= 1`);
}

function assertThreshold(value, path) {
  assert(Number.isInteger(value) && value >= 0 && value <= 100, `${path} must be 0..100`);
}

function parseReport(raw, reportRoot) {
  let report;
  try {
    report = JSON.parse(raw);
  } catch (error) {
    throw new Error(`invalid JSON in mutation report: ${error.message}`, { cause: error });
  }

  assert(isObject(report), 'report must be an object');
  assert(
    typeof report.schemaVersion === 'string' && schemaVersionPattern.test(report.schemaVersion),
    'schemaVersion is missing or unsupported',
  );
  assert(isObject(report.thresholds), 'thresholds must be an object');
  assertThreshold(report.thresholds.high, 'thresholds.high');
  assertThreshold(report.thresholds.low, 'thresholds.low');
  assertThreshold(report.thresholds.break, 'thresholds.break');
  assert(
    report.thresholds.high === 100 &&
      report.thresholds.low === 100 &&
      report.thresholds.break === 0,
    'mutation thresholds must keep high/low at 100 and break at 0',
  );
  assert(isObject(report.config), 'config must be an object');
  assert(report.config.dryRunOnly === false, 'final mutation report must not be a dry run');
  assert(
    report.config.coverageAnalysis === 'perTest',
    'final mutation report must use per-test coverage analysis',
  );
  assert(report.config.ignoreStatic === false, 'ignoreStatic must be false');
  assert(report.config.incremental === false, 'final mutation report must be non-incremental');
  assert(
    Array.isArray(report.config.ignorePatterns) && report.config.ignorePatterns.length === 0,
    'ignorePatterns must be empty',
  );
  assert(
    Array.isArray(report.config.ignorers) && report.config.ignorers.length === 0,
    'ignorers must be empty',
  );
  assert(
    Array.isArray(report.config.testFiles) && report.config.testFiles.length === 0,
    'testFiles must use complete Vitest discovery',
  );
  assert(report.config.testRunner === 'vitest', 'final mutation report must use Vitest');
  assert(
    isObject(report.config.vitest) &&
      report.config.vitest.configFile === 'vitest.config.ts' &&
      report.config.vitest.related === true,
    'final mutation report must use the complete related Vitest configuration',
  );
  assert(
    Array.isArray(report.config.checkers) &&
      report.config.checkers.length === 1 &&
      report.config.checkers[0] === 'typescript',
    'final mutation report must use the TypeScript checker',
  );
  assert(
    report.config.tsconfigFile === 'tsconfig.json',
    'final mutation report must use the production TypeScript config',
  );
  assert(
    isObject(report.config.typescriptChecker) &&
      report.config.typescriptChecker.experimentalNativePreview === true,
    'final mutation report must use the native TypeScript checker',
  );
  assert(
    Array.isArray(report.config.checkerNodeArgs) &&
      report.config.checkerNodeArgs.length === 2 &&
      report.config.checkerNodeArgs[0] === '--import' &&
      report.config.checkerNodeArgs[1] ===
        pathToFileURL(join(reportRoot, '.mutation-stage', 'scripts', 'stryker-typescript-hook.mjs'))
          .href,
    'final mutation report must use the attested TypeScript compatibility hook',
  );
  assert(report.config.inPlace === false, 'inPlace must be false');
  assert(report.config.tempDirName === '.stryker-tmp', 'tempDirName must be .stryker-tmp');
  assert(report.config.cleanTempDir === 'always', 'cleanTempDir must be always');
  assert(report.config.force === false, 'force must be false');
  assert(report.config.allowEmpty === false, 'allowEmpty must be false');
  assert(report.config.disableBail === false, 'disableBail must be false');
  assert(report.config.symlinkNodeModules === true, 'symlinkNodeModules must be true');
  assert(
    Array.isArray(report.config.reporters) &&
      ['clear-text', 'json', 'html'].every(
        (reporter, index) => report.config.reporters[index] === reporter,
      ) &&
      report.config.reporters.length === 3,
    'final mutation report must use the complete reporter set',
  );
  assert(
    Array.isArray(report.config.mutate) &&
      report.config.mutate.length === requiredMutatePatterns.length &&
      requiredMutatePatterns.every((pattern, index) => report.config.mutate[index] === pattern),
    'final mutation report must use the complete production TypeScript scope',
  );
  assert(isObject(report.config.mutator), 'config.mutator must be an object');
  assert(
    Array.isArray(report.config.mutator.excludedMutations) &&
      report.config.mutator.excludedMutations.length === 0,
    'excludedMutations must be empty',
  );
  assert(isObject(report.files), 'files must be an object');

  const files = Object.entries(report.files);
  assert(files.length > 0, 'files must not be empty');
  const expectedFiles = assertMutationScopeMatchesRuntimeGraph(reportRoot);
  const typesFileName = 'internal/assets/src/js/types.ts';
  assertTypeOnlyModuleSource(typesFileName, readFileSync(join(reportRoot, typesFileName), 'utf8'));
  for (const fileName of expectedFiles) {
    const source = readFileSync(join(reportRoot, fileName), 'utf8');
    assert(
      !/\bStryker\s+(?:disable|restore)\b/iu.test(source),
      `${fileName} contains a Stryker suppression comment`,
    );
  }
  const actualFiles = files.map(([fileName]) => fileName).sort();
  for (const fileName of zeroMutantFiles) {
    assertImportOnlyModuleSource(fileName, readFileSync(join(reportRoot, fileName), 'utf8'));
  }
  const missingFiles = expectedFiles.filter(
    (fileName) => !actualFiles.includes(fileName) && !zeroMutantFiles.has(fileName),
  );
  const unexpectedFiles = actualFiles.filter((fileName) => !expectedFiles.includes(fileName));
  assert(
    missingFiles.length === 0 && unexpectedFiles.length === 0,
    `report scope mismatch: missing=[${missingFiles.join(', ')}], ` +
      `unexpected=[${unexpectedFiles.join(', ')}]`,
  );

  const seenIds = new Set();
  let mutantCount = 0;
  for (const [fileName, file] of files) {
    assert(fileName.trim().length > 0, 'file name must not be empty');
    assert(isObject(file), `${fileName} must be an object`);
    assert(typeof file.language === 'string', `${fileName}.language must be a string`);
    assert(typeof file.source === 'string', `${fileName}.source must be a string`);
    assert(
      file.source === readFileSync(join(reportRoot, fileName), 'utf8'),
      `${fileName}.source does not match the current working tree`,
    );
    assert(Array.isArray(file.mutants), `${fileName}.mutants must be an array`);
    if (zeroMutantFiles.has(fileName)) {
      assert(file.mutants.length === 0, `${fileName} must remain import-only`);
    } else {
      assert(file.mutants.length > 0, `${fileName}.mutants must not be empty`);
    }

    for (const [index, mutant] of file.mutants.entries()) {
      const path = `${fileName}.mutants[${index}]`;
      assert(isObject(mutant), `${path} must be an object`);
      assert(typeof mutant.id === 'string' && mutant.id.length > 0, `${path}.id is required`);
      assert(!seenIds.has(mutant.id), `${path}.id duplicates mutant ${mutant.id}`);
      seenIds.add(mutant.id);
      assert(
        typeof mutant.mutatorName === 'string' && mutant.mutatorName.length > 0,
        `${path}.mutatorName is required`,
      );
      assert(statusOrder.includes(mutant.status), `${path}.status is unknown: ${mutant.status}`);
      assert(isObject(mutant.location), `${path}.location must be an object`);
      assertPosition(mutant.location.start, `${path}.location.start`);
      assertPosition(mutant.location.end, `${path}.location.end`);
      mutantCount += 1;
    }
  }
  assert(mutantCount > 0, 'report must contain at least one mutant');

  return { files, mutantCount, schemaVersion: report.schemaVersion };
}

function emptyCounts() {
  return Object.fromEntries(statusOrder.map((status) => [status, 0]));
}

function summarize(files) {
  const totalCounts = emptyCounts();
  const fileCounts = [];

  for (const [fileName, file] of files) {
    const counts = emptyCounts();
    for (const mutant of file.mutants) {
      counts[mutant.status] += 1;
      totalCounts[mutant.status] += 1;
    }
    fileCounts.push({ fileName, counts, total: file.mutants.length });
  }

  return {
    fileCounts: fileCounts.sort((left, right) => left.fileName.localeCompare(right.fileName)),
    totalCounts,
  };
}

function printSummary({ fileCounts, totalCounts }, schemaVersion, mutantCount, log) {
  log(
    `Mutation report schema ${schemaVersion}: ${fileCounts.length} files, ${mutantCount} mutants`,
  );
  log(
    `Directly killed: ${totalCounts.Killed}/${mutantCount} all generated mutants ` +
      `(${((totalCounts.Killed / mutantCount) * 100).toFixed(2)}%; ` +
      'CompileError reported separately)',
  );
  log('Statuses:');
  for (const status of statusOrder) {
    log(`  ${status.padEnd(12)} ${totalCounts[status]}`);
  }
  log('Files:');
  for (const { fileName, counts, total } of fileCounts) {
    const statuses = statusOrder
      .filter((status) => counts[status] > 0)
      .map((status) => `${status}=${counts[status]}`)
      .join(' ');
    log(`  ${fileName}: total=${total} ${statuses}`);
  }
}

export function checkMutationReport(reportRoot = repoRoot, log = console.log) {
  const verifiedRun = verifyFullRunAttestation(reportRoot);
  const { files, mutantCount, schemaVersion } = parseReport(
    verifiedRun.reportBytes.toString('utf8'),
    reportRoot,
  );
  const summary = summarize(files);
  for (const { fileName, counts } of summary.fileCounts) {
    assert(counts.Killed > 0, `${fileName} has no behaviorally killed mutant`);
  }
  printSummary(summary, schemaVersion, mutantCount, log);

  const failures = statusOrder.filter(
    (status) => !directlyResolvedStatuses.has(status) && summary.totalCounts[status] > 0,
  );
  if (failures.length > 0) {
    const details = failures.map((status) => `${status}=${summary.totalCounts[status]}`).join(', ');
    throw new Error(`unresolved mutation statuses: ${details}`);
  }
  // Re-read the proof and inputs after the structural/source checks so a file
  // changed concurrently with this checker cannot leave a passing stale result.
  verifyFullRunAttestation(reportRoot);
  log('Mutation report check passed.');
  return summary;
}

export function isMutationReportCheckerMain(entryPath = process.argv[1]) {
  if (!entryPath) return false;
  try {
    return realpathSync(entryPath) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (isMutationReportCheckerMain()) {
  try {
    checkMutationReport();
  } catch (error) {
    console.error(`Mutation report check failed: ${error.message}`);
    process.exitCode = 1;
  }
}
