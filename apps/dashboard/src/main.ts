import { bootstrapApplication } from '@angular/platform-browser';
import { appConfig } from './app/app.config';
import { App } from './app/app';
import { initSentry } from './app/core/sentry';

// Before bootstrap, so an error thrown while the app is starting is still
// caught. A no-op unless a DSN is set, which is the local default.
initSentry();

bootstrapApplication(App, appConfig)
  .catch((err) => console.error(err));
