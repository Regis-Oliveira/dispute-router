import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject, input, signal } from '@angular/core';
import { EvidenceApi, formatBytes } from '../core/evidence.api';

@Component({
  selector: 'app-evidence-panel',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [DatePipe],
  templateUrl: './evidence-panel.html',
  styleUrl: './evidence-panel.css',
})
export class EvidencePanel {
  readonly disputeId = input.required<number>();

  protected readonly api = inject(EvidenceApi);
  protected readonly dragging = signal(false);

  protected readonly files = computed(() => this.api.files.value());
  protected readonly isLoading = computed(() => this.api.files.isLoading());

  protected readonly progress = computed(() => this.api.upload().progress ?? 0);

  protected size(bytes: number): string {
    return formatBytes(bytes);
  }

  protected onPicked(event: Event): void {
    const input = event.target as HTMLInputElement;
    const file = input.files?.[0];
    if (file) void this.api.send(this.disputeId(), file);
    // Clearing lets the same file be chosen twice in a row, which otherwise
    // fires no change event and looks like the button is broken.
    input.value = '';
  }

  protected onDragOver(event: DragEvent): void {
    event.preventDefault();
    this.dragging.set(true);
  }

  protected onDragLeave(): void {
    this.dragging.set(false);
  }

  protected onDrop(event: DragEvent): void {
    event.preventDefault();
    this.dragging.set(false);
    const file = event.dataTransfer?.files?.[0];
    if (file) void this.api.send(this.disputeId(), file);
  }
}
