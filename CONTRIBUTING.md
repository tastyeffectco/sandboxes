# Contributing to sandboxed

Thanks for your interest! sandboxed aims to stay **small, reliable, and easy to
self-host**. We favour a tight, well-understood core over feature breadth.

## Project layout

```
sandboxed/
├── docker-compose.yml     # the full stack: traefik + sandboxd
├── install.sh             # one-click installer
├── .env.example           # all configuration, with defaults
├── traefik/               # edge config (static + dynamic/ wake & auth routers)
├── image/                 # the per-sandbox base image (Dockerfile + skeleton)
└── control-plane/         # the sandboxd control plane (Go)
    ├── cmd/sandboxd/       # daemon entrypoint + env wiring
    ├── cmd/runtimed/       # in-sandbox supervisor (baked into the image)
    └── internal/           # docker wrapper, storage, traefik labels, reaper,
                            # wake path, reconciler, store (SQLite), API
```

## Dev loop

The control plane is plain Go (toolchain 1.22+):

```bash
cd control-plane
go build ./...      # compile
go test ./...       # unit tests
go vet ./...
```

To exercise the whole stack, run `./install.sh` (or `docker compose up -d
--build`) and hit the API on `SANDBOXED_API_BIND`. The base image build is the
slow part; it's cached after the first run.

## Guidelines

- **Keep the core lean.** New host dependencies are a hard sell — the headline
  promise is "runs fully on Docker, one command." If a feature needs more than
  Docker, it should be optional and default-off.
- **The control plane shells out to the `docker` CLI** by design (easy to read,
  easy to debug). Reach for the SDK only with a measured reason.
- **SQLite is the source of truth**; the reconciler converges Docker to it.
  Don't add state that lives only in Docker.
- Match the surrounding code's style and comment density.
- Include a test for behaviour changes where practical.

## Reporting issues

Please include your Docker version, OS, the relevant `docker compose logs
sandboxd` output, and the request that triggered the problem.
