import { computed, inject, Injectable, signal } from '@angular/core';
import { httpResource } from '@angular/common/http';
import { API_BASE } from './api.config';
import { DisputesApi } from './disputes.api';
import type { EvidenceFile, UploadTarget } from './api.types';

/**
 * The same whitelist the API enforces, checked here for the feedback.
 *
 * The server remains the gate - anything in a browser is a suggestion - but
 * telling somebody their .docx is unsupported before they wait for an upload is
 * the difference between a rule and an ambush.
 */
const ACCEPTED = new Map<string, string>([
  ['application/pdf', 'PDF'],
  ['image/png', 'PNG'],
  ['image/jpeg', 'JPEG'],
  ['text/plain', 'text'],
  ['text/csv', 'CSV'],
]);

/**
 * Checked here only.
 *
 * A presigned PUT cannot carry a size limit - the signature covers the method,
 * the key and the content type, not the body - so this is a courtesy, not a
 * control. Enforcing it properly needs a presigned POST policy with a
 * content-length-range, which is the next thing to build here.
 */
const MAX_BYTES = 25 * 1024 * 1024;

export type UploadStatus = 'idle' | 'preparing' | 'uploading' | 'done' | 'error';

export interface UploadState {
  status: UploadStatus;
  filename?: string;
  /** 0-100, real: measured from the bytes actually on the wire. */
  progress?: number;
  message?: string;
}

@Injectable({ providedIn: 'root' })
export class EvidenceApi {
  private readonly base = inject(API_BASE);
  private readonly disputes = inject(DisputesApi);

  /** Reloads whenever the selected dispute changes; idle when none is. */
  readonly files = httpResource<EvidenceFile[]>(
    () => {
      const id = this.disputes.selectedId();
      return id === null ? undefined : `${this.base}/api/disputes/${id}/evidence`;
    },
    { defaultValue: [] },
  );

  private readonly uploadState = signal<UploadState>({ status: 'idle' });
  readonly upload = this.uploadState.asReadonly();

  readonly isBusy = computed(
    () => this.upload().status === 'preparing' || this.upload().status === 'uploading',
  );

  readonly acceptAttribute = [...ACCEPTED.keys()].join(',');

  describeAccepted(): string {
    return [...ACCEPTED.values()].join(', ');
  }

  reset(): void {
    this.uploadState.set({ status: 'idle' });
  }

  /**
   * Two requests, and the interesting part is that the second one does not
   * touch this application's server at all: the browser is handed a signed URL
   * and PUTs the bytes straight to S3.
   */
  async send(disputeId: number, file: File): Promise<void> {
    if (!ACCEPTED.has(file.type)) {
      this.uploadState.set({
        status: 'error',
        filename: file.name,
        message: `${file.type || 'That file type'} is not accepted. Allowed: ${this.describeAccepted()}.`,
      });
      return;
    }

    if (file.size > MAX_BYTES) {
      this.uploadState.set({
        status: 'error',
        filename: file.name,
        message: `That file is ${formatBytes(file.size)}. The limit is ${formatBytes(MAX_BYTES)}.`,
      });
      return;
    }

    this.uploadState.set({ status: 'preparing', filename: file.name });

    let target: UploadTarget;
    try {
      const response = await fetch(`${this.base}/api/disputes/${disputeId}/evidence`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ filename: file.name, content_type: file.type }),
      });
      if (!response.ok) {
        const body = (await response.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `The server refused the upload (${response.status}).`);
      }
      target = (await response.json()) as UploadTarget;
    } catch (error) {
      this.uploadState.set({
        status: 'error',
        filename: file.name,
        message: (error as Error).message,
      });
      return;
    }

    this.uploadState.set({ status: 'uploading', filename: file.name, progress: 0 });

    try {
      await putWithProgress(target, file, (progress) =>
        this.uploadState.set({ status: 'uploading', filename: file.name, progress }),
      );
    } catch (error) {
      this.uploadState.set({
        status: 'error',
        filename: file.name,
        message: (error as Error).message,
      });
      return;
    }

    this.uploadState.set({ status: 'done', filename: file.name, progress: 100 });
    this.files.reload();
  }
}

/**
 * The one request in this application that does not go through HttpClient.
 *
 * The app is configured with `withFetch()`, and the Fetch API has no way to
 * report how much of a request body has been sent - it can report download
 * progress and nothing else. For a 40MB scan of a delivery receipt that is the
 * difference between a progress bar and a frozen dialog, so this one call drops
 * to XMLHttpRequest, which has had upload progress events since 2006.
 *
 * The Content-Type must match the presigned value exactly. The signature covers
 * it, so sending anything else is rejected by S3 with a signature mismatch
 * rather than a helpful error.
 */
function putWithProgress(
  target: UploadTarget,
  file: File,
  onProgress: (percent: number) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open(target.method || 'PUT', target.url, true);
    request.setRequestHeader('Content-Type', target.content_type);

    request.upload.addEventListener('progress', (event) => {
      if (event.lengthComputable) {
        onProgress(Math.round((event.loaded / event.total) * 100));
      }
    });

    request.addEventListener('load', () => {
      if (request.status >= 200 && request.status < 300) {
        resolve();
        return;
      }
      // S3 answers with XML. Surfacing the status is more use than surfacing
      // an unparsed document.
      reject(new Error(`Storage rejected the upload (${request.status}).`));
    });

    request.addEventListener('error', () =>
      reject(new Error('The upload could not reach storage. Check that LocalStack is running.')),
    );
    request.addEventListener('abort', () => reject(new Error('Upload cancelled.')));

    request.send(file);
  });
}

export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB'];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
}
