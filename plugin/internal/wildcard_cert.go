// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/samber/lo"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"

	sdkYandex "github.com/simple-container-com/pulumi-yandex/sdk/go/yandex"

	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

// WildcardCertOutput is what the parent stack exports for a wildcard cert.
// A `uses: [<name>]` consumer reads the cert id from the parent stack root
// via its ComputeProcessor.
type WildcardCertOutput struct {
	Certificate   *sdkYandex.CmCertificate // nil when adopting an operator-provided cert
	AcmeRecordset *sdkYandex.DnsRecordset  // nil when adopting
	Adopted       bool                     // true when using cfg.AdoptCertificateID
	Zone          *sdkYandex.LookupDnsZoneResult
	CertificateID sdk.StringOutput
}

// WildcardCert provisions (or adopts) a managed Let's Encrypt certificate
// covering <ZoneName> + *.<ZoneName>, writing the ACME DNS_CNAME challenge
// through the plugin's own yc-dns registrar in the same `sdk.Context`.
//
// Why the shape: a managed CM cert validates over DNS_CNAME at a single
// `_acme-challenge.<zone>` record, and that record holds exactly one target.
// A second managed cert in the same zone contends on that record and
// expires at ~90 days (see yandex.RegistrarConfig.CertificateID doc). This
// resource is the parent-infra answer: one cert per zone, every per-stack
// landing adopts its id.
//
// `managed.challenge_count: 1` is not a guess (see
// landing/infra/yc/main.tf:52-57): `<zone>` and `*.<zone>` validate against
// the SAME `_acme-challenge.<zone>` record, so CM emits one challenge for
// the pair. Setting it makes the count known at plan time; a mismatch (a
// third unrelated domain added) fails loudly rather than silently under-
// creating records.
func WildcardCert(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	cfg, ok := input.Descriptor.Config.Config.(*yandex.WildcardCertConfig)
	if !ok {
		return nil, errors.Errorf("invalid config type %T is not *yandex.WildcardCertConfig", input.Descriptor.Config.Config)
	}
	if err := api.ConvertAuth(cfg, &cfg.AccountConfig); err != nil {
		return nil, errors.Wrapf(err, "failed to convert auth config to yandex.AccountConfig")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	resName := input.ToResName(input.Descriptor.Name)
	providerName := fmt.Sprintf("%s-wildcard-cert-provider", resName)
	providerArgs := &sdkYandex.ProviderArgs{
		CloudId:  sdk.StringPtr(cfg.CloudID),
		FolderId: sdk.StringPtr(cfg.FolderID),
		RegionId: sdk.StringPtr(cfg.EffectiveRegion()),
		Zone:     sdk.StringPtr(cfg.EffectiveZone()),
	}
	if cfg.ServiceAccountKey != "" {
		providerArgs.ServiceAccountKeyFile = secretStringPtr(cfg.ServiceAccountKey)
	}
	provider, err := sdkYandex.NewProvider(ctx, providerName, providerArgs)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to init yandex provider for wildcard cert zone %q", cfg.ZoneName)
	}

	zone, zoneID, err := lookupZoneForWildcardCert(ctx, cfg, provider)
	if err != nil {
		return nil, err
	}
	params.Log.Info(ctx.Context(), "resolved yandex DNS zone %q (%s) for wildcard cert on %q", zone.Name, zoneID, cfg.ZoneName)

	out := &WildcardCertOutput{Zone: zone}

	if cfg.AdoptCertificateID != "" {
		params.Log.Info(ctx.Context(), "adopting operator-provided cert %q for zone %q — no managed cert will be provisioned",
			cfg.AdoptCertificateID, cfg.ZoneName)
		out.Adopted = true
		out.CertificateID = sdk.String(cfg.AdoptCertificateID).ToStringOutput()
		ctx.Export(toWildcardCertIDExport(resName), out.CertificateID)
		ctx.Export(toWildcardCertZoneIDExport(resName), sdk.String(zoneID))
		ctx.Export(toWildcardCertZoneNameExport(resName), sdk.String(cfg.ZoneName))
		return &api.ResourceOutput{Ref: out}, nil
	}

	trimmed := strings.TrimSuffix(cfg.ZoneName, ".")
	certName := fmt.Sprintf("%s-cert", strings.ReplaceAll(trimmed, ".", "-"))
	params.Log.Info(ctx.Context(), "provisioning managed Let's Encrypt cert for %q (+%s)", trimmed, "*."+trimmed)
	cert, err := sdkYandex.NewCmCertificate(ctx, certName, &sdkYandex.CmCertificateArgs{
		Name:     sdk.String(certName),
		FolderId: sdk.StringPtr(cfg.FolderID),
		Domains:  sdk.StringArray{sdk.String(trimmed), sdk.String("*." + trimmed)},
		Managed: &sdkYandex.CmCertificateManagedArgs{
			ChallengeType:  sdk.String("DNS_CNAME"),
			ChallengeCount: sdk.IntPtr(1),
		},
	}, sdk.Provider(provider))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create managed CM cert for %q", trimmed)
	}
	out.Certificate = cert

	// The one challenge is `_acme-challenge.<zone>` DNS_CNAME → <dns_value>.
	// Reading dns_name/dns_type from the SDK keeps us honest if CM ever
	// changes the record type (DNS_CNAME vs DNS_TXT behave the same here).
	//
	// Why `.Challenges.Index(0)`: the SDK's `CmCertificateChallengeArrayOutput`
	// is a typed array. `.Index(0)` returns a `CmCertificateChallengeOutput`
	// whose `DnsName()` / `DnsType()` / `DnsValue()` are `StringPtrOutput` —
	// exactly what sdk.StringArray wants after an `ApplyT` to dereference.
	challenge := cert.Challenges.Index(sdk.Int(0))
	acmeName := challenge.DnsName().Elem()
	acmeType := challenge.DnsType().Elem()
	acmeValue := challenge.DnsValue().Elem()

	acmeRecordset, err := sdkYandex.NewDnsRecordset(ctx, fmt.Sprintf("%s-acme-challenge", resName), &sdkYandex.DnsRecordsetArgs{
		ZoneId: sdk.String(zoneID),
		Name:   acmeName,
		Type:   acmeType,
		Ttl:    sdk.Int(60),
		Datas:  sdk.StringArray{acmeValue},
	}, sdk.Provider(provider))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create ACME challenge recordset for %q", trimmed)
	}
	out.AcmeRecordset = acmeRecordset

	// Wait for the cert to flip to ISSUED. The resource ApplyT below reads
	// `cert.Status` and asserts; sdk.All ensures the recordset is created
	// before the cert's status is read. Pulumi's engine then holds the
	// top-level stack's progress until this chain resolves — the ApplyT
	// cannot be dead-stripped because its output is used in the export
	// below (ctx.Export forces evaluation; see trap 15 in the extraction
	// playbook).
	certID := sdk.All(cert.Status, cert.ID(), acmeRecordset.ID()).ApplyT(func(args []any) (string, error) {
		status := args[0].(string)
		id := args[1].(sdk.ID)
		switch strings.ToUpper(status) {
		case "ISSUED", "RENEWING":
			params.Log.Info(ctx.Context(), "wildcard cert %q is %s (id=%s)", trimmed, status, id)
			return string(id), nil
		case "VALIDATING":
			// Pulumi plan time; the ApplyT runs after the cert is created,
			// which (per the SDK's resource lifecycle) is after YC's own
			// wait-for-validation step. So VALIDATING here means CM is
			// genuinely still validating — the DNS record likely has not
			// propagated. Return nothing yet; the next refresh will retry.
			return "", errors.Errorf("wildcard cert %q is still VALIDATING — check that the yc-dns registrar is live and that %s has propagated",
				trimmed, "_acme-challenge."+trimmed)
		default:
			return "", errors.Errorf("wildcard cert %q is in unexpected state %q (want ISSUED)", trimmed, status)
		}
	}).(sdk.StringOutput)
	out.CertificateID = certID

	ctx.Export(toWildcardCertIDExport(resName), certID)
	ctx.Export(toWildcardCertZoneIDExport(resName), sdk.String(zoneID))
	ctx.Export(toWildcardCertZoneNameExport(resName), sdk.String(cfg.ZoneName))

	return &api.ResourceOutput{Ref: out}, nil
}

// lookupZoneForWildcardCert is the registrar's lookupZone split out so the
// wildcard-cert driver can reuse it without constructing a registrar struct.
// Same shape, same guard: a resource name mismatch fails loudly rather than
// writing records into the wrong zone.
func lookupZoneForWildcardCert(ctx *sdk.Context, cfg *yandex.WildcardCertConfig, provider *sdkYandex.Provider) (*sdkYandex.LookupDnsZoneResult, string, error) {
	args := &sdkYandex.LookupDnsZoneArgs{FolderId: lo.ToPtr(cfg.FolderID)}
	if cfg.ZoneID != "" {
		args.DnsZoneId = lo.ToPtr(cfg.ZoneID)
	} else {
		args.Name = lo.ToPtr(cfg.EffectiveZoneResourceName())
	}

	zone, err := sdkYandex.LookupDnsZone(ctx, args, sdk.Provider(provider))
	if err != nil {
		return nil, "", errors.Wrapf(err, "failed to look up yandex DNS zone for wildcard cert on %q (searched folder %q by %s)",
			cfg.ZoneName, cfg.FolderID, lo.If(cfg.ZoneID != "", "id "+cfg.ZoneID).Else("name "+cfg.EffectiveZoneResourceName()))
	}
	if served := strings.TrimSuffix(zone.Zone, "."); !strings.EqualFold(served, strings.TrimSuffix(cfg.ZoneName, ".")) {
		return nil, "", errors.Errorf("yandex DNS zone %q (%s) serves %q, not the configured zoneName %q",
			zone.Name, lo.If(zone.DnsZoneId != "", zone.DnsZoneId).Else(zone.Id), served, cfg.ZoneName)
	}

	for _, candidate := range []string{zone.DnsZoneId, zone.Id} {
		if candidate != "" {
			return zone, candidate, nil
		}
	}
	return nil, "", errors.Errorf("yandex DNS zone %q for %q resolved without an id", zone.Name, cfg.ZoneName)
}

func toWildcardCertIDExport(resName string) string {
	return fmt.Sprintf("%s-wildcard-cert-id", resName)
}

func toWildcardCertZoneIDExport(resName string) string {
	return fmt.Sprintf("%s-wildcard-cert-zone-id", resName)
}

func toWildcardCertZoneNameExport(resName string) string {
	return fmt.Sprintf("%s-wildcard-cert-zone-name", resName)
}
