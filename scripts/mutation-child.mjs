// Keep a uniquely titled process-group leader alive for the entire Stryker run.
// The two-phase IPC handshake prevents both unpersisted starts and successful
// workload exit from being mistaken for permission to outlive the launcher.

import { spawn } from 'node:child_process';

const [compatibilityHook, strykerBin, ...strykerArgs] = process.argv.slice(2);
if (!compatibilityHook || !strykerBin || typeof process.send !== 'function') {
  throw new Error('mutation child must be started by the guarded launcher');
}

let state = 'awaiting-start';
let workloadResult;
let releaseTimeout;

function terminateOwnProcessGroup(reason) {
  if (state === 'released' || state === 'stopping') return;
  state = 'stopping';
  clearTimeout(startTimeout);
  if (releaseTimeout) clearTimeout(releaseTimeout);
  process.exitCode = 1;
  console.error(`[mutation-child] ${reason}; terminating unmonitored process group`);

  // The launcher creates this process as a detached group leader, so pid is
  // also the PGID. Keep this leader alive through SIGTERM, then include it in a
  // forced group kill so descendants cannot outlive the resource monitor.
  try {
    process.kill(-process.pid, 'SIGTERM');
  } catch (error) {
    if (error.code !== 'ESRCH') {
      console.error(`[mutation-child] process-group SIGTERM failed: ${error.message}`);
    }
  }
  setTimeout(() => {
    try {
      process.kill(-process.pid, 'SIGKILL');
    } catch (error) {
      if (error.code !== 'ESRCH') {
        console.error(`[mutation-child] process-group SIGKILL failed: ${error.message}`);
      }
      process.exit(1);
    }
  }, 1_000);
}

function reportWorkloadResult(code, signal) {
  if (state !== 'running') return;
  state = 'awaiting-release';
  workloadResult = { code, signal: signal ?? null };
  releaseTimeout = setTimeout(
    () => terminateOwnProcessGroup('launcher did not release the completed process group'),
    30_000,
  );
  try {
    process.send({ type: 'result', ...workloadResult }, (error) => {
      if (error) terminateOwnProcessGroup(`could not report workload result: ${error.message}`);
    });
  } catch (error) {
    terminateOwnProcessGroup(`could not report workload result: ${error.message}`);
  }
}

const startTimeout = setTimeout(
  () => terminateOwnProcessGroup('launcher did not complete the start handshake'),
  30_000,
);

process.on('disconnect', () => {
  if (state !== 'released') terminateOwnProcessGroup('launcher IPC disconnected unexpectedly');
});

for (const signal of ['SIGHUP', 'SIGINT', 'SIGTERM']) {
  process.on(signal, () => terminateOwnProcessGroup(`received ${signal}`));
}

process.on('message', (message) => {
  if (state === 'awaiting-start') {
    if (message?.type !== 'start' || message.processGroupId !== process.pid) {
      terminateOwnProcessGroup('received an invalid start handshake');
      return;
    }
    state = 'running';
    clearTimeout(startTimeout);

    const stryker = spawn(
      process.execPath,
      ['--import', compatibilityHook, strykerBin, 'run', ...strykerArgs],
      {
        env: process.env,
        stdio: 'inherit',
      },
    );
    stryker.once('error', (error) => {
      console.error(`[mutation-child] could not start Stryker: ${error.message}`);
      reportWorkloadResult(1, null);
    });
    stryker.once('close', (code, signal) => reportWorkloadResult(code, signal));
    return;
  }

  if (state === 'awaiting-release' && message?.type === 'release') {
    state = 'released';
    clearTimeout(releaseTimeout);
    process.exitCode = workloadResult.signal ? 1 : (workloadResult.code ?? 1);
    if (process.connected) process.disconnect();
    return;
  }

  terminateOwnProcessGroup('received an invalid IPC state transition');
});
