import type { SocketLike } from "../src/conn";

// A WebSocket the test drives: open, recv a frame, or drop as the broker would.
export class FakeSocket implements SocketLike {
  onopen: SocketLike["onopen"] = null;
  onmessage: SocketLike["onmessage"] = null;
  onclose: SocketLike["onclose"] = null;
  sent: Record<string, unknown>[] = [];
  closed = false; // by the page, not the broker
  private done = false; // as a WebSocket, a closed socket closes no more
  constructor(readonly url: string) {}
  send(data: string) { this.sent.push(JSON.parse(data)); }
  close(code = 1000, reason = "") { this.closed = true; this.drop(code, reason); }
  open() { this.onopen?.(); }
  recv(frame: unknown) { this.onmessage?.({ data: JSON.stringify(frame) }); }
  drop(code: number, reason = "") {
    if (this.done) return;
    this.done = true;
    this.onclose?.({ code, reason });
  }
}
