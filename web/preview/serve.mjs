// SPDX-License-Identifier: Apache-2.0

// The smallest static server for the UI preview, stdlib only, no cluster and
// no network beyond localhost. Serves web/ so the fixture, the sources' CSS
// and the built preview.js are all reachable; open the URL it prints.

import { createReadStream, existsSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { extname, join, normalize, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

// web/, or SERVE_ROOT: the built demo site (npm run demo).
const root = process.env.SERVE_ROOT ? resolve(process.env.SERVE_ROOT) + sep : fileURLToPath(new URL("..", import.meta.url));
const port = Number(process.env.PORT ?? 4180);

const types = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json; charset=utf-8",
  ".jsonl": "application/x-ndjson; charset=utf-8",
};

createServer((req, res) => {
  const path = normalize(decodeURIComponent(new URL(req.url ?? "/", "http://x").pathname));
  let file = join(root, path);
  if (file.startsWith(root) && existsSync(file) && statSync(file).isDirectory()) file = join(file, "index.html");
  if (!file.startsWith(root) || !existsSync(file) || !statSync(file).isFile()) {
    res.writeHead(404).end("not found");
    return;
  }
  res.writeHead(200, { "content-type": types[extname(file)] ?? "application/octet-stream" });
  createReadStream(file).pipe(res);
}).listen(port, "127.0.0.1", () => {
  if (process.env.SERVE_ROOT) {
    console.log(`live demo: http://127.0.0.1:${port}/`);
    return;
  }
  console.log(`room log preview:  http://127.0.0.1:${port}/preview/preview.html   (add ?live to replay)`);
  console.log(`room page preview: http://127.0.0.1:${port}/preview/full.html   (add ?theme=dark for the dark palette)`);
});
