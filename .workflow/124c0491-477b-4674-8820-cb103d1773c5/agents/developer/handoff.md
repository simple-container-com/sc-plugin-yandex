### 1. Commit + branch identity
- Commit `d82dba1d458654c202af55cb3a942f822109e336`, signed off (`-s`), with no Co-Authored-By and no Claude links.
- Pushed to `fix/dns-recordset-apex-multivalue-priority`, created from the current `origin/main` (`e311437`). I did not merge and did not push to main.
- **Draft PR:** https://github.com/simple-container-com/sc-plugin-yandex/pull/20
- The workflow-run branch for this handoff is `forge/workflow/3271cd71-3009-4e2a-b476-c44ff557c6ef/run/124c0491-477b-4674-8820-cb103d1773c5`.
- The code changes were already sitting uncommitted in the working tree, left by an earlier attempt. I reviewed them against the brief and committed them as they were; they passed the tests below.

### 2. Files touched
| Path | Lines added / removed | What changed |
|---|---|---|
| plugin/internal/registrar.go | +82/-3 | Added `recordsetResourceName`, `legacyRecordsetResourceName`, `recordsetFqdn`, `recordValues` and `startsWithInteger`. `NewRecord` now builds multi-value `Datas`, uses the typed resource name and adds `sdk.Aliases` for the old name. |
| plugin/cmd/sc-plugin-yandex/main.go | +6/-2 | `NewDnsRecord` builds an `api.DnsRecord` and copies `req.Priority` into `Priority` when it is set. |
| plugin/internal/registrar_test.go | +155/-0 | Five new tests, plus mock helpers `newTestRegistrar`, `recordsetDatas` and `recordsetInputs`. |

`fqdn()` and `guardZoneInfrastructureRecord` are unchanged.

### 3. Acceptance criteria verification
| AC# | Criterion | Status | Evidence |
|---|---|---|---|
| A | Resource name is `<name>-<type lower>-recordset`, old name kept as an alias | PASS | `recordsetResourceName` and `sdk.Aliases(... legacyRecordsetResourceName ...)` in `NewRecord`. Tests: `TestRecordsetResourceNames`, `TestNewRecord_KeepsLegacyNameAsAlias`. |
| B | `@` or empty gives `<zone>.`; in-zone names unchanged plus a dot; relative names get `<name>.<zone>.`; `TestFqdn` still passes | PASS | `recordsetFqdn` uses `api.DomainInZone`. `TestRecordsetFqdn` covers `@`, empty, in-zone, trailing dot, relative, and `notastrofit.ru` (which is not treated as in-zone). `TestFqdn` PASS. |
| C | Split only on `\n`, trim, skip blanks, one recordset, spaces in TXT kept | PASS | `TestRecordValues` and `TestNewRecord_MxAndTxtAtApexDoNotCollide` (MX `datas` = `["10 mx1.improvmx.com.", "20 mx2.improvmx.com."]`; TXT keeps its spaces). The protocol-change pros and cons are written up in the PR body, not implemented. |
| D | Priority copied from `req.Priority`; prepended for MX/SRV/URI only when the value has no leading integer; ignored for other types | PASS | `main.go` copies `req.Priority`. `TestRecordValues` covers prepend, no duplicate, SRV, TXT/CNAME ignoring priority, and nil priority. |
| E | `guardZoneInfrastructureRecord` untouched | PASS | The diff does not touch it. `TestGuardZoneInfrastructureRecord` PASS. |
| T1 | MX and TXT on `@` give two distinct resources, no duplicate URN | PASS | `TestNewRecord_MxAndTxtAtApexDoNotCollide`: `countOf` = 2, with names `@-mx-recordset` and `@-txt-recordset`. |
| T2 | `@` yields `<zone>.` | PASS | The same test checks `name` = `astrofit.ru.`. |
| T3 | Two MX values yield one recordset with two entries | PASS | The same test checks `datas`. |
| T4 | Old name verified as an alias | PASS | See the alias verification note below. |
| T5 | Existing tests still pass | PASS | `TestFqdn`, `TestGuardZoneInfrastructureRecord` and `TestHostnameRecordTypeUsesAnameAtApex` all PASS. |

**Alias verification:** `TestNewRecord_KeepsLegacyNameAsAlias` runs `NewRecord` for `app.astrofit.ru` CNAME under the Pulumi mocks. It reads the raw `RegisterResourceRequest` captured by `mocks.registerRPCOf(...)`. It asserts that the request name is `app.astrofit.ru-cname-recordset` and that the aliases (`GetAliases()` / `GetAliasURNs()`) contain `app.astrofit.ru-recordset`. This is checked at the RPC level only. I did not run it against real stack state, so alias adoption on a deployed stack has not been exercised live.

### 4. Tests run
- `cd plugin && GOFLAGS=-mod=mod go test ./... -count=1` gave:
  ```
  ok  github.com/simple-container-com/sc-plugin-yandex/plugin/cmd/sc-plugin-yandex  0.416s
  ok  github.com/simple-container-com/sc-plugin-yandex/plugin/internal              0.256s
  ```
- `cd plugin && GOFLAGS=-mod=mod go test ./internal/ -count=1 -v` gave 51 `--- PASS` and 0 `--- FAIL`. This includes the five new tests and the three existing tests named in the brief.
- `cd api && GOFLAGS=-mod=mod go test ./... -count=1` gave `ok  github.com/simple-container-com/sc-plugin-yandex/api  0.046s`.
- A `go vet` run timed out after 60s on the cold build cache. I did not rerun it, so vet is unverified.
- `gofmt -l` lists `bucket.go`, `compute_proc.go` and `docker.go`. I did not touch those files, and my three files are not listed.
- I did not run `sc provision` or any live preview, as instructed.

### What remains and risks
- **Operator, after review:** run `sc provision --preview` on astrofit.ru. Confirm the existing recordsets show as a rename or alias with no replace or delete, and that the MX and TXT records are created.
- **Aliases:** they only protect stacks whose current state holds `<name>-recordset`. A recordset created under some other name is not covered.
- **Value packing:** a literal TXT value that contains a newline is now split into several `datas` entries. That is the intended `value: |` behaviour, but it is a change for anyone who relied on a single multi-line value.
- **Output values:** Pulumi Output values (from in-process callers such as `ProvisionDomainForEndpoint`) stay single-valued, and priority is not applied to them.
- **Protocol:** adding a repeated `values` field would need changes in `simple-container-com/api` and an SC release. The pros and cons are in the PR body and I did not implement it.
- **PR state:** the PR is a draft. Its body was posted through `gh`, because the generic `api_call` had no GitHub credentials for creating the PR.

**Verdict:** signoff