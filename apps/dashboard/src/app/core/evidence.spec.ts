import { describe, expect, it } from 'vitest';
import { formatBytes } from './evidence.api';

describe('formatBytes', () => {
  it('keeps small files in bytes', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(46)).toBe('46 B');
    expect(formatBytes(1023)).toBe('1023 B');
  });

  it('steps up a unit at a time', () => {
    expect(formatBytes(1024)).toBe('1.0 KB');
    expect(formatBytes(1024 * 1024)).toBe('1.0 MB');
    expect(formatBytes(1024 * 1024 * 1024)).toBe('1.0 GB');
  });

  /** One decimal below ten, none above: "9.4 MB" reads, "9.4213 MB" does not. */
  it('drops the decimal once the number is big enough not to need it', () => {
    expect(formatBytes(9.4 * 1024 * 1024)).toBe('9.4 MB');
    expect(formatBytes(25 * 1024 * 1024)).toBe('25 MB');
    expect(formatBytes(512 * 1024)).toBe('512 KB');
  });

  /** The size cap in the upload guard has to render as something readable. */
  it('renders the upload limit the way the error message needs', () => {
    expect(formatBytes(25 * 1024 * 1024)).toBe('25 MB');
  });
});
