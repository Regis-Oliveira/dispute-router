import { InjectionToken } from '@angular/core';

/**
 * The read API's origin.
 *
 * A token rather than a hardcoded string so the value is configuration, and a
 * separate origin rather than a dev-server proxy so the CORS rules the Go
 * service enforces are actually exercised in development - a proxy would hide
 * a misconfiguration until deployment.
 */
export const API_BASE = new InjectionToken<string>('API_BASE', {
  providedIn: 'root',
  factory: () => 'http://localhost:8081',
});
