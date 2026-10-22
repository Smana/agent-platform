#!/usr/bin/env python3
"""A CONNECT proxy that allows exactly the hosts in ALLOW (comma-separated) on 443 and logs each
CONNECT as `allow <host>` or `refuse <host>` to the file named by LOG. Stands in for the run's CNP."""
import asyncio
import os
import sys

ALLOW = set(filter(None, os.environ["ALLOW"].split(",")))
LOG = open(os.environ["LOG"], "a", buffering=1)


async def pipe(r, w):
    try:
        while data := await r.read(65536):
            w.write(data)
            await w.drain()
    except Exception:
        pass
    finally:
        w.close()


async def handle(r, w):
    line = (await r.readline()).decode(errors="replace")
    while (await r.readline()) not in (b"\r\n", b""):
        pass
    parts = line.split()
    if len(parts) < 2 or parts[0] != "CONNECT":
        LOG.write("refuse non-connect %s\n" % line.strip())
        w.write(b"HTTP/1.1 405 Method Not Allowed\r\n\r\n")
        w.close()
        return
    host, _, port = parts[1].rpartition(":")
    if host not in ALLOW or port != "443":
        LOG.write("refuse %s:%s\n" % (host, port))
        w.write(b"HTTP/1.1 403 Forbidden\r\n\r\n")
        w.close()
        return
    LOG.write("allow %s\n" % host)
    try:
        ur, uw = await asyncio.open_connection(host, 443)
    except OSError as exc:
        LOG.write("error %s %s\n" % (host, exc))
        w.write(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
        w.close()
        return
    w.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
    await w.drain()
    await asyncio.gather(pipe(r, uw), pipe(ur, w))


async def main():
    server = await asyncio.start_server(handle, "127.0.0.1", int(sys.argv[1]))
    async with server:
        await server.serve_forever()


asyncio.run(main())
