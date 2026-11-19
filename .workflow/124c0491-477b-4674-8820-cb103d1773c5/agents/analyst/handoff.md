### 1. Commit + branch identity
No code mutations in this turn. This is a read-only review of PR #20, so I did not re-run the tests.
- PR: https://github.com/simple-container-com/sc-plugin-yandex/pull/20. It is open, and the Developer reports it as a draft.
- PR branch: `fix/dns-recordset-apex-multivalue-priority`, head commit `d82dba1d458654c202af55cb3a942f822109e336`.
- That commit's message has a `Signed-off-by:` trailer and no Co-Authored-By or Claude link. The PR body has no Claude link either.
- Workflow-run branch for this handoff: `forge/workflow/3271cd71-3009-4e2a-b476-c44ff557c6ef/run/124c0491-477b-4674-8820-cb103d1773c5`.
- The run prompt asks for a capability matrix, but the brief is this DNS fix. I reviewed the brief's work and included a short matrix of the capabilities it touches.

### 2. Files touched
This turn touched no files. The table lists the PR's files, which I reviewed.

| Path | Lines added / removed | What changed |
|---|---|---|
| (this turn) | 0/0 | No file changes by the Analyst. |
| plugin/internal/registrar.go (PR, reviewed) | +81/-4 | Typed resource name plus legacy alias, `recordsetFqdn`, `recordValues`, `startsWithInteger`, and multi-value `Datas`. |
| plugin/cmd/sc-plugin-yandex/main.go (PR, reviewed) | +6/-2 | `NewDnsRecord` copies `req.Priority` into `api.DnsRecord.Priority`. |
| plugin/internal/registrar_test.go (PR, reviewed) | +155/-0 | Five new tests plus mock helpers. |

The PR API reports registrar.go as +81/-4. The Developer's handoff said +82/-3.

### 3. Acceptance criteria verification
I checked each criterion against the PR diff. I did not run the tests myself.

| AC# | Criterion | Status | Evidence |
|---|---|---|---|
| A | Typed resource name, old name kept as an alias | PASS (code) / UNVERIFIED (live) | `recordsetResourceName` and `sdk.Aliases([]sdk.Alias{{Name: legacy}})` are in the diff. Alias adoption on real stack state is not exercised. |
| B | `@`/empty gives `<zone>.`; in-zone names unchanged plus a dot; relative names get `<name>.<zone>.`; `TestFqdn` intact | PASS | `recordsetFqdn` uses `api.DomainInZone`. `fqdn()` is not in the diff. `TestRecordsetFqdn` covers the cases, including the `notastrofit.ru` negative. |
| C | Split only on `\n`, trim, skip blanks, one recordset, spaces in TXT kept | PASS | `recordValues` and its test. The protocol pros and cons are in the PR body, and nothing was implemented. |
| D | Priority copied; prepended for MX/SRV/URI only when no leading integer; ignored for other types | PASS | main.go copies `req.Priority` only when it is non-nil. `recordValues` and `TestRecordValues` cover the cases. |
| E | `guardZoneInfrastructureRecord` untouched | PASS | It is not in the diff. |
| Tests | MX+TXT at `@` give two distinct resources; `@` gives `<zone>.`; two MX give one recordset with two datas; priority not duplicated; alias asserted | PASS (as reported) | The tests are in the diff. The counts (51 PASS in `internal`, `api` ok) are the Developer's report and I did not re-run them. |
| Process | Branch from origin/main, PR open, no merge, DCO, no Claude links | PASS | The PR targets `main` and is not merged. The commit has a sign-off trailer. |

### 4. Tests run
I ran no tests. The Developer's reported results:
- `cd plugin && GOFLAGS=-mod=mod go test ./... -count=1` gave `ok` for both packages.
- `cd api && GOFLAGS=-mod=mod go test ./... -count=1` gave `ok`.
- `go vet` timed out and was not re-run, so it is unverified.

### Capability matrix (scope: what this change touches)
Scope names are inferred, because the brief names none. The Forge scope for opening a PR is unverified.

| Capability | Control scope | Status |
|---|---|---|
| Distinct recordsets for MX and TXT on one name | none (plugin code); fleet effect needs an SC release | Confirmed in mocks. Not confirmed on a real stack. |
| Apex `@` resolves to `<zone>.` | none | Confirmed in a unit test. |
| Multi-value `value: \|` packed into one recordset | none (plugin-internal) | Confirmed in a mock. |
| Priority from `req.Priority` | none; the request field already exists | Confirmed in unit tests of `recordValues`. The `NewDnsRecord` wiring in main.go has no direct test. |
| Alias protects deployed stacks | none | Documented and asserted at the RPC level only. Live adoption is unverified. |
| Typed multi-value protocol field (repeated `values`) | needs changes in `simple-container-com/api` plus an SC release | Documented only; deliberately not done. |
| Creating the PR through the generic `api_call` | `github:write` | The Developer says `api_call` had no GitHub credentials for this. The PR was made through `gh`. |

### Findings and risks, in priority order
1. **Alias conflict risk (highest, unverified).** Every recordset for a name now claims the same legacy alias `<name>-recordset`. Two things could go wrong:
   - **Both claim the old resource.** If a name already has a deployed `@-recordset` and the config now also adds a second type at that name (the astrofit.ru case), both new resources alias to one old resource. I could not verify how Pulumi's engine handles two new resources claiming one old URN: it may error, or only one may adopt it.
   - **The wrong type adopts it.** The type is likely ForceNew on the YC recordset, so the wrong one adopting the old state could replace the existing record.
   - **Suggested fix:** scope the alias per type. Only the type that owned the old record should carry it, which can't be known inside the plugin. The alternative is an operator check on the `--preview` output.
2. **`--preview` is the real acceptance test.** The operator should confirm two things on astrofit.ru:
   - Existing recordsets show as alias or rename, with no delete or replace.
   - MX and TXT are created with no Duplicate URN error.
3. **Newline split is a behaviour change.** A TXT value with a literal newline now becomes several `datas` entries. This is intended and is noted in the PR.
4. **Pulumi Output values stay single-valued with no priority applied.** This affects the in-process callers such as `ProvisionDomainForEndpoint`.
5. **Wiring gap.** main.go's priority copy has no test of its own. Only `recordValues` is tested.
6. **Hygiene.** `go vet` is unverified. `gofmt -l` lists `bucket.go`, `compute_proc.go` and `docker.go`; none of them are in this PR.
7. **Docs.** The repo's CLAUDE.md has no V1 scope entry for multi-value or apex records. An update is optional.
8. **Contradiction between docs and reality.** The Developer says no GitHub credentials were available through `api_call`, yet the PR was created through `gh`. That means the Forge `api_call` grant for `api.github.com` was missing or unusable. It is an environment risk, not a code risk.

### Prioritized next steps
1. The operator runs `sc provision --preview` locally against astrofit.ru and checks the alias behaviour in finding 1.
2. If the preview shows conflicts, change the alias to apply only to the type that existed before. Alternatively, add a one-time state migration.
3. Add a small test of the `NewDnsRecord` priority wiring in main.go, and re-run `go vet` with a warm cache.
4. Decide on the repeated `values` protocol field as a separate follow-up. It needs a release in `simple-container-com/api` and SC.
5. Mark the PR ready for review after step 1. Do not merge it before then.

- **Empty-PR-OK:** yes. This was a review-only turn with no source changes, and the code lives on PR #20's own branch. The evidence is the PR #20 diff and the commit metadata, read through the repository tools. I did not re-run the tests.

**Verdict:** signoff