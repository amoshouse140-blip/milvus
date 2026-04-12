# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Milvus is a high-performance vector database. Go + C++ (`internal/core/`) + Rust (tantivy).
`pkg/` has its own go.mod (module: `github.com/milvus-io/milvus/pkg/v2`). Run `go get` from `pkg/` when adding dependencies there, not from root.

## Build

```bash
./scripts/install_deps.sh   # first-time: install all build deps (Go >= 1.21 required)
make                         # full build (C++ + Go), binary at bin/milvus
make build-go                # Go only (requires C++ libs already built)
make build-cpp               # C++ core only
```

## Linting & Formatting

```bash
make fmt                     # gofumpt formatting
make lint-fix                # gci (import order) + gofumpt
make static-check            # golangci-lint
make verifiers               # all checks: cppcheck + rustcheck + fmt + static-check
```

Run `make getdeps` first to install linting tools (golangci-lint, mockery, gotestsum).

## Testing

Go tests MUST use `-tags dynamic,test` and `-gcflags="all=-N -l"` (disable optimizations/inlining) or they won't compile / mockey-based monkey patching will fail:

```bash
go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/querycoordv2/...
go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/proxy/... -run TestXxx
```

Per-module shortcuts: `make test-querycoord`, `make test-proxy`, `make test-rootcoord`, `make test-datacoord`, etc.

```bash
make integration-test        # run integration tests
make codecov                 # generate coverage reports (Go + C++)
```

## Run Milvus Locally

```bash
scripts/start_standalone.sh    # start standalone mode
scripts/start_cluster.sh       # start cluster mode
scripts/stop_graceful.sh       # stop
scripts/standalone_embed.sh    # embedded standalone (no external deps)
```

## Architecture

Coordinators manage metadata and scheduling; nodes execute work.
- Coordinators: rootcoord, datacoord, querycoordv2 (note the v2 suffix in directory names)
- Nodes: proxy (user-facing), querynodev2, datanode, streamingnode
- MixCoord: hybrid coordinator combining multiple coord roles
- All component interfaces defined in `internal/types/types.go`
- Entry point: `cmd/`

## Code Conventions

- Error handling: use `merr` package, not fmt.Errorf
- Logging: use `pkg/v2/log`, not standard `"log"` or fmt.Println
- Import order: standard -> third-party -> github.com/milvus-io (enforced by gci)
- Config params: paramtable (`pkg/v2/util/paramtable`), config in `configs/milvus.yaml`

## PR and Commit Conventions

PR title format: `{type}: {description}`. Valid types: `feat:`, `fix:`, `enhance:`, `test:`, `doc:`, `auto:`, `build(deps):`.
PR body must be non-empty. Issue/doc linking rules:
- `fix:` -- must link issue (e.g. `issue: #123`)
- `feat:` -- must link issue + design doc from milvus-io/milvus-design-docs
- `enhance:` -- must link issue if size L/XL/XXL
- `doc:`, `test:` -- no issue required
- 2.x branch PRs must link the corresponding master PR (e.g. `pr: #123`)

DCO check is required. Always use `-s` so the developer's Signed-off-by is appended last:

```
git commit -s -m "commit message

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

`-s` auto-appends `Signed-off-by: <developer>` at the end. The developer MUST be the final sign-off, not the AI.

## Generated Files -- Do Not Hand-Edit

- Mock files (`internal/mocks/*`, `mock_*.go`): regenerate with `make generate-mockery-{module}` (e.g. `generate-mockery-proxy`, `generate-mockery-querycoord`)
- Proto files (`pkg/proto/*.pb.go`): regenerate with `make generated-proto-without-cpp` (fast, no C++ rebuild)
