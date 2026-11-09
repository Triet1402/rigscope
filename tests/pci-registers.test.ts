import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, writeFileSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {readMappedPciRegisters} from '../collector/sampling/pci-registers';

test('mapped register helper reads aligned words across pages and handles unavailable mappings', () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-pci-'));
  try {
    const path = join(root, 'resource0');
    const bytes = Buffer.alloc(0x21000);
    bytes.writeUInt32LE(0xc0002480, 0x2046c);
    bytes.writeUInt32LE(0x80008500, 0xe2a8);
    writeFileSync(path, bytes);
    assert.deepEqual(readMappedPciRegisters(path, [0x2046c, 0xe2a8]), [0xc0002480, 0x80008500]);
    assert.deepEqual(readMappedPciRegisters(path + '-missing', [0]), [null]);
    assert.deepEqual(readMappedPciRegisters(path, [0x22000]), [null]);
    assert.deepEqual(readMappedPciRegisters(path, [1]), [null]);
  } finally {
    rmSync(root, {recursive: true, force: true});
  }
});
