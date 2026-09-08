import type { Routes } from '@angular/router';

export const routes: Routes = [
  {
    path: 'disputes',
    // Lazy even with one route: it is the shape the second and third screens
    // will need, and retrofitting it later means touching every route at once.
    loadComponent: () => import('./disputes/disputes-page').then((m) => m.DisputesPage),
    title: 'Disputes · Dispute Router',
  },
  {
    path: 'reviews',
    loadComponent: () => import('./reviews/reviews-page').then((m) => m.ReviewsPage),
    title: 'Review queue · Dispute Router',
  },
  {
    path: 'decisions',
    loadComponent: () => import('./reviews/decisions-page').then((m) => m.DecisionsPage),
    title: 'Decisions · Dispute Router',
  },
  { path: '', pathMatch: 'full', redirectTo: 'disputes' },
  { path: '**', redirectTo: 'disputes' },
];
