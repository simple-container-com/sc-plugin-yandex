# sc-plugin-yandex — notes for AI coding agents

If you're working here, you've been asked to extend the Yandex Cloud plugin for Simple Container. The context you need in one page:

## What this repo is

The second out-of-tree SC cloud driver (after `sc-plugin-cloudflare`), following the pattern defined in [forge/docs/design/2026-10-24/sc-plugin-architecture](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-10-24/sc-plugin-architecture).

Two Go modules:
- `/api` — config structs. Imports SC core's `pkg/api` only. SC core imports THIS module at build time to validate `type: yc-*` configs.
- `/plugin` — the subprocess binary. Imports both SC core's plugin runtime AND the `pulumi-yandex` SDK. SC core NEVER imports this module.

The plugin advertises a schema (`SCCloudPlugin.Schema`) and SC core routes resource dispatch to it via `hashicorp/go-plugin` gRPC subprocess + the shared Pulumi Resource Monitor.

## V1 scope

**Shipped**:
- Registrar surface (`type: yc-dns`). `ProvisionRegistrar`, `NewDnsRecord`, `NewOverrideHeaderRule`, `ProvisionDomainForEndpoint` all wired through the plugin bus with the `waitForRef` pattern that makes Pulumi's async `RegisterResource` effectively synchronous on the plugin side.
- `LogStream` RPC with progress events tagged by phase (`provision-registrar`, `dns-record`, `worker-route`, `endpoint`).

**Lifted but NOT YET wired**:
- `pulumi:providers:yandex` (Provider RPC) — the lifted driver is at `plugin/internal/provider.go`. SC core's sidecar factory (`makeProviderSidecar`) exists and sends the provider URN back through `recordProviderURN`. Need to add a `Provider` handler on the plugin side.
- `yc-bucket` and `yc-serverless-container` (Provision RPC). SC core's `makeResourceSidecar` currently passes only `resource_type` + `resource_name` — **the plugin receives NO config inputs**. Fixing this requires an SC-core-side bump to serialize `input.Descriptor.Config.Config` into the `ProvisionRequest.inputs` map (same `serializeConfigToInputs` as the registrar sidecar).
- `yc-bucket` ComputeProcessor — same input-serialization gap.
- `yc-object-storage` state store (`InitStateStore` RPC). The lifted `InitStateStore` function is at `plugin/internal/provider.go:38`. The RPC request already carries a config map, so this is straightforward wire-up.

## Plugin-bus inputs are JSON-encoded, then YAML-decoded

Core serialises a resource's config with `encoding/json`; `decodeInputsAs` decodes it through YAML. A config struct whose field has different json and yaml names arrives empty without an error — the classic case is `json:",inline"` on a NAMED field (encoding/json nests it, YAML flattens it). Give every field of an input struct identical json/yaml tags, and add a JSON→YAML round-trip test (see `api/static_website_test.go`).

## Known Yandex Cloud quirks worth re-reading

Before deploying ANYTHING to Yandex Cloud, open `forge/CLAUDE.md` §"Yandex Cloud: a green deploy is not a running container". The headlines:
- A failed revision deploy is only a WARNING — `sc deploy` exits 0 with a container that has zero revisions. Always `yc serverless container revision list --container-id <id>` after.
- `PORT` is reserved. YAML merge keys cannot delete it; the YC env block must be written out in full.
- The Lambda base image is rejected (`public.ecr.aws/lambda/provided:al2023`). Use `public.ecr.aws/amazonlinux/amazonlinux:2023`.
- Nothing streams over HTTP on a Serverless Container. Streaming rides an API Gateway WebSocket.
- Pre-SC v2026.9.16, YC provider private keys LEAKED in deploy logs. Rotate if any deploy ran from an older SC.
- `yc config list` prints the operator's OAuth token to stdout — never run it.

## Local dev loop

```sh
# Build
cd plugin && GOFLAGS=-mod=mod go build -o /tmp/sc-plugin-yandex-dbg ./cmd/sc-plugin-yandex

# Point a smoke stack at the local binary
# .sc/stacks/.../server.yaml:
#   plugins:
#     yandex:
#       path: /tmp/sc-plugin-yandex-dbg
```

Tests:
```sh
cd plugin && GOFLAGS=-mod=mod go test ./... -count=1
cd api    && GOFLAGS=-mod=mod go test ./... -count=1
```

## Commit discipline

- DCO sign-off required (`git commit -s`).
- Do NOT add `Co-Authored-By` trailers.
- Do NOT paste a `Claude-Session:` or claude.ai link in commits or PR descriptions. This repo's rules match the forge umbrella's.
- Push to `main` triggers an auto-release workflow that cuts a GitHub Release tagged `vYYYY.MM.DD-<short-sha>` with linux/amd64, linux/arm64, darwin/arm64 binaries + `sha256sums.txt`.
