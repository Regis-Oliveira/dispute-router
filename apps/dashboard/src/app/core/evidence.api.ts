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
 * Checked here so an oversized file fails instantly instead of after a long
 * upload. It is not the control: the signed policy carries a
 * content-length-range and S3 counts the bytes it actually receives, so a
 * client that skips this check is refused by storage rather than by politeness.
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
      await postWithProgress(target, file, (progress) =>
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
 * progress and nothing else. For a 25MB scan of a delivery receipt that is the
 * difference between a progress bar and a frozen dialog, so this one call drops
 * to XMLHttpRequest, which has had upload progress events since 2006.
 *
 * The form is a signed policy plus the file. Two rules S3 does not forgive:
 * every field the policy conditions on must be present, and the file part must
 * come last - S3 stops reading fields when it reaches it.
 */
function postWithProgress(
  target: UploadTarget,
  file: File,
  onProgress: (percent: number) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const form = new FormData();
    for (const [name, value] of Object.entries(target.fields)) {
      form.append(name, value);
    }
    form.append('file', file);

    const request = new XMLHttpRequest();
    request.open('POST', target.url, true);
    // No Content-Type header is set by hand: the browser has to write the
    // multipart boundary into it, and overriding it breaks the parse.

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
      // S3 answers with XML, and its message is genuinely useful here - it says
      // which policy condition failed, or that the file was too large.
      reject(new Error(explainS3Error(request.status, request.responseText)));
    });

    request.addEventListener('error', () =>
      reject(new Error('The upload could not reach storage. Check that LocalStack is running.')),
    );
    request.addEventListener('abort', () => reject(new Error('Upload cancelled.')));

    request.send(form);
  });
}

function explainS3Error(status: number, body: string): string {
  const message = /<Message>([^<]+)<\/Message>/.exec(body)?.[1];
  if (message) return `Storage refused the upload: ${message}`;
  return `Storage refused the upload (${status}).`;
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
