import { randomUUID } from 'node:crypto';
import {
  existsSync,
  linkSync,
  mkdirSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import { dirname } from 'node:path';

export function parseMutationLockRecord(content) {
  try {
    const parsed = JSON.parse(content);
    if (parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed)) return parsed;
  } catch {
    const legacyOwner = Number.parseInt(content.trim(), 10);
    if (Number.isInteger(legacyOwner) && legacyOwner > 0) {
      return { version: 1, pid: legacyOwner };
    }
  }
  return {};
}

function sameFile(leftPath, rightPath) {
  const left = statSync(leftPath, { bigint: true });
  const right = statSync(rightPath, { bigint: true });
  return left.dev === right.dev && left.ino === right.ino;
}

function readLock(path) {
  return parseMutationLockRecord(readFileSync(path, 'utf8'));
}

function writeCandidate(path, record) {
  writeFileSync(path, `${JSON.stringify(record)}\n`, {
    encoding: 'utf8',
    flag: 'wx',
    mode: 0o600,
  });
}

export function acquireMutationLock({
  afterStaleUnlinked = () => {},
  isOwnerAlive,
  lockPath,
  record,
  reclaim,
  recoveryPath,
}) {
  if (typeof record?.token !== 'string' || record.token.length < 16) {
    throw new Error('new mutation lock record must have a strong ownership token');
  }
  mkdirSync(dirname(lockPath), { recursive: true });
  const candidatePath = `${lockPath}.${process.pid}.${randomUUID()}.candidate`;
  writeCandidate(candidatePath, record);
  try {
    for (let attempt = 0; attempt < 4; attempt += 1) {
      // A recovery hard link is a filesystem-level claim on the stale lock's
      // inode. Never publish a new owner while a prior recovery is incomplete.
      if (existsSync(recoveryPath)) {
        throw new Error(
          `mutation lock recovery is in progress or abandoned at ${recoveryPath}; ` +
            'refusing to start',
        );
      }

      try {
        linkSync(candidatePath, lockPath);
        return record.token;
      } catch (error) {
        if (error.code !== 'EEXIST') throw error;
      }

      try {
        // The fixed destination makes recovery ownership atomic: exactly one
        // contender can hard-link the currently published lock inode here.
        linkSync(lockPath, recoveryPath);
      } catch (error) {
        if (error.code === 'ENOENT') continue;
        if (error.code === 'EEXIST') {
          throw new Error(`another launcher owns mutation lock recovery at ${recoveryPath}`);
        }
        throw error;
      }

      try {
        // Read only the claimed inode. The lock may have changed between the
        // failed publication and recovery claim, so earlier observations are
        // deliberately ignored.
        const claimedRecord = readLock(recoveryPath);
        const owner =
          Number.isInteger(claimedRecord.pid) && claimedRecord.pid > 0
            ? claimedRecord.pid
            : undefined;
        if (owner && isOwnerAlive(owner)) {
          throw new Error(`another mutation guard is already running (pid ${owner})`);
        }

        reclaim(claimedRecord);
        if (!sameFile(lockPath, recoveryPath)) {
          throw new Error('mutation lock changed during stale recovery; refusing to unlink it');
        }
        rmSync(lockPath);
        afterStaleUnlinked();

        try {
          // Publish the new owner before releasing the recovery claim. A
          // contender that raced an earlier claim check may win this link, but
          // this launcher will never delete or overlap that new owner.
          linkSync(candidatePath, lockPath);
          return record.token;
        } catch (error) {
          if (error.code !== 'EEXIST') throw error;
          throw new Error('another launcher acquired the mutation lock during recovery');
        }
      } finally {
        rmSync(recoveryPath, { force: true });
      }
    }
  } finally {
    rmSync(candidatePath, { force: true });
  }
  throw new Error('could not acquire mutation guard lock');
}

export function updateOwnedMutationLock(lockPath, token, transform) {
  const current = readLock(lockPath);
  if (current.token !== token || current.pid !== process.pid) {
    throw new Error('mutation guard lock ownership changed unexpectedly');
  }
  const candidatePath = `${lockPath}.${process.pid}.${randomUUID()}.update`;
  writeCandidate(candidatePath, transform(current));
  try {
    renameSync(candidatePath, lockPath);
  } finally {
    rmSync(candidatePath, { force: true });
  }
}

export function releaseOwnedMutationLock(lockPath, token) {
  let current;
  try {
    current = readLock(lockPath);
  } catch (error) {
    if (error.code === 'ENOENT') return;
    throw error;
  }
  if (current.token === token) rmSync(lockPath);
}
