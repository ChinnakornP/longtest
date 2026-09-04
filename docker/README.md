# docker

Image definitions. Deliberately empty in stage 1 — the local stack
(`docker-compose.yml`) only runs upstream Postgres and MinIO images, and the
backend, web app and daemon are run from source during development.

What lands here later:

| File                  | Task | Notes                                                         |
| --------------------- | ---- | ------------------------------------------------------------- |
| `server.Dockerfile`   | T12  | distroless, non-root                                          |
| `web.Dockerfile`      | T12  | Next.js standalone output                                     |
| `browser.Dockerfile`  | T9   | Chromium sandbox: non-root, read-only rootfs, dropped caps,   |
|                       |      | seccomp profile, no host network, CPU/RAM/PID limits          |

The browser image is a security boundary, not a packaging convenience: it is
the process that opens pages we do not control. It never gets `--privileged`,
host networking or a mounted docker socket.
