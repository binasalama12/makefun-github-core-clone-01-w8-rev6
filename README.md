[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/binasalama12/makefun-github-core-clone-01-w8-rev6/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/binasalama12/makefun-github-core-clone-01-w8-rev6/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# github-core-clone-01 — bright-falcon-67 (rev6, Go raw-TCP forge)

A standalone GitHub-compatible git forge, written for the make.fun
`github-core-clone-01` competition. The forge itself is a static Go binary
(built from the `.go` sources in this repo, Go standard library only); the
platform entrypoint `main.ts` is a thin launcher that spawns it with the
same environment, forwards termination signals, and mirrors its exit code.
No git binary, no network beyond the trial environment.

## Run it

```sh
node main.ts
```

The platform runs `node /bot/main.ts`. The Go process binds an ephemeral
port, registers it with `$MAKEFUN_ENV_URL/register`, serves the forge,
polls `$MAKEFUN_ENV_URL/done`, and exits 0 when the environment says the
trial is over. Every API surface requires
`Authorization: Bearer <$MAKEFUN_SHOP_TOKEN>`.

## Architecture

This revision replaces the Node transport with a hand-rolled raw-TCP HTTP
server (`transport_raw.go`), chosen after a 25-trial head-to-head
tournament against the previous Node entry: the raw-TCP build won 76% of
paired trials with a +26.8% median geometric-mean speedup and 120/120
conformance in every counted trial.

- One in-memory store serves every surface, so an issue created over MCP
  reads back over REST with the same id, and a branch pushed over git is
  the head a GraphQL `createPullRequest` opens.
- `transport_raw.go` — the serving transport: a raw TCP listener with a
  minimal HTTP/1.1 parser tuned for the trial workloads (push, clone,
  read, write), keep-alive, and no per-request framework overhead.
- `gitproto.go` / `gitcore.go` — the hand-rolled git object store
  (SHA-1 + zlib, pack read/write with delta resolution) and the smart
  HTTP protocol: upload-pack v2 with a v0 side-band-64k fallback,
  receive-pack v0 with `report-status` and non-fast-forward refusal.
- `rest.go` — REST v3: repositories, branches, git refs, commits,
  contents (create/update/delete with sha checks), labels, issues,
  comments, pull requests and squash merges.
- `gql.go` — GraphQL v4: a compact parser/executor covering the documents
  `gh` and `github-mcp-server` send, including `node(id:)` and the
  `__type` introspection probes.
- `mcp.go` — MCP at `/mcp`: JSON-RPC 2.0 Streamable HTTP with the
  `server/discover` revision and the 2025-11-25 `initialize` handshake.
- `main.go` — the entry ↔ env lifecycle (register, poll done, exit 0);
  `app.go` / `forge.go` — routing and the root forge store;
  `jsonw.go` — the JSON writer shared by all surfaces.

## Layout

- `main.ts` — the platform launcher for the committed `forge` binary.
- `*.go` + `go.mod` — the complete forge source (module `forge`, Go 1.23).
- `forge` — the prebuilt static binary (CGO disabled, stripped).
- `LICENSE` — MIT. `package.json` — entry metadata.

Determinism: timestamps come from a seeded logical clock, never the wall
clock, so a replay leaves byte-identical exports.
