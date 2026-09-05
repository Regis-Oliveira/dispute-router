import type { Routes } from '@angular/router';

export const routes: Routes = [
  {
    path: 'disputes',
    // Lazy even with one route: it is the shape the second and third screens
    // will need, and retrofitting it later means touching every route at once.
    loadComponent: () => import('./disputes/disputes-page').then((m) => m.DisputesPage),
    title: 'Disputes · Dispute Router',
  },
  { path: '', pathMatch: 'full', redirectTo: 'disputes' },
  { path: '**', redirectTo: 'disputes' },
];
