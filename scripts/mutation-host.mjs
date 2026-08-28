import { accessSync, constants, realpathSync, statSync } from 'node:fs';
import { delimiter, isAbsolute, join, relative, resolve, sep } from 'node:path';

function isInside(root, candidate) {
  const pathFromRoot = relative(root, candidate);
  return (
    pathFromRoot === '' ||
    (pathFromRoot !== '..' && !pathFromRoot.startsWith(`..${sep}`) && !isAbsolute(pathFromRoot))
  );
}

export function resolveSafeHostExecutable(name, { repoRoot, searchPath = process.env.PATH } = {}) {
  if (typeof searchPath !== 'string' || searchPath.length === 0) {
    throw new Error(`cannot locate required host executable ${name}: PATH is empty`);
  }
  const resolvedRepoRoot = resolve(repoRoot);
  const canonicalRepoRoot = realpathSync(resolvedRepoRoot);

  for (const entry of searchPath.split(delimiter)) {
    // Empty and relative entries resolve through the current directory, while
    // repository descendants can contain attacker-controlled lookalikes. They
    // are never eligible sources for resource-inspection commands.
    if (entry.length === 0 || !isAbsolute(entry)) continue;
    const directory = resolve(entry);
    if (isInside(resolvedRepoRoot, directory)) continue;

    let canonicalDirectory;
    try {
      canonicalDirectory = realpathSync(directory);
      if (!statSync(canonicalDirectory).isDirectory()) continue;
    } catch {
      continue;
    }
    if (isInside(canonicalRepoRoot, canonicalDirectory)) continue;

    const candidate = join(canonicalDirectory, name);
    try {
      accessSync(candidate, constants.X_OK);
      if (!statSync(candidate).isFile()) continue;
      const canonicalCandidate = realpathSync(candidate);
      if (!isInside(canonicalRepoRoot, canonicalCandidate)) return canonicalCandidate;
    } catch {
      // Continue through the safe absolute PATH entries.
    }
  }
  throw new Error(`cannot locate a safe executable ${name} in PATH`);
}
