// SPDX-License-Identifier: Apache-2.0

// The smallest static server for the UI preview, stdlib only, no cluster and
// no network beyond localhost. Serves web/ so the fixture, the sources' CSS
// and the built preview.js are all reachable; open the URL it prints.

import { createReadStream, existsSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { extname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url)); // web/
const port = Number(process.env.PORT ?? 4180);

const types = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".jsonl": "application/x-ndjson; charset=utf-8",
};

createServer((req, res) => {
  const path = normalize(decodeURIComponent(new URL(req.url ?? "/", "http://x").pathname));
  const file = join(root, path);
  if (!file.startsWith(root) || !existsSync(file) || !statSync(file).isFile()) {
    res.writeHead(404).end("not found");
    return;
  }
  res.writeHead(200, { "content-type": types[extname(file)] ?? "application/octet-stream" });
  createReadStream(file).pipe(res);
}).listen(port, "127.0.0.1", () => {
  console.log(`room UI preview: http://127.0.0.1:${port}/preview/preview.html   (add ?live to replay)`);
});
