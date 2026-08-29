// Runtime-vendored JavaScript provenance primitives. The offline path validates
// the exact committed set and local SHA-256 values. The explicit upstream path
// additionally downloads pinned npm tarballs, verifies sha512 SRI, and reads the
// declared files from tar entirely in memory -- no shell extraction or temp tree.

import { Buffer } from 'node:buffer';
import { createHash, timingSafeEqual } from 'node:crypto';
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gunzipSync } from 'node:zlib';

export const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..');
const manifestRelativePath = 'internal/assets/vendor-manifest.json';
const scope = 'runtime-vendored-javascript';
const sha256Pattern = /^[0-9a-f]{64}$/u;
const versionPattern = /^(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/u;
const tarBlockSize = 512;
const maxTarballBytes = 8 * 1024 * 1024;
const maxTarBytes = 32 * 1024 * 1024;
const maxTarEntries = 10_000;
const downloadTimeoutMilliseconds = 30_000;
const staticDirectory = 'internal/assets/static';
const firstPartyRuntimeJavaScript = `${staticDirectory}/readout.js`;

const manifestKeys = ['artifacts', 'schemaVersion', 'scope'];
const artifactKeys = [
  'artifactPath',
  'artifactSHA256',
  'license',
  'licensePackagePath',
  'licensePath',
  'licenseSHA256',
  'name',
  'packagePath',
  'sourceIntegrity',
  'sourceURL',
  'version',
];

// This code-owned set is the trust boundary: changing the manifest alone cannot
// silently omit a runtime vendor or reintroduce the removed preload extension.
const expectedRuntimeVendors = [
  {
    name: 'htmx.org',
    artifactPath: 'internal/assets/static/htmx.min.js',
    packagePath: 'package/dist/htmx.min.js',
    license: '0BSD',
    licensePackagePath: 'package/LICENSE',
    licensePath: 'internal/assets/static/LICENSES/htmx.txt',
  },
  {
    name: 'idiomorph',
    artifactPath: 'internal/assets/static/idiomorph-ext.min.js',
    packagePath: 'package/dist/idiomorph-ext.min.js',
    license: '0BSD',
    licensePackagePath: 'package/LICENSE',
    licensePath: 'internal/assets/static/LICENSES/idiomorph.txt',
  },
];

function fail(message) {
  throw new Error(`vendor asset verification failed: ${message}`);
}

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function requireString(value, field) {
  if (typeof value !== 'string' || value.length === 0) {
    fail(`${field} must be a non-empty string`);
  }
  return value;
}

function requireExactKeys(value, expected, field) {
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  if (actual.length !== wanted.length || actual.some((key, index) => key !== wanted[index])) {
    fail(`${field} keys are [${actual.join(', ')}], want [${wanted.join(', ')}]`);
  }
}

function requireSafePath(value, field) {
  const path = requireString(value, field);
  if (
    path.startsWith('/') ||
    path.endsWith('/') ||
    path.includes('\\') ||
    path.includes('\0') ||
    path.split('/').some((part) => part === '' || part === '.' || part === '..')
  ) {
    fail(`${field} must be a normalized relative path`);
  }
  return path;
}

function expectedTarballURL(name, version) {
  return `https://registry.npmjs.org/${name}/-/${name}-${version}.tgz`;
}

function parseSRI(value, field) {
  const sri = requireString(value, field);
  const match = /^sha512-([A-Za-z0-9+/]+={0,2})$/u.exec(sri);
  if (!match) fail(`${field} must be one canonical sha512 SRI value`);
  const digest = Buffer.from(match[1], 'base64');
  if (digest.length !== 64 || digest.toString('base64') !== match[1]) {
    fail(`${field} must encode exactly one SHA-512 digest`);
  }
  return digest;
}

function sha256(data) {
  return createHash('sha256').update(data).digest('hex');
}

function readRepoFile(root, relativePath, field) {
  requireSafePath(relativePath, field);
  try {
    return readFileSync(join(root, relativePath));
  } catch (error) {
    fail(`${field} cannot be read: ${error.message}`);
  }
}

export function validateVendorManifest(manifest) {
  if (!isObject(manifest)) fail('manifest must be an object');
  requireExactKeys(manifest, manifestKeys, 'manifest');
  if (manifest.schemaVersion !== 2) fail('manifest.schemaVersion must be 2');
  if (manifest.scope !== scope) fail(`manifest.scope must be ${scope}`);
  if (!Array.isArray(manifest.artifacts)) fail('manifest.artifacts must be an array');
  if (manifest.artifacts.length !== expectedRuntimeVendors.length) {
    fail(
      `manifest.artifacts must contain exactly ${expectedRuntimeVendors.length} runtime vendors`,
    );
  }

  const names = new Set();
  const artifactPaths = new Set();
  const licensePaths = new Set();
  for (const [index, artifact] of manifest.artifacts.entries()) {
    const prefix = `manifest.artifacts[${index}]`;
    if (!isObject(artifact)) fail(`${prefix} must be an object`);
    requireExactKeys(artifact, artifactKeys, prefix);

    const expected = expectedRuntimeVendors[index];
    const name = requireString(artifact.name, `${prefix}.name`);
    const version = requireString(artifact.version, `${prefix}.version`);
    const artifactPath = requireSafePath(artifact.artifactPath, `${prefix}.artifactPath`);
    requireSafePath(artifact.packagePath, `${prefix}.packagePath`);
    const licensePath = requireSafePath(artifact.licensePath, `${prefix}.licensePath`);
    requireSafePath(artifact.licensePackagePath, `${prefix}.licensePackagePath`);
    requireString(artifact.license, `${prefix}.license`);
    const sourceURL = requireString(artifact.sourceURL, `${prefix}.sourceURL`);
    parseSRI(artifact.sourceIntegrity, `${prefix}.sourceIntegrity`);

    if (!versionPattern.test(version)) fail(`${prefix}.version must be exact x.y.z`);
    if (!sha256Pattern.test(artifact.artifactSHA256)) {
      fail(`${prefix}.artifactSHA256 must be lowercase SHA-256`);
    }
    if (!sha256Pattern.test(artifact.licenseSHA256)) {
      fail(`${prefix}.licenseSHA256 must be lowercase SHA-256`);
    }
    if (sourceURL !== expectedTarballURL(name, version)) {
      fail(`${prefix}.sourceURL does not match its exact npm package version`);
    }

    for (const field of [
      'name',
      'artifactPath',
      'packagePath',
      'license',
      'licensePackagePath',
      'licensePath',
    ]) {
      if (artifact[field] !== expected[field]) {
        fail(`${prefix}.${field} is ${artifact[field]}, want ${expected[field]}`);
      }
    }
    if (names.has(name)) fail(`${prefix}.name duplicates ${name}`);
    if (artifactPaths.has(artifactPath)) {
      fail(`${prefix}.artifactPath duplicates ${artifactPath}`);
    }
    if (licensePaths.has(licensePath)) fail(`${prefix}.licensePath duplicates ${licensePath}`);
    names.add(name);
    artifactPaths.add(artifactPath);
    licensePaths.add(licensePath);
  }
  return manifest;
}

export function loadVendorManifest(root = repoRoot) {
  const manifestPath = join(root, manifestRelativePath);
  let manifest;
  try {
    manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
  } catch (error) {
    fail(`manifest cannot be parsed: ${error.message}`);
  }
  return validateVendorManifest(manifest);
}

export function verifyLocalVendorAssets(manifest, root = repoRoot) {
  validateVendorManifest(manifest);
  let staticJavaScriptPaths;
  try {
    staticJavaScriptPaths = readdirSync(join(root, staticDirectory), { withFileTypes: true })
      .filter((entry) => entry.name.endsWith('.js'))
      .map((entry) => `${staticDirectory}/${entry.name}`);
  } catch (error) {
    fail(`${staticDirectory} cannot be enumerated: ${error.message}`);
  }
  verifyVendoredJavaScriptSet(manifest, staticJavaScriptPaths);

  for (const [index, artifact] of manifest.artifacts.entries()) {
    const prefix = `manifest.artifacts[${index}]`;
    const asset = readRepoFile(root, artifact.artifactPath, `${prefix}.artifactPath`);
    const actualArtifactSHA256 = sha256(asset);
    if (actualArtifactSHA256 !== artifact.artifactSHA256) {
      fail(
        `${artifact.artifactPath} SHA-256 is ${actualArtifactSHA256}, want ${artifact.artifactSHA256}`,
      );
    }
    const license = readRepoFile(root, artifact.licensePath, `${prefix}.licensePath`);
    const actualLicenseSHA256 = sha256(license);
    if (actualLicenseSHA256 !== artifact.licenseSHA256) {
      fail(
        `${artifact.licensePath} SHA-256 is ${actualLicenseSHA256}, want ${artifact.licenseSHA256}`,
      );
    }
    if (
      artifact.name === 'htmx.org' &&
      !asset.includes(Buffer.from(`version:"${artifact.version}"`))
    ) {
      fail(`${artifact.artifactPath} does not identify itself as htmx ${artifact.version}`);
    }
  }
  return manifest.artifacts.length;
}

export function verifyVendoredJavaScriptSet(manifest, staticJavaScriptPaths) {
  validateVendorManifest(manifest);
  if (!Array.isArray(staticJavaScriptPaths)) {
    fail('static JavaScript paths must be an array');
  }

  const actualPaths = new Set();
  for (const [index, candidate] of staticJavaScriptPaths.entries()) {
    const path = requireSafePath(candidate, `static JavaScript paths[${index}]`);
    const name = path.slice(`${staticDirectory}/`.length);
    if (
      !path.startsWith(`${staticDirectory}/`) ||
      name.length === 0 ||
      name.includes('/') ||
      !name.endsWith('.js')
    ) {
      fail(`static JavaScript paths[${index}] must name a top-level ${staticDirectory}/*.js file`);
    }
    if (actualPaths.has(path)) fail(`static JavaScript paths duplicate ${path}`);
    actualPaths.add(path);
  }

  if (!actualPaths.delete(firstPartyRuntimeJavaScript)) {
    fail(`${firstPartyRuntimeJavaScript} must be present as the first-party runtime bundle`);
  }
  const actualVendors = [...actualPaths].sort();
  const expectedVendors = manifest.artifacts.map((artifact) => artifact.artifactPath).sort();
  if (
    actualVendors.length !== expectedVendors.length ||
    actualVendors.some((path, index) => path !== expectedVendors[index])
  ) {
    fail(
      `runtime-vendored JavaScript paths are [${actualVendors.join(', ')}], want [${expectedVendors.join(', ')}]`,
    );
  }
}

export function verifySourceIntegrity(data, sourceIntegrity, field = 'sourceIntegrity') {
  const expected = parseSRI(sourceIntegrity, field);
  const actual = createHash('sha512').update(data).digest();
  if (!timingSafeEqual(actual, expected)) fail(`${field} does not match downloaded bytes`);
}

function readTarString(header, start, length) {
  const field = header.subarray(start, start + length);
  const end = field.indexOf(0);
  return field.subarray(0, end === -1 ? field.length : end).toString('utf8');
}

function readTarOctal(header, start, length, field) {
  const raw = readTarString(header, start, length).trim();
  if (!/^[0-7]+$/u.test(raw)) fail(`tar ${field} is not canonical octal`);
  const value = Number.parseInt(raw, 8);
  if (!Number.isSafeInteger(value) || value < 0) fail(`tar ${field} is out of range`);
  return value;
}

function verifyTarHeaderChecksum(header) {
  const expected = readTarOctal(header, 148, 8, 'header checksum');
  let actual = 0;
  for (let index = 0; index < tarBlockSize; index += 1) {
    actual += index >= 148 && index < 156 ? 32 : (header[index] ?? 0);
  }
  if (actual !== expected) fail(`tar header checksum is ${actual}, want ${expected}`);
}

function verifyUstarHeader(header) {
  if (!header.subarray(257, 263).equals(Buffer.from('ustar\0'))) {
    fail('tar header does not use POSIX ustar magic');
  }
  if (!header.subarray(263, 265).equals(Buffer.from('00'))) {
    fail('tar header does not use POSIX ustar version 00');
  }
}

function isZeroTarBlock(block) {
  return block.every((byte) => byte === 0);
}

export function extractTarFiles(tar, wantedPaths) {
  if (!Buffer.isBuffer(tar)) fail('tar payload must be a Buffer');
  if (tar.length > maxTarBytes) fail(`tar payload exceeds ${maxTarBytes} bytes`);
  if (tar.length % tarBlockSize !== 0) fail('tar payload is not block-aligned');
  const wanted = new Set();
  for (const path of wantedPaths) wanted.add(requireSafePath(path, 'wanted tar path'));
  const found = new Map();
  const seen = new Set();
  let offset = 0;
  let entries = 0;
  let foundEndOfArchive = false;

  while (offset < tar.length) {
    const header = tar.subarray(offset, offset + tarBlockSize);
    if (isZeroTarBlock(header)) {
      const secondEndBlock = tar.subarray(offset + tarBlockSize, offset + tarBlockSize * 2);
      if (secondEndBlock.length !== tarBlockSize || !isZeroTarBlock(secondEndBlock)) {
        fail('tar archive does not end with two zero blocks');
      }
      if (!isZeroTarBlock(tar.subarray(offset + tarBlockSize * 2))) {
        fail('tar archive contains non-zero data after its end marker');
      }
      foundEndOfArchive = true;
      break;
    }
    entries += 1;
    if (entries > maxTarEntries) fail(`tar contains more than ${maxTarEntries} entries`);
    verifyTarHeaderChecksum(header);
    verifyUstarHeader(header);

    const type = header[156] ?? 0;
    if (type !== 0 && type !== 48 && type !== 53) {
      fail(`tar contains unsupported typeflag ${JSON.stringify(String.fromCharCode(type))}`);
    }

    const name = readTarString(header, 0, 100);
    const prefix = readTarString(header, 345, 155);
    const rawPath = prefix ? `${prefix}/${name}` : name;
    const path = requireSafePath(
      type === 53 && rawPath.endsWith('/') ? rawPath.slice(0, -1) : rawPath,
      'tar entry path',
    );
    if (seen.has(path)) fail(`tar contains duplicate entry ${path}`);
    seen.add(path);

    const size = readTarOctal(header, 124, 12, `size for ${path}`);
    if (type === 53 && size !== 0) fail(`tar directory ${path} has non-zero size`);
    const dataStart = offset + tarBlockSize;
    const dataEnd = dataStart + size;
    if (dataEnd > tar.length) fail(`tar entry ${path} exceeds the archive`);
    const nextOffset = dataStart + Math.ceil(size / tarBlockSize) * tarBlockSize;
    if (nextOffset > tar.length) fail(`tar entry ${path} has truncated block padding`);
    if ((type === 0 || type === 48) && wanted.has(path)) {
      found.set(path, Buffer.from(tar.subarray(dataStart, dataEnd)));
    }
    offset = nextOffset;
  }

  if (!foundEndOfArchive) fail('tar archive is missing its two zero end blocks');

  for (const path of wanted) {
    if (!found.has(path)) fail(`tar does not contain regular file ${path}`);
  }
  return found;
}

export async function downloadWithIntegrity(
  artifact,
  fetchImpl = globalThis.fetch,
  { maxBytes = maxTarballBytes, timeoutMilliseconds = downloadTimeoutMilliseconds } = {},
) {
  if (typeof fetchImpl !== 'function') fail('global fetch is unavailable');
  if (!Number.isSafeInteger(maxBytes) || maxBytes <= 0 || maxBytes > maxTarballBytes) {
    fail(`download maxBytes must be between 1 and ${maxTarballBytes}`);
  }
  if (!Number.isSafeInteger(timeoutMilliseconds) || timeoutMilliseconds <= 0) {
    fail('download timeoutMilliseconds must be a positive safe integer');
  }

  const controller = new AbortController();
  const timeoutError = new Error(
    `${artifact.name} tarball download timed out after ${timeoutMilliseconds}ms`,
  );
  let response;
  let reader;
  let readerReleased = false;
  let stopped = false;
  let timedOut = false;
  let rejectTimeout;
  const timeoutPromise = new Promise((_, reject) => {
    rejectTimeout = reject;
  });

  function stopDownload(reason) {
    if (stopped) return;
    stopped = true;
    controller.abort(reason);
    let cancellation;
    try {
      cancellation = reader ? reader.cancel(reason) : response?.body?.cancel(reason);
    } catch {
      return;
    }
    Promise.resolve(cancellation).catch(() => {});
  }

  function releaseReader() {
    if (!reader || readerReleased) return;
    readerReleased = true;
    try {
      reader.releaseLock?.();
    } catch {
      // Cleanup must not hide the verification result. A canceled native
      // reader releases once its pending read settles.
    }
  }

  const timeout = setTimeout(() => {
    timedOut = true;
    stopDownload(timeoutError);
    rejectTimeout(timeoutError);
  }, timeoutMilliseconds);

  async function waitFor(promise, stage) {
    try {
      const result = await Promise.race([promise, timeoutPromise]);
      if (timedOut) fail(timeoutError.message);
      return result;
    } catch (error) {
      stopDownload(error);
      if (timedOut || error === timeoutError) fail(timeoutError.message);
      fail(`${artifact.name} tarball ${stage} failed: ${error?.message ?? String(error)}`);
    }
  }

  try {
    const fetchPromise = Promise.resolve().then(() =>
      fetchImpl(artifact.sourceURL, {
        headers: { Accept: 'application/octet-stream' },
        redirect: 'error',
        signal: controller.signal,
      }),
    );
    response = await waitFor(fetchPromise, 'fetch');
    if (!response || typeof response !== 'object') {
      stopDownload(new Error('tarball fetch returned no Response'));
      fail(`${artifact.name} tarball fetch returned no Response`);
    }
    if (!response.ok) {
      stopDownload(new Error(`HTTP ${response.status}`));
      fail(`${artifact.name} tarball fetch returned HTTP ${response.status}`);
    }
    if (response.url && response.url !== artifact.sourceURL) {
      stopDownload(new Error(`redirected to ${response.url}`));
      fail(`${artifact.name} tarball redirected to ${response.url}`);
    }
    if (!response.headers || typeof response.headers.get !== 'function') {
      stopDownload(new Error('Response headers are unavailable'));
      fail(`${artifact.name} tarball response headers are unavailable`);
    }
    const rawDeclaredLength = response.headers.get('content-length');
    if (rawDeclaredLength !== null) {
      const normalizedLength =
        typeof rawDeclaredLength === 'string' ? rawDeclaredLength.trim() : '';
      if (!/^\d+$/u.test(normalizedLength)) {
        stopDownload(new Error('invalid Content-Length'));
        fail(`${artifact.name} tarball has invalid Content-Length`);
      }
      const declaredLength = BigInt(normalizedLength);
      if (declaredLength > BigInt(maxBytes)) {
        stopDownload(new Error(`exceeds ${maxBytes} bytes`));
        fail(`${artifact.name} tarball exceeds ${maxBytes} bytes`);
      }
    }
    if (!response.body || typeof response.body.getReader !== 'function') {
      stopDownload(new Error('Response body is not a readable stream'));
      fail(`${artifact.name} tarball response body is not a readable stream`);
    }

    reader = response.body.getReader();
    if (!reader || typeof reader.read !== 'function' || typeof reader.cancel !== 'function') {
      stopDownload(new Error('Response body reader is invalid'));
      fail(`${artifact.name} tarball response body reader is invalid`);
    }
    const received = Buffer.allocUnsafe(maxBytes);
    let totalBytes = 0;
    while (true) {
      const result = await waitFor(
        Promise.resolve().then(() => reader.read()),
        'stream read',
      );
      if (!result || typeof result !== 'object') {
        stopDownload(new Error('stream reader returned an invalid result'));
        fail(`${artifact.name} tarball stream reader returned an invalid result`);
      }
      if (result.done) break;
      if (!(result.value instanceof Uint8Array)) {
        stopDownload(new Error('stream returned a non-byte chunk'));
        fail(`${artifact.name} tarball stream returned a non-byte chunk`);
      }
      if (result.value.byteLength > maxBytes - totalBytes) {
        stopDownload(new Error(`exceeds ${maxBytes} bytes`));
        fail(`${artifact.name} tarball exceeds ${maxBytes} bytes`);
      }
      received.set(result.value, totalBytes);
      totalBytes += result.value.byteLength;
    }
    const tarball = Buffer.from(received.subarray(0, totalBytes));
    verifySourceIntegrity(tarball, artifact.sourceIntegrity, `${artifact.name} sourceIntegrity`);
    return tarball;
  } catch (error) {
    stopDownload(error);
    throw error;
  } finally {
    clearTimeout(timeout);
    releaseReader();
  }
}

export async function verifyUpstreamVendorAssets(
  manifest,
  root = repoRoot,
  fetchImpl = globalThis.fetch,
) {
  validateVendorManifest(manifest);
  if (typeof fetchImpl !== 'function') fail('global fetch is unavailable');

  await Promise.all(
    manifest.artifacts.map(async (artifact) => {
      const tarball = await downloadWithIntegrity(artifact, fetchImpl);
      let tar;
      try {
        tar = gunzipSync(tarball, { maxOutputLength: maxTarBytes });
      } catch (error) {
        fail(`${artifact.name} tarball cannot be decompressed safely: ${error.message}`);
      }
      const files = extractTarFiles(tar, [artifact.packagePath, artifact.licensePackagePath]);
      const upstreamAsset = files.get(artifact.packagePath);
      const localAsset = readRepoFile(root, artifact.artifactPath, artifact.artifactPath);
      if (!upstreamAsset.equals(localAsset)) {
        fail(`${artifact.artifactPath} differs from ${artifact.name}@${artifact.version}`);
      }
      const upstreamLicense = files.get(artifact.licensePackagePath);
      const localLicense = readRepoFile(root, artifact.licensePath, artifact.licensePath);
      if (!upstreamLicense.equals(localLicense)) {
        fail(`${artifact.licensePath} differs from ${artifact.name}@${artifact.version}`);
      }
    }),
  );
  return manifest.artifacts.length;
}
