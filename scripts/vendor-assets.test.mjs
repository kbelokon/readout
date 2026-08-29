import assert from 'node:assert/strict';
import { Buffer } from 'node:buffer';
import { createHash } from 'node:crypto';
import test from 'node:test';
import {
  downloadWithIntegrity,
  extractTarFiles,
  loadVendorManifest,
  validateVendorManifest,
  verifySourceIntegrity,
  verifyVendoredJavaScriptSet,
} from './vendor-assets.mjs';

function cloneManifest() {
  return structuredClone(loadVendorManifest());
}

function writeTarString(header, start, length, value) {
  const encoded = Buffer.from(value);
  assert.ok(encoded.length <= length, `tar fixture field too long: ${value}`);
  encoded.copy(header, start);
}

function writeTarOctal(header, start, length, value) {
  writeTarString(header, start, length, `${value.toString(8).padStart(length - 1, '0')}\0`);
}

function tarEntry(path, data, { magic = 'ustar\0', type = 48, version = '00' } = {}) {
  const body = Buffer.from(data);
  const header = Buffer.alloc(512);
  writeTarString(header, 0, 100, path);
  writeTarOctal(header, 100, 8, 0o644);
  writeTarOctal(header, 108, 8, 0);
  writeTarOctal(header, 116, 8, 0);
  writeTarOctal(header, 124, 12, body.length);
  writeTarOctal(header, 136, 12, 0);
  header.fill(32, 148, 156);
  header[156] = type;
  writeTarString(header, 257, 6, magic);
  writeTarString(header, 263, 2, version);
  const checksum = header.reduce((sum, byte) => sum + byte, 0);
  writeTarString(header, 148, 8, `${checksum.toString(8).padStart(6, '0')}\0 `);
  const padding = Buffer.alloc((512 - (body.length % 512)) % 512);
  return Buffer.concat([header, body, padding]);
}

function tarArchive(entries) {
  return Buffer.concat([
    ...entries.map(([path, data, options]) => tarEntry(path, data, options)),
    Buffer.alloc(1024),
  ]);
}

function integrityFor(bytes) {
  return `sha512-${createHash('sha512').update(bytes).digest('base64')}`;
}

function downloadArtifact(bytes = Buffer.alloc(0)) {
  return {
    name: 'fixture',
    sourceIntegrity: integrityFor(bytes),
    sourceURL: 'https://example.invalid/fixture.tgz',
  };
}

test('manifest is the exact runtime vendor set', () => {
  assert.doesNotThrow(() => validateVendorManifest(cloneManifest()));

  const missing = cloneManifest();
  missing.artifacts.pop();
  assert.throws(() => validateVendorManifest(missing), /exactly 2 runtime vendors/u);

  const extra = cloneManifest();
  extra.artifacts.push({ ...extra.artifacts[0], name: 'htmx-ext-preload' });
  assert.throws(() => validateVendorManifest(extra), /exactly 2 runtime vendors/u);
});

test('manifest rejects unsafe package paths', () => {
  const manifest = cloneManifest();
  manifest.artifacts[0].packagePath = 'package/../escape.js';
  assert.throws(() => validateVendorManifest(manifest), /normalized relative path/u);
});

test('runtime vendor set excludes only the explicit first-party bundle', () => {
  const manifest = cloneManifest();
  const expected = [
    'internal/assets/static/readout.js',
    ...manifest.artifacts.map((artifact) => artifact.artifactPath),
  ];
  assert.doesNotThrow(() => verifyVendoredJavaScriptSet(manifest, expected));
  assert.throws(
    () =>
      verifyVendoredJavaScriptSet(manifest, [...expected, 'internal/assets/static/preload.min.js']),
    /preload\.min\.js/u,
  );
  assert.throws(
    () => verifyVendoredJavaScriptSet(manifest, expected.slice(0, -1)),
    /runtime-vendored JavaScript paths/u,
  );
  assert.throws(
    () => verifyVendoredJavaScriptSet(manifest, expected.slice(1)),
    /readout\.js must be present/u,
  );
});

test('source integrity rejects changed tarball bytes', () => {
  const bytes = Buffer.from('pinned npm tarball');
  const integrity = integrityFor(bytes);
  assert.doesNotThrow(() => verifySourceIntegrity(bytes, integrity));
  assert.throws(
    () => verifySourceIntegrity(Buffer.from('changed npm tarball'), integrity),
    /does not match downloaded bytes/u,
  );
});

test('streaming download enforces its running cap without trusting Content-Length', async () => {
  for (const declaredLength of [null, '1']) {
    let canceled = false;
    const body = {
      getReader() {
        const chunks = [Buffer.from('12345'), Buffer.from('67890')];
        return {
          cancel() {
            canceled = true;
            return Promise.resolve();
          },
          read() {
            const value = chunks.shift();
            return Promise.resolve(value ? { done: false, value } : { done: true });
          },
          releaseLock() {},
        };
      },
    };
    const fetchImpl = async () => ({
      body,
      headers: { get: () => declaredLength },
      ok: true,
      status: 200,
      url: 'https://example.invalid/fixture.tgz',
    });

    await assert.rejects(
      () =>
        downloadWithIntegrity(downloadArtifact(), fetchImpl, {
          maxBytes: 8,
          timeoutMilliseconds: 1_000,
        }),
      /exceeds 8 bytes/u,
    );
    assert.equal(canceled, true);
  }
});

test('successful streaming download clears its timeout without aborting or canceling', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const bytes = Buffer.from('bounded fixture tarball');
  let aborted = false;
  let canceled = false;
  let read = false;
  let releases = 0;
  const fetchImpl = async (_url, options) => {
    options.signal.addEventListener('abort', () => {
      aborted = true;
    });
    return {
      body: {
        getReader() {
          return {
            cancel() {
              canceled = true;
              return Promise.resolve();
            },
            read() {
              if (read) return Promise.resolve({ done: true });
              read = true;
              return Promise.resolve({ done: false, value: bytes });
            },
            releaseLock() {
              releases += 1;
            },
          };
        },
      },
      headers: { get: () => null },
      ok: true,
      status: 200,
      url: 'https://example.invalid/fixture.tgz',
    };
  };

  const downloaded = await downloadWithIntegrity(downloadArtifact(bytes), fetchImpl, {
    maxBytes: bytes.length,
    timeoutMilliseconds: 1_000,
  });
  t.mock.timers.tick(5_000);
  assert.deepEqual(downloaded, bytes);
  assert.equal(aborted, false);
  assert.equal(canceled, false);
  assert.equal(releases, 1);
});

test('streaming download keeps memory bounded across many small chunks', async () => {
  const bytes = Buffer.alloc(4_096, 97);
  let offset = 0;
  const fetchImpl = async () => ({
    body: {
      getReader() {
        return {
          cancel() {
            return Promise.resolve();
          },
          read() {
            if (offset === bytes.length) return Promise.resolve({ done: true });
            const value = bytes.subarray(offset, offset + 1);
            offset += 1;
            return Promise.resolve({ done: false, value });
          },
          releaseLock() {},
        };
      },
    },
    headers: { get: () => null },
    ok: true,
    status: 200,
    url: 'https://example.invalid/fixture.tgz',
  });

  const downloaded = await downloadWithIntegrity(downloadArtifact(bytes), fetchImpl, {
    maxBytes: bytes.length,
    timeoutMilliseconds: 1_000,
  });
  assert.deepEqual(downloaded, bytes);
});

test('streaming download aborts and cancels a hanging body at its total timeout', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let aborted = false;
  let canceled = false;
  let readStarted = false;
  let releases = 0;
  const fetchImpl = async (_url, options) => {
    options.signal.addEventListener('abort', () => {
      aborted = true;
    });
    return {
      body: {
        getReader() {
          return {
            cancel() {
              canceled = true;
              return Promise.resolve();
            },
            read() {
              readStarted = true;
              return new Promise(() => {});
            },
            releaseLock() {
              releases += 1;
            },
          };
        },
      },
      headers: { get: () => null },
      ok: true,
      status: 200,
      url: 'https://example.invalid/fixture.tgz',
    };
  };

  const download = downloadWithIntegrity(downloadArtifact(), fetchImpl, {
    maxBytes: 8,
    timeoutMilliseconds: 500,
  });
  for (let turn = 0; turn < 10 && !readStarted; turn += 1) await Promise.resolve();
  assert.equal(readStarted, true);
  t.mock.timers.tick(500);
  await assert.rejects(download, /timed out after 500ms/u);
  assert.equal(aborted, true);
  assert.equal(canceled, true);
  assert.equal(releases, 1);
});

test('tar reader returns only declared regular files', () => {
  const tar = tarArchive([
    ['package/dist/vendor.min.js', 'vendor'],
    ['package/LICENSE', 'license'],
    ['package/README.md', 'ignored'],
  ]);
  const files = extractTarFiles(tar, ['package/dist/vendor.min.js', 'package/LICENSE']);
  assert.equal(files.get('package/dist/vendor.min.js')?.toString(), 'vendor');
  assert.equal(files.get('package/LICENSE')?.toString(), 'license');
  assert.equal(files.has('package/README.md'), false);
});

test('tar reader rejects traversal and duplicate entries', () => {
  assert.throws(
    () => extractTarFiles(tarArchive([['../escape', 'x']]), ['package/LICENSE']),
    /normalized relative path/u,
  );
  assert.throws(
    () =>
      extractTarFiles(
        tarArchive([
          ['package/LICENSE', 'first'],
          ['package/LICENSE', 'second'],
        ]),
        ['package/LICENSE'],
      ),
    /duplicate entry/u,
  );
});

test('tar reader requires POSIX ustar headers and two zero EOF blocks', () => {
  const entry = tarEntry('package/LICENSE', 'license');
  assert.throws(
    () =>
      extractTarFiles(tarArchive([['package/LICENSE', 'license', { magic: 'notar\0' }]]), [
        'package/LICENSE',
      ]),
    /POSIX ustar magic/u,
  );
  assert.throws(
    () =>
      extractTarFiles(tarArchive([['package/LICENSE', 'license', { version: '01' }]]), [
        'package/LICENSE',
      ]),
    /POSIX ustar version 00/u,
  );
  assert.throws(
    () => extractTarFiles(entry, ['package/LICENSE']),
    /missing its two zero end blocks/u,
  );
  assert.throws(
    () => extractTarFiles(Buffer.concat([entry, Buffer.alloc(512)]), ['package/LICENSE']),
    /does not end with two zero blocks/u,
  );
  assert.throws(
    () => extractTarFiles(tarArchive([['package/LICENSE', 'license']]).subarray(0, -1), []),
    /not block-aligned/u,
  );
});

test('tar reader rejects links, PAX metadata, and GNU extensions', () => {
  for (const type of [49, 50, 120, 103, 76, 75]) {
    assert.throws(
      () =>
        extractTarFiles(tarArchive([['package/unsupported', 'payload', { type }]]), [
          'package/LICENSE',
        ]),
      /unsupported typeflag/u,
    );
  }
});
