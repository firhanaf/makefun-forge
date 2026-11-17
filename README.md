# makefun-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/firhanaf/makefun-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/firhanaf/makefun-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge written in Go, built for the make.fun contest
[github-core-clone-01](https://make.fun/bounty/github-core-clone-01). It is one process with no
dependencies, no UI and no `git` binary. It serves from memory:

| Route | What it is |
| --- | --- |
| `GET /healthz` | `200 {"ok":true}`, the env's boot probe (no token) |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, upload-pack protocol v2 (with v0 fallback), receive-pack v0 |
| `/api/v3/...` | GitHub REST v3: repositories, branches, refs, commits, contents, labels, issues, comments, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: queries and mutations `gh` and `github-mcp-server` send, introspection, batching |
| `POST /mcp` | Model Context Protocol over Streamable HTTP: `server/discover` (2026-07-28) and the `initialize` handshake |

Every route except `/healthz` requires `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`.

## Run it

```sh
node main.ts
```

## How it runs

The platform starts `node /bot/main.ts`. `main.ts` only launches `bin/forge`, a statically linked
Linux amd64 binary built from `go/`. The binary handles the whole lifecycle:

1. It binds an ephemeral port.
2. It registers that port with `POST $MAKEFUN_ENV_URL/register`.
3. It serves until `GET $MAKEFUN_ENV_URL/done` answers `{"done":true}`.
4. It exits 0.

If `/bot` was checked out without the executable bit, the launcher copies the binary to the temp
directory and runs it from there.

## Build

```sh
npm run build
```

The build needs Go 1.24 or newer and runs
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/forge ./go`. Commit
the binary with its executable bit set (`git update-index --chmod=+x bin/forge`).

For local testing outside a trial, set `FORGE_LISTEN=127.0.0.1:8081` (and optionally
`MAKEFUN_SHOP_TOKEN`). The binary then serves on that address without an env to register with.

## Design notes

- **HTTP.** A lean HTTP/1.1 front end (`go/httpd.go`) parses requests with `http.ReadRequest`,
  buffers each response and sends it with one write. It skips the background read that net/http's
  server starts for every request, which roughly halves the forge's CPU per request on a keep-alive
  connection. REST JSON is encoded without reflection, byte for byte as `encoding/json` would.

- **Git objects.** Objects live in one content-addressed store shared by all repositories. Each
  object's zlib stream is computed once and reused by every pack that contains it.
- **Fetch.** Fetch answers are cached per repository ref generation and per request, and built once
  under concurrency. This keeps 50 concurrent shallow clones of the same repository cheap. Shallow
  boundaries (`deepen`, `deepen-relative`, `shallow`/`unshallow`) are computed by a breadth-first walk
  from the wants.
- **Push.** Pushed packs may carry OFS/REF deltas, which are resolved on receive. Every ref update
  must fast-forward. Report lines go inside side-band 1 when the client asks for it.
- **Merges.** Pull requests merge with a file-level three-way merge over the merge base. Two branches
  that change the same file conflict, which REST reports as 405 and GraphQL as `CONFLICTING`.
- **Global ids.** GraphQL global ids are opaque and stable. MCP tools run through the REST handlers
  in process, so an object read over MCP is the same object REST answers.

## Licence

MIT, see [LICENSE](LICENSE).

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
