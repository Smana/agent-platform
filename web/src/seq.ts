// SPDX-License-Identifier: Apache-2.0

export type Verdict = "next" | "gap" | "duplicate";

// The client's own proof that it saw every seq once (SC-2).
export class SeqTracker {
  gaps = 0;
  duplicates = 0;
  constructor(public last = 0) {}

  // A sync frame's fromSeq - 1 is the broker's view of what this client holds. On a
  // resume it is the afterSeq sent, unless the broker clamped that to the room's
  // mark: then the mark wins, or every live event would look like a duplicate. It
  // never legitimately exceeds last once last > 0 (the broker clamps only down, and
  // a live gap's range starts at last + 1), so a baseline past it is a gap: false,
  // and last stays for the resume.
  baseline(fromSeq: number): boolean {
    if (this.last === 0 || fromSeq - 1 <= this.last) {
      this.last = fromSeq - 1;
      return true;
    }
    this.gaps++;
    return false;
  }

  observe(seq: number): Verdict {
    if (seq <= this.last) { this.duplicates++; return "duplicate"; }
    if (seq > this.last + 1) { this.gaps++; return "gap"; }
    this.last = seq;
    return "next";
  }
}
