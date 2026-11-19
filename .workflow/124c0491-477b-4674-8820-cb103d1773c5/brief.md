REPO: simple-container-com/sc-plugin-yandex. Read CLAUDE.md first and follow it. Branch: fix/dns-recordset-apex-multivalue-priority from current origin/main (a local branch with the same name exists on the operator's machine but is not pushed; create it fresh). Open a PR; DO NOT merge, DO NOT push to main. Commits: git commit -s (DCO), no Co-Authored-By, no claude.ai / Claude-Session links in commits or PR.

PROGRESS VISIBILITY: a previous run of this brief sat 2.5h with zero visible output. Push the branch after your first commit (even WIP) and open the PR early (draft is fine) so progress is observable. If you hit a blocker (cannot clone, cannot run go, no push access), STOP and report it in the handoff immediately instead of retrying silently.

GOAL: fix the yc-dns registrar so astrofit.ru can have at apex: MX 10 mx1.improvmx.com., MX 20 mx2.improvmx.com., TXT "v=spf1 include:spf.improvmx.com ~all". Currently `sc provision --preview` fails with "Duplicate resource URN ...DnsRecordset::@-recordset".
Files: plugin/internal/registrar.go (NewRecord, fqdn), plugin/cmd/sc-plugin-yandex/main.go (NewDnsRecord, ~line 218).

PROBLEMS: (1) resource name `<name>-recordset` has no type, so MX and TXT on the same name collide. (2) fqdn() just appends a dot: "@" becomes "@.". (3) Datas is sdk.StringArray{value}: a single value; SC core calls NewDnsRecord one record at a time so the plugin cannot group records. (4) NewDnsRecordRequest.Priority (*int32) is not read in main.go nor copied into api.DnsRecord.

TASKS:
A. Resource name: `<name>-<type lowercase>-recordset`. BACKWARD COMPAT IS MANDATORY: already-deployed fleet stacks must not be recreated. Use sdk.Aliases([]sdk.Alias{{Name: sdk.String("<name>-recordset")}}) (old name) on the NewDnsRecordset resource.
B. Absolute name via zone: "@" or "" -> "<zone>."; name already inside the zone -> as before (+ trailing dot); relative name -> "<name>.<zone>.". Existing callers pass full domains (endpoint.Domain, e.g. app.astrofit.ru) and that must not change. Use api.DomainInZone(name, zone) (already used in main.go) for the in-zone check. Update fqdn() or add a new function; keep TestFqdn expectations passing.
C. Multi-value: value may contain several values separated by newline (YAML `value: |`). Split ONLY on \n, trim whitespace, skip empty lines, put all in Datas of ONE recordset. Spaces inside a value (TXT) must stay intact. Plugin-internal decision, no protocol change. If you think a repeated field in the protocol is right, do NOT do it silently: describe pros/cons in the PR (it touches simple-container-com/api and the SC release).
D. Priority: pass req.Priority from NewDnsRecord into api.DnsRecord.Priority. For MX/SRV/URI, if Priority is set and the value does not start with an integer, prepend it to every value ("10 mx1.improvmx.com."). If the value already starts with a number, do not duplicate. Ignore Priority for other types.
E. Do NOT touch guardZoneInfrastructureRecord (NS, SOA, _acme-challenge. stay forbidden).

TESTS (plugin/internal/registrar_test.go; pattern with Pulumi mocks in bucket_test.go: newBucketMocks + pulumi.RunErr + WithMocks; provider via bucketProvisionParams):
- MX and TXT on "@" create two distinct resources, no Duplicate URN.
- "@" yields name "<zone>.".
- Two MX via value with \n yield ONE recordset with two entries in datas.
- Priority is prepended to MX but not duplicated if the number is already present.
- The old resource name is kept as an alias (assert from the mock inputs / RegisterRPC).
- Existing behaviour intact: TestFqdn, TestGuardZoneInfrastructureRecord, TestHostnameRecordTypeUsesAnameAtApex.
Run: cd plugin && GOFLAGS=-mod=mod go test ./... -count=1 ; cd api && GOFLAGS=-mod=mod go test ./... -count=1  (Go at /snap/bin/go).

HANDOFF to reviewer: PR link, branch, commits, summary, exact test results (counts), and an explicit note on how the alias is verified. Do NOT run sc provision / live preview: the operator does that locally after review. Do not print secrets; never run `yc config list`.