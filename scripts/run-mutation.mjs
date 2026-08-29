// Resource-monitored Stryker launcher. Stryker owns its worker pool; this wrapper
// samples its process group and stops a run only after a resource or process-safety
// limit is observed. A full campaign deliberately has no wall-clock deadline.

import { execFileSync, spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import {
  createWriteStream,
  existsSync,
  lstatSync,
  readFileSync,
  rmSync,
  statfsSync,
  statSync,
  unlinkSync,
} from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { parseMutationCliArguments } from './mutation-cli.mjs';
import { resolveSafeHostExecutable } from './mutation-host.mjs';
import {
  assertMutationInputsEqual,
  assertMutationRuntimeEligible,
  assertMutationScopeMatchesRuntimeGraph,
  createFullRunAttestation,
  createMutationInputStage,
  ensureMutationArtifactDirectory,
  fullRunAttestationRelativePath,
  invalidateFullMutationReport,
  mutationHtmlReportRelativePath,
  mutationReportRelativePath,
  mutationStagePath,
  openMutationLogFile,
  publishFullMutationReports,
  removeMutationInputStage,
  snapshotMutationInputs,
  writeFullRunAttestation,
} from './mutation-integrity.mjs';
import {
  acquireMutationLock,
  releaseOwnedMutationLock,
  updateOwnedMutationLock,
} from './mutation-lock.mjs';

const GIB = 1024 ** 3;
const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const installedStrykerBin = join(repoRoot, 'node_modules', '.bin', 'stryker');
const stageRoot = mutationStagePath(repoRoot);
const strykerBin = join(stageRoot, 'node_modules', '.bin', 'stryker');
const compatibilityHook = pathToFileURL(
  join(stageRoot, 'scripts', 'stryker-typescript-hook.mjs'),
).href;
const sourceMutationChild = join(repoRoot, 'scripts', 'mutation-child.mjs');
const mutationChild = join(stageRoot, 'scripts', 'mutation-child.mjs');
const legacyTempDir = join(repoRoot, '.stryker-tmp');
const reportDir = join(repoRoot, 'reports', 'mutation');
const stagedReportPath = join(stageRoot, mutationReportRelativePath);
const stagedHtmlReportPath = join(stageRoot, mutationHtmlReportRelativePath);
const attestationPath = join(repoRoot, fullRunAttestationRelativePath);
const logPath = join(reportDir, 'latest.log');
const lockPath = join(reportDir, '.guard.lock');
const recoveryPath = join(reportDir, '.guard.recovery');
const guardEnvironment = 'READOUT_MUTATION_GUARD';
const modeEnvironment = 'READOUT_MUTATION_MODE';
const runIdEnvironment = 'READOUT_MUTATION_RUN_ID';
const supportedPlatforms = new Set(['darwin', 'linux']);
const { concurrency, mode, strykerArgs } = parseMutationCliArguments(process.argv.slice(2));
const fullRun = mode === 'full';
const runId = randomUUID();
const childMarker = `readout-mutation-${runId}`;

if (!supportedPlatforms.has(process.platform)) {
  throw new Error('the mutation resource guard supports only POSIX macOS and Linux hosts');
}
assertMutationRuntimeEligible(repoRoot);
const duBin = resolveSafeHostExecutable('du', { repoRoot });
const psBin = resolveSafeHostExecutable('ps', { repoRoot });

const minimumFreeBytes = readLimit('MUTATION_MIN_FREE_GIB', 30, 'minimum') * GIB;
const maximumGeneratedBytes = readLimit('MUTATION_MAX_GENERATED_GIB', 2, 'maximum') * GIB;
const maximumRssKiB = (readLimit('MUTATION_MAX_RSS_GIB', 8, 'maximum') * GIB) / 1024;
const pollMilliseconds = readLimit('MUTATION_MONITOR_SECONDS', 15, 'maximum') * 1000;
const heartbeatMilliseconds = readLimit('MUTATION_HEARTBEAT_MINUTES', 5, 'maximum') * 60_000;

function readLimit(name, fallback, direction) {
  const raw = process.env[name];
  if (raw === undefined) return fallback;
  const value = Number(raw);
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`${name} must be a positive number, got ${JSON.stringify(raw)}`);
  }
  if (direction === 'minimum' && value < fallback) {
    throw new Error(`${name} cannot lower the safety floor below ${fallback}`);
  }
  if (direction === 'maximum' && value > fallback) {
    throw new Error(`${name} cannot raise the safety ceiling above ${fallback}`);
  }
  return value;
}

function freeBytes() {
  const stats = statfsSync(repoRoot);
  return stats.bavail * stats.bsize;
}

function directoryBytes(path) {
  if (!existsSync(path)) return 0;
  const output = execFileSync(duBin, ['-sk', path], { encoding: 'utf8' }).trim();
  const kibibytes = Number.parseInt(output.split(/\s+/u)[0], 10);
  if (!Number.isFinite(kibibytes)) throw new Error(`could not parse du output for ${path}`);
  return kibibytes * 1024;
}

function processGroupRssKiB(processGroupId) {
  return processTable()
    .filter(({ pgid }) => pgid === processGroupId)
    .reduce((total, { rssKiB }) => total + rssKiB, 0);
}

function gibibytes(bytes) {
  return (bytes / GIB).toFixed(2);
}

function mutationInputBytes(inputs) {
  return Object.keys(inputs.files).reduce(
    (total, fileName) => total + statSync(join(repoRoot, fileName)).size,
    0,
  );
}

function cleanLegacyTempDir() {
  const resolvedTempDir = resolve(legacyTempDir);
  if (resolvedTempDir !== join(repoRoot, '.stryker-tmp')) {
    throw new Error(`refusing to clean unexpected temp path: ${resolvedTempDir}`);
  }
  try {
    if (lstatSync(resolvedTempDir).isSymbolicLink()) {
      unlinkSync(resolvedTempDir);
      return;
    }
  } catch (error) {
    if (error.code === 'ENOENT') return;
    throw error;
  }
  rmSync(resolvedTempDir, { recursive: true, force: true });
}

function cleanMutationWorkspace() {
  const failures = [];
  for (const cleanup of [() => removeMutationInputStage(repoRoot), () => cleanLegacyTempDir()]) {
    try {
      cleanup();
    } catch (error) {
      failures.push(error);
    }
  }
  if (failures.length > 0) {
    throw new Error(failures.map((error) => error.message).join('; '), {
      cause: failures[0],
    });
  }
}

function processExists(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    if (error.code === 'ESRCH') return false;
    throw error;
  }
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

function processTable() {
  const output = execFileSync(psBin, ['-axo', 'pgid=,pid=,rss=,command='], {
    encoding: 'utf8',
  });
  if (output.trim().length === 0) return [];
  return output
    .trimEnd()
    .split('\n')
    .map((line) => /^\s*(\d+)\s+(\d+)\s+(\d+)\s+(.*)$/u.exec(line))
    .filter((match) => match !== null)
    .map((match) => ({
      command: match[4].trim(),
      pgid: Number(match[1]),
      pid: Number(match[2]),
      rssKiB: Number(match[3]),
    }));
}

function processGroupProcesses(processGroupId) {
  return processTable().filter(({ pgid }) => pgid === processGroupId);
}

function currentProcessGroupId() {
  const current = processTable().find(({ pid }) => pid === process.pid);
  if (!current || !Number.isInteger(current.pgid) || current.pgid <= 0) {
    throw new Error('could not determine launcher PGID from ps output');
  }
  return current.pgid;
}

function probeHostUtilities() {
  // These options are shared by the BSD ps/du on macOS and procps/coreutils on
  // Linux. Probe them before publishing a lock or invalidating a full report.
  directoryBytes(sourceMutationChild);
  currentProcessGroupId();
}

const waitArray = new Int32Array(new SharedArrayBuffer(4));
function waitForProcessGroupExit(processGroupId, timeoutMilliseconds) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (processGroupExists(processGroupId)) {
    if (Date.now() >= deadline) return false;
    Atomics.wait(waitArray, 0, 0, Math.min(100, deadline - Date.now()));
  }
  return true;
}

async function waitForProcessGroupExitAsync(processGroupId, timeoutMilliseconds) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (processGroupExists(processGroupId)) {
    if (Date.now() >= deadline) return false;
    await new Promise((resolveWait) => setTimeout(resolveWait, 50));
  }
  return true;
}

function reclaimOrphanedProcessGroup(record) {
  if (!Number.isInteger(record.pgid) || record.pgid <= 0) {
    const identity =
      (record.version === 2 || record.version === 3) &&
      record.pgid === null &&
      typeof record.marker === 'string' &&
      /^readout-mutation-[a-f0-9-]{36}$/u.test(record.marker)
        ? `marker ${record.marker}`
        : 'an unverifiable identity';
    throw new Error(
      `stale mutation lock has no persisted child PGID (${identity}); ` +
        'refusing automatic cleanup',
    );
  }
  if (!processGroupExists(record.pgid)) return;
  if (record.pgid <= 1 || record.pgid === currentProcessGroupId()) {
    throw new Error(`refusing to signal unsafe stale process group ${record.pgid}`);
  }
  if (
    typeof record.marker !== 'string' ||
    !/^readout-mutation-[a-f0-9-]{36}$/u.test(record.marker)
  ) {
    throw new Error(`stale mutation lock has no verifiable identity for PGID ${record.pgid}`);
  }
  const processes = processGroupProcesses(record.pgid);
  const verifiedLeader = processes.some(
    ({ command, pgid, pid }) => pid === pgid && pid === record.pgid && command === record.marker,
  );
  if (!verifiedLeader) {
    throw new Error(`refusing to signal unverified stale process group ${record.pgid}`);
  }

  console.warn(`[mutation-monitor] reclaiming orphaned mutation process group ${record.pgid}`);
  try {
    process.kill(-record.pgid, 'SIGTERM');
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
  }
  if (waitForProcessGroupExit(record.pgid, 5_000)) return;
  try {
    process.kill(-record.pgid, 'SIGKILL');
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
  }
  if (!waitForProcessGroupExit(record.pgid, 10_000)) {
    throw new Error(`orphaned mutation process group ${record.pgid} did not exit after SIGKILL`);
  }
}

function persistChildProcessGroup(token, processGroupId) {
  updateOwnedMutationLock(lockPath, token, (record) => ({ ...record, pgid: processGroupId }));
}

function acquireLock() {
  const token = randomUUID();
  const record = {
    version: 3,
    pid: process.pid,
    pgid: null,
    marker: childMarker,
    mode,
    runId,
    startedAt: new Date().toISOString(),
    token,
  };
  return acquireMutationLock({
    isOwnerAlive: processExists,
    lockPath,
    reclaim: reclaimOrphanedProcessGroup,
    record,
    recoveryPath,
  });
}

if (!existsSync(installedStrykerBin)) {
  throw new Error('Stryker is not installed; run npm ci first');
}
probeHostUtilities();
ensureMutationArtifactDirectory(repoRoot);

const lockToken = acquireLock();
let lockReleased = false;
let retainLock = false;
function releaseOwnedLock() {
  if (lockReleased) return;
  releaseOwnedMutationLock(lockPath, lockToken);
  lockReleased = true;
}

const startedAt = Date.now();
const startedAtIso = new Date(startedAt).toISOString();
let startingFreeBytes;
let startingInputs;
let stagedInputs;
let logStream;
try {
  // A full attempt cannot inherit a prior report or proof. Dry runs use only a
  // text reporter and intentionally leave the last full result untouched.
  if (fullRun) invalidateFullMutationReport(repoRoot);
  assertMutationScopeMatchesRuntimeGraph(repoRoot);
  startingInputs = snapshotMutationInputs(repoRoot);
  // acquireLock() has already waited for any verified orphaned process group,
  // so it is now safe to remove the deterministic stage and legacy sandbox.
  cleanMutationWorkspace();
  const preStageFreeBytes = freeBytes();
  const existingGeneratedBytes = directoryBytes(reportDir);
  const stagedSourceBytes = mutationInputBytes(startingInputs);
  if (preStageFreeBytes - stagedSourceBytes < minimumFreeBytes) {
    throw new Error(
      `mutation run refused: staging ${gibibytes(stagedSourceBytes)} GiB from ` +
        `${gibibytes(preStageFreeBytes)} GiB free would cross the ` +
        `${gibibytes(minimumFreeBytes)} GiB minimum`,
    );
  }
  if (existingGeneratedBytes + stagedSourceBytes > maximumGeneratedBytes) {
    throw new Error(
      `mutation run refused: reports plus staged inputs would use at least ` +
        `${gibibytes(existingGeneratedBytes + stagedSourceBytes)} GiB, ` +
        `maximum is ${gibibytes(maximumGeneratedBytes)} GiB`,
    );
  }
  ({ inputs: stagedInputs } = createMutationInputStage(repoRoot, startingInputs));
  assertMutationScopeMatchesRuntimeGraph(stageRoot);
  startingFreeBytes = freeBytes();
  const stagedGeneratedBytes = directoryBytes(stageRoot) + directoryBytes(reportDir);
  if (startingFreeBytes < minimumFreeBytes) {
    throw new Error(
      `mutation run refused after staging: ${gibibytes(startingFreeBytes)} GiB free, ` +
        `minimum is ${gibibytes(minimumFreeBytes)} GiB`,
    );
  }
  if (stagedGeneratedBytes > maximumGeneratedBytes) {
    throw new Error(
      `mutation run refused after staging: mutation data is ` +
        `${gibibytes(stagedGeneratedBytes)} GiB, ` +
        `maximum is ${gibibytes(maximumGeneratedBytes)} GiB`,
    );
  }
  const { descriptor: logDescriptor } = openMutationLogFile(repoRoot);
  logStream = createWriteStream(logPath, { autoClose: true, fd: logDescriptor });
} catch (error) {
  try {
    cleanMutationWorkspace();
  } catch (cleanupFailure) {
    console.error(`[mutation-monitor] initialization cleanup failed: ${cleanupFailure.message}`);
  }
  retainLock = fullRun && existsSync(attestationPath);
  if (retainLock) {
    console.error(
      `[mutation-monitor] old attestation could not be invalidated; retaining ${lockPath}`,
    );
  } else {
    releaseOwnedLock();
  }
  throw error;
}

console.log(
  `[mutation-monitor] ${mode} start (concurrency=${concurrency}): ` +
    `${gibibytes(startingFreeBytes)} GiB free; ` +
    `limits generated=${gibibytes(maximumGeneratedBytes)} GiB, ` +
    `rss=${gibibytes(maximumRssKiB * 1024)} GiB, ` +
    `deadline=none; log=${logPath}`,
);

const child = spawn(
  process.execPath,
  [`--title=${childMarker}`, mutationChild, compatibilityHook, strykerBin, ...strykerArgs],
  {
    cwd: stageRoot,
    detached: true,
    env: {
      ...process.env,
      [guardEnvironment]: '1',
      [modeEnvironment]: mode,
      [runIdEnvironment]: runId,
    },
    stdio: ['ignore', 'pipe', 'pipe', 'ipc'],
  },
);
child.stdout.pipe(logStream, { end: false });
child.stderr.pipe(logStream, { end: false });

const processGroupId = child.pid;
if (!processGroupId) {
  logStream.destroy();
  let startupCleanupError;
  try {
    cleanMutationWorkspace();
  } catch (error) {
    startupCleanupError = error;
  }
  try {
    releaseOwnedLock();
  } catch (error) {
    startupCleanupError ??= error;
  }
  if (startupCleanupError) throw startupCleanupError;
  throw new Error('could not obtain the mutation child process-group ID');
}
try {
  persistChildProcessGroup(lockToken, processGroupId);
} catch (error) {
  let childStopped = false;
  let cleanupError;
  try {
    if (child.connected) child.disconnect();
  } catch (disconnectError) {
    cleanupError = disconnectError;
  }
  try {
    process.kill(-processGroupId, 'SIGKILL');
  } catch (killError) {
    if (killError.code !== 'ESRCH') cleanupError ??= killError;
  }
  try {
    // Yield to libuv while waiting so it can reap this direct child. A blocking
    // poll can leave a killed child visible as a zombie and falsely claim that
    // the group is still live.
    childStopped = await waitForProcessGroupExitAsync(processGroupId, 5_000);
  } catch (waitError) {
    cleanupError ??= waitError;
  }
  logStream.destroy();
  if (childStopped) {
    try {
      cleanMutationWorkspace();
    } catch (stageCleanupError) {
      cleanupError ??= stageCleanupError;
    }
    releaseOwnedLock();
  } else {
    // The child never receives the start handshake before its PGID is durably
    // recorded. If synchronous cleanup cannot prove that paused group is gone,
    // retain the lock. A persisted PGID can be verified by the next launcher;
    // a pre-persistence lock deliberately fails closed for manual inspection.
    const cleanupDetail = cleanupError ? ` (${cleanupError.message})` : '';
    throw new Error(
      `mutation child startup failed and process group ${processGroupId} could not be ` +
        `confirmed stopped; recovery lock retained at ${lockPath}${cleanupDetail}`,
      { cause: error },
    );
  }
  throw error;
}
let childResult;
let stopping = false;
let killTimer;
let latestSample;
let finalized = false;
let finalizing = false;
let childClosed = false;
let cleanupError;

function signalProcessGroup(signal) {
  if (!processGroupId) return;
  try {
    process.kill(-processGroupId, signal);
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
  }
}

function stopProcessGroup(reason) {
  if (stopping) return;
  stopping = true;
  console.error(`[mutation-monitor] stopping: ${reason}`);
  signalProcessGroup('SIGTERM');
  killTimer = setTimeout(() => signalProcessGroup('SIGKILL'), 15_000);
}

function sampleResources() {
  const free = freeBytes();
  const generated = directoryBytes(stageRoot) + directoryBytes(reportDir);
  const rssKiB = processGroupId ? processGroupRssKiB(processGroupId) : 0;
  latestSample = { free, generated, rssKiB };
  return latestSample;
}

function inspectResources() {
  try {
    const sample = sampleResources();
    if (sample.free < minimumFreeBytes) {
      stopProcessGroup(`free disk fell to ${gibibytes(sample.free)} GiB`);
    } else if (sample.generated > maximumGeneratedBytes) {
      stopProcessGroup(`mutation data grew to ${gibibytes(sample.generated)} GiB`);
    } else if (sample.rssKiB > maximumRssKiB) {
      stopProcessGroup(`mutation processes reached ${gibibytes(sample.rssKiB * 1024)} GiB RSS`);
    }
  } catch (error) {
    stopProcessGroup(`resource inspection failed: ${error.message}`);
  }
}

function heartbeat() {
  try {
    const sample = latestSample ?? sampleResources();
    const elapsedMinutes = Math.floor((Date.now() - startedAt) / 60_000);
    console.log(
      `[mutation-monitor] running ${elapsedMinutes}m: free=${gibibytes(sample.free)} GiB, ` +
        `generated=${gibibytes(sample.generated)} GiB, ` +
        `rss=${gibibytes(sample.rssKiB * 1024)} GiB`,
    );
  } catch (error) {
    stopProcessGroup(`heartbeat inspection failed: ${error.message}`);
  }
}

const monitor = setInterval(inspectResources, pollMilliseconds);
const heartbeatTimer = setInterval(heartbeat, heartbeatMilliseconds);

function finalizeWhenProcessGroupStops() {
  if (finalized || finalizing || childResult === undefined || !childClosed) return;
  if (processGroupId && processGroupExists(processGroupId)) return;
  finalizing = true;
  clearInterval(monitor);
  clearInterval(heartbeatTimer);
  clearInterval(groupWatcher);
  if (killTimer) clearTimeout(killTimer);

  const complete = () => {
    if (finalized) return;
    if (killTimer) clearTimeout(killTimer);
    let finalizationFailed = cleanupError !== undefined;
    let finalFreeText = 'unknown';
    try {
      const finalSample = sampleResources();
      finalFreeText = `${gibibytes(finalSample.free)} GiB`;
      if (finalSample.free < minimumFreeBytes) {
        throw new Error(`free disk fell to ${gibibytes(finalSample.free)} GiB before publication`);
      }
      if (finalSample.generated > maximumGeneratedBytes) {
        throw new Error(
          `mutation data grew to ${gibibytes(finalSample.generated)} GiB before publication`,
        );
      }
    } catch (error) {
      finalizationFailed = true;
      console.error(`[mutation-monitor] final resource inspection failed: ${error.message}`);
    }
    let currentInputs;
    let finalStagedInputs;
    let reportBytes;
    let htmlReportBytes;
    let attestationWritten = false;
    const childSucceeded = !stopping && !childResult.signal && childResult.code === 0;
    if (childSucceeded) {
      try {
        finalStagedInputs = snapshotMutationInputs(stageRoot);
        assertMutationInputsEqual(
          stagedInputs,
          finalStagedInputs,
          'staged mutation inputs during run',
        );
        if (fullRun) {
          currentInputs = snapshotMutationInputs(repoRoot);
          assertMutationInputsEqual(
            startingInputs,
            currentInputs,
            'working-tree mutation inputs during full run',
          );
          reportBytes = readFileSync(stagedReportPath);
          htmlReportBytes = readFileSync(stagedHtmlReportPath);
        }
      } catch (error) {
        finalizationFailed = true;
        console.error(`[mutation-monitor] staged result validation failed: ${error.message}`);
      }
    }

    try {
      // The process-group check above is the authorization boundary for both
      // successful cleanup and crash-recovery cleanup on the next launcher.
      cleanMutationWorkspace();
    } catch (error) {
      finalizationFailed = true;
      console.error(`[mutation-monitor] stage cleanup failed: ${error.message}`);
    }

    if (fullRun && childSucceeded && !finalizationFailed) {
      try {
        publishFullMutationReports(repoRoot, { htmlReportBytes, reportBytes });
        const completedAt = new Date().toISOString();
        writeFullRunAttestation(
          repoRoot,
          createFullRunAttestation({
            completedAt,
            executionInputs: finalStagedInputs,
            htmlReportBytes,
            inputs: currentInputs,
            repoRoot,
            reportBytes,
            runId,
            startedAt: startedAtIso,
          }),
        );
        attestationWritten = true;
      } catch (error) {
        finalizationFailed = true;
        console.error(`[mutation-monitor] full-run publication failed: ${error.message}`);
      }
    }
    if (fullRun && (!childSucceeded || finalizationFailed || !attestationWritten)) {
      try {
        invalidateFullMutationReport(repoRoot);
      } catch (error) {
        finalizationFailed = true;
        retainLock = existsSync(attestationPath);
        console.error(`[mutation-monitor] full-run artifact invalidation failed: ${error.message}`);
      }
    }
    if (!retainLock) {
      try {
        releaseOwnedLock();
      } catch (error) {
        finalizationFailed = true;
        console.error(`[mutation-monitor] lock release failed: ${error.message}`);
        if (fullRun) {
          try {
            invalidateFullMutationReport(repoRoot);
          } catch (invalidationError) {
            retainLock = existsSync(attestationPath);
            console.error(
              `[mutation-monitor] post-release-failure invalidation failed: ` +
                invalidationError.message,
            );
          }
        }
      }
    } else {
      console.error(`[mutation-monitor] retaining recovery lock at ${lockPath}`);
    }
    const elapsedSeconds = Math.round((Date.now() - startedAt) / 1000);
    finalized = true;
    console.log(
      `[mutation-monitor] end after ${elapsedSeconds}s: ${finalFreeText} free; ` + `log=${logPath}`,
    );
    if (stopping) process.exitCode = 75;
    else if (finalizationFailed || childResult.signal) process.exitCode = 1;
    else process.exitCode = childResult.code ?? 1;
  };

  if (logStream.destroyed || logStream.closed) {
    complete();
  } else {
    logStream.once('close', complete);
    logStream.end();
  }
}

const groupWatcher = setInterval(finalizeWhenProcessGroupStops, 250);

for (const signal of ['SIGHUP', 'SIGINT', 'SIGTERM']) {
  process.on(signal, () => stopProcessGroup(`launcher received ${signal}`));
}

logStream.on('error', (error) => {
  cleanupError ??= error;
  if (!finalizing) stopProcessGroup(`log write failed: ${error.message}`);
});

child.on('error', (error) => {
  childResult = { code: 1, signal: undefined };
  stopProcessGroup(`could not start Stryker: ${error.message}`);
  finalizeWhenProcessGroupStops();
});

child.on('message', (message) => {
  const validCode =
    message?.code === null || (Number.isInteger(message?.code) && message.code >= 0);
  const validSignal = message?.signal === null || typeof message?.signal === 'string';
  if (
    message?.type !== 'result' ||
    !validCode ||
    !validSignal ||
    (message.code === null && message.signal === null) ||
    childResult !== undefined
  ) {
    stopProcessGroup('mutation child sent an invalid or duplicate result');
    return;
  }
  childResult = { code: message.code, signal: message.signal ?? undefined };

  try {
    const groupProcesses = processGroupProcesses(processGroupId);
    const verifiedLeader = groupProcesses.some(
      ({ command, pgid, pid }) =>
        pid === processGroupId && pgid === processGroupId && command === childMarker,
    );
    const descendants = groupProcesses.filter(({ pid }) => pid !== processGroupId);
    if (!verifiedLeader || descendants.length > 0) {
      stopProcessGroup(
        `workload exited with ${descendants.length} descendant process(es) still in its group`,
      );
      return;
    }
    child.send({ type: 'release' }, (error) => {
      if (error) stopProcessGroup(`could not release mutation child: ${error.message}`);
    });
  } catch (error) {
    stopProcessGroup(`could not verify completed mutation process group: ${error.message}`);
  }
});

child.on('exit', (code, signal) => {
  childResult = { code, signal };
  if (processGroupId && processGroupExists(processGroupId)) {
    stopProcessGroup('Stryker leader exited while worker processes were still running');
  }
  finalizeWhenProcessGroupStops();
});

child.on('close', (code, signal) => {
  childClosed = true;
  childResult ??= { code, signal };
  finalizeWhenProcessGroupStops();
});

process.on('exit', () => {
  if (!finalized) {
    try {
      signalProcessGroup('SIGKILL');
    } catch {
      // A persisted PGID can be verified by the next launcher. If persistence
      // itself failed, the marker-bearing lock intentionally remains fail-closed.
    }
    // Do not remove the lock or sandbox while a group may still be alive. This
    // also makes an uncatchable SIGKILL recoverable by the next launcher.
  } else if (!lockReleased && !retainLock) {
    try {
      releaseOwnedLock();
    } catch {
      // The next launcher can reclaim the dead owner's lock.
    }
  }
});

try {
  child.send({ type: 'start', processGroupId }, (error) => {
    if (error) stopProcessGroup(`could not send mutation start handshake: ${error.message}`);
  });
} catch (error) {
  stopProcessGroup(`could not send mutation start handshake: ${error.message}`);
}
