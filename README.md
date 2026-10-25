# sc-plugin-yandex

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Yandex Cloud driver plugin for [Simple Container](https://github.com/simple-container-com/api). Extracted from the SC monolith's `pkg/clouds/{yandex,pulumi/yandex}` subtrees as the **second out-of-tree sc-plugin** per the [`sc-plugin-architecture`](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-10-24/sc-plugin-architecture) design.

A plugin binary lets SC drive Yandex Cloud resources without SC core having to vendor the (~110 MB) `pulumi-yandex` SDK. The plugin talks to SC core's running Pulumi engine over the SC plugin bus, registers resources against the SAME monitor, and shows up in state exactly as the in-tree driver did.

## What this plugin does

**V1 (now):** DNS registrar for `type: yc-dns`.
- Zone lookup — resolves the stack's `zoneName` to a YC DNS zone id.
- DNS records — bulk at provision time via `dnsRecords:`; per-record on demand via `NewRecord`.
- Override-host rule — API Gateway spec in the registrar's folder that forwards to a different host, used by k3s-cloudrun / serverless-container domains.
- Domain-for-endpoint — proxied record + override rule, in one call.

**V2+ (follow-ups):**
- `pulumi:providers:yandex` (Provider RPC).
- `yc-bucket` (Provision + ComputeProcess) — Object Storage.
- `yc-serverless-container` (Provision) — Serverless Containers.
- `yc-object-storage` state storage (InitStateStore).

## Install

### Download a release binary

Pre-built binaries for each platform are attached to each GitHub Release:

| GOOS   | GOARCH | Binary filename                      |
|--------|--------|--------------------------------------|
| linux  | amd64  | `sc-plugin-yandex-linux-amd64`       |
| linux  | arm64  | `sc-plugin-yandex-linux-arm64`       |
| darwin | arm64  | `sc-plugin-yandex-darwin-arm64`      |

darwin/amd64 (Intel Macs) and Windows are intentionally unsupported.

### Let SC fetch it for you

SC core v2026.10+ resolves plugins directly from a GitHub Release — specify a `source:` block in `plugins:` instead of hand-placing a binary. SC caches the binary under `~/.sc/plugins/<repo>/<version>/` and sha256-verifies on every spawn.

## Configure in `server.yaml`

```yaml
schemaVersion: 1.0

provisioner:
  type: pulumi
  config:
    organization: your-org
    state-storage: { type: s3-bucket, config: {...} }
    secrets-provider: { type: awskms, config: {...} }

plugins:
  yandex:
    source:
      type: github-release
      repository: simple-container-com/sc-plugin-yandex
      version: v2026.10.25-<short-sha>   # pin a specific release

resources:
  registrar:
    type: yc-dns
    config:
      credentials: ${secret:YC_SA_KEY}       # service-account JSON key
      folderId:    ${env:YC_FOLDER_ID}
      zoneName:    your-zone.example.com
      certificateId: ${env:YC_CERT_ID}       # optional — for ProvisionDomainForEndpoint

  resources:
    staging:
      resources: {}
```

The service-account key is a Yandex Cloud SA JSON key (not an OAuth token). The SA needs:
- `dns.editor` on the folder owning the DNS zone (for record / zone operations).
- `serverless.apigateways.editor` on the folder (for ProvisionDomainForEndpoint).
- `cert-manager.certificates.downloader` if `certificateId` is set.

**Yandex Cloud caveats worth re-reading before deploying anything** — SC's `forge/CLAUDE.md` has a dedicated section titled "Yandex Cloud: a green deploy is not a running container". The headline rules:
- A **failed revision deploy is only a WARNING** — assert the revision exists after any serverless-container deploy with `yc serverless container revision list`.
- `PORT` is reserved — a revision that sets it is refused.
- The Lambda base image is rejected — use `public.ecr.aws/amazonlinux/amazonlinux:2023`.
- **Nothing streams over HTTP** — streaming rides an API Gateway WebSocket.
- Older SC versions LEAKED the SA private key in deploy logs — rotate if you ever shipped from a workflow pinned below SC `v2026.9.16`.

## Repository layout

```
sc-plugin-yandex/
  api/                              # Go module — config structs for SC core
    auth.go, bucket.go, container.go, registrar.go, yandex.go  # lifted configs
    init.go                         # api.RegisterProviderConfig + ProvisionerFieldConfig
    go.mod                          # imports: SC's pkg/api only. NO pulumi-yandex.
  plugin/                           # Go module — the plugin binary
    cmd/sc-plugin-yandex/
      main.go                       # SCCloudPluginServer implementation (v1: registrar)
    internal/
      registrar.go                  # lifted driver code — the Pulumi DNS registrar
      bucket.go, serverless_container.go, provider.go, compute_proc.go, docker.go, credentials.go
                                    # lifted but NOT YET wired through the plugin bus (v2+)
    go.mod                          # imports: pulumi-yandex SDK + SC plugin runtime
  LICENSE                           # MIT
  CODEOWNERS
  README.md                         # this file
  .github/workflows/release.yml     # auto-release on push to main (CalVer)
```

**Two Go modules.** SC core imports `/api` at build time to register the yandex / yc-* types in its config-tier map and generate schemas; the `/plugin` module and its heavy `pulumi-yandex` SDK dependency never cross that boundary. Same pattern as `sc-plugin-cloudflare`, same decision point in the design doc.

## Compatibility

| sc-plugin-yandex | SC core (`simple-container-com/api`) | Pulumi SDK | pulumi-yandex |
|---|---|---|---|
| v0.0.x (initial) | main after sc-plugin/surface-complete lands | sdk/v3 v3.245.0 | v1.1.0 |

The plugin binary vendors its own `pulumi-yandex` SDK; SC core's version doesn't matter after this plugin is pinned. SC core's `pulumi/sdk/v3` version MUST match the plugin's — a mismatch breaks the shared monitor gRPC protocol.

## License

[MIT](./LICENSE).
