// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/MShekow/directory-checksum/directory_checksum"
	"github.com/pkg/errors"
	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	sdkYandex "github.com/simple-container-com/pulumi-yandex/sdk/go/yandex"
	"github.com/spf13/afero"

	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

// StaticWebsiteOutput is what the per-stack yc-static-website driver returns.
// OriginGroup and CdnResource are nil on the default bucket-TLS path and
// only populated when `cdn.enabled: true` opts into the per-stack CDN
// shape.
type StaticWebsiteOutput struct {
	Bucket         *sdkYandex.StorageBucket
	ServiceAccount *sdkYandex.IamServiceAccount
	AccessKey      *sdkYandex.IamServiceAccountStaticAccessKey
	OriginGroup    *sdkYandex.CdnOriginGroup
	CdnResource    *sdkYandex.CdnResource
	Record         *api.ResourceOutput
	Domain         string
	CDNEnabled     bool
}

// StaticWebsite provisions a per-stack static-site deployment. See
// api/static_website.go for the architecture rationale.
//
// The shared wildcard cert id is read from `params.ComputeContext`, which
// SC core populates from WildcardCertComputeProcessor's env-var emission
// on the consuming stack's `uses:` block. The driver picks the FIRST env
// var matching `YC_WILDCARD_CERT_*_ID` — operators with multiple wildcard
// certs set `certificateId: ${resource:<name>.certificateId}` explicitly.
func StaticWebsite(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	cfg, ok := input.Descriptor.Config.Config.(*yandex.StaticWebsiteInput)
	if !ok {
		return nil, errors.Errorf("failed to cast config to yandex.StaticWebsiteInput for %q: got %T", input.Descriptor.Type, input.Descriptor.Config.Config)
	}
	if err := api.ConvertAuth(cfg, &cfg.AccountConfig); err != nil {
		return nil, errors.Wrapf(err, "failed to convert auth config to yandex.AccountConfig for yc-static-website %q", input.Descriptor.Name)
	}
	if params.Provider == nil {
		return nil, errors.Errorf("provider must not be nil for yc-static-website %q in stack %q", input.Descriptor.Name, stack.Name)
	}
	effectiveDomain := cfg.EffectiveDomain()
	if params.Registrar == nil {
		return nil, errors.Errorf("registrar must not be nil for yc-static-website %q (zone of %q)", input.Descriptor.Name, effectiveDomain)
	}
	if effectiveDomain == "" {
		return nil, errors.Errorf("yc-static-website %q has empty domain", input.Descriptor.Name)
	}

	certIDStr := cfg.CertificateID
	// Resolve unresolved template interpolations or an operator-elided
	// `location:` via a live YC CM List call: list certificates in the
	// folder and match on domains==[zone,*.zone]. StackConfigStatic has no
	// `uses:` field (api PR to add one is pending), so client.yaml's
	// `location: ${resource:<name>.certificateId}` cannot reliably be
	// resolved at template parse time — the compute-processor extension
	// never fires on static stacks. Instead we auto-discover here, keeping
	// the fleet-pure "zero cert ids in config" invariant this plugin has
	// across the yc-dns registrar + yc-serverless-container paths.
	if certIDStr == "" || strings.HasPrefix(certIDStr, "${") {
		zone := cfg.EffectiveBaseDnsZone()
		if zone == "" {
			zone = effectiveDomain
		}
		if cfg.ServiceAccountKey == "" {
			return nil, errors.Errorf("yc-static-website %q: no `location:` cert id AND no service-account key for live CM lookup (zone %q)", input.Descriptor.Name, zone)
		}
		id, lookupErr := lookupCertByZoneLive(ctx.Context(), cfg.ServiceAccountKey, cfg.FolderID, zone)
		if lookupErr != nil {
			return nil, errors.Wrapf(lookupErr, "yc-static-website %q: live CM cert lookup for zone %q failed", input.Descriptor.Name, zone)
		}
		if id == "" {
			return nil, errors.Errorf("yc-static-website %q: no ISSUED wildcard cert covering [%q, %q] found in folder %q — either provision a yc-wildcard-cert for the zone on the parent, or set `location: <cert-id>` explicitly on the client stack",
				input.Descriptor.Name, strings.TrimSuffix(zone, "."), "*."+strings.TrimSuffix(zone, "."), cfg.FolderID)
		}
		params.Log.Info(ctx.Context(), "yc-static-website %q: resolved cert id %q from live CM lookup for zone %q", input.Descriptor.Name, id, zone)
		certIDStr = id
	}

	// Pulumi logical name (URN-anchored) — keeps slashes etc. for state
	// identity. YC-side resource names (SA, origin group) need sanitising:
	// YC rejects anything not matching `[a-z][-a-z0-9]*`.
	siteName := input.ToResName(input.Descriptor.Name)
	ycName := sanitiseYcName(strings.ReplaceAll(effectiveDomain, ".", "-"))
	domain := strings.TrimSuffix(effectiveDomain, ".")
	bucketName := domain // YC virtual-host website style: bucket named = FQDN

	opts := []sdk.ResourceOption{sdk.Provider(params.Provider)}

	// Service account + static key for the bundle sync.
	params.Log.Info(ctx.Context(), "configure service account for yc-static-website %q (bucket %q)", input.Descriptor.Name, bucketName)
	sa, err := sdkYandex.NewIamServiceAccount(ctx, fmt.Sprintf("%s-sa", siteName), &sdkYandex.IamServiceAccountArgs{
		Name:        sdk.String(fmt.Sprintf("%s-sa", ycName)),
		FolderId:    sdk.String(cfg.FolderID),
		Description: sdk.String(fmt.Sprintf("service account for static site %s (stack %s)", bucketName, stack.Name)),
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create service account for yc-static-website %q", input.Descriptor.Name)
	}

	roleBinding, err := sdkYandex.NewResourcemanagerFolderIamMember(ctx, fmt.Sprintf("%s-role", siteName), &sdkYandex.ResourcemanagerFolderIamMemberArgs{
		FolderId: sdk.String(cfg.FolderID),
		Role:     sdk.String("storage.editor"),
		Member:   sa.ID().ToStringOutput().ApplyT(func(id string) string { return fmt.Sprintf("serviceAccount:%s", id) }).(sdk.StringOutput),
	}, append(opts, sdk.DependsOn([]sdk.Resource{sa}))...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to bind storage.editor role for yc-static-website %q", input.Descriptor.Name)
	}

	accessKey, err := sdkYandex.NewIamServiceAccountStaticAccessKey(ctx, fmt.Sprintf("%s-key", siteName), &sdkYandex.IamServiceAccountStaticAccessKeyArgs{
		ServiceAccountId: sa.ID().ToStringOutput(),
		Description:      sdk.String(fmt.Sprintf("static key for yc-static-website %s", bucketName)),
	}, append(opts, sdk.DependsOn([]sdk.Resource{roleBinding}))...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create static access key for yc-static-website %q", input.Descriptor.Name)
	}

	bucketArgs := &sdkYandex.StorageBucketArgs{
		Bucket:       sdk.String(bucketName),
		FolderId:     sdk.String(cfg.FolderID),
		ForceDestroy: sdk.Bool(cfg.ForceDestroy),
		AnonymousAccessFlags: &sdkYandex.StorageBucketAnonymousAccessFlagsArgs{
			Read:       sdk.BoolPtr(true),
			List:       sdk.BoolPtr(false),
			ConfigRead: sdk.BoolPtr(false),
		},
		Website: &sdkYandex.StorageBucketWebsiteArgs{
			IndexDocument: sdk.StringPtr(cfg.EffectiveIndexPage()),
			ErrorDocument: sdk.StringPtr(cfg.EffectiveErrorPage()),
		},
	}
	// Bind the shared wildcard cert to the bucket so Object Storage terminates
	// its own TLS. Only on the bucket-TLS path — the CDN path owns the cert at
	// the CDN resource and the bucket should NOT also carry it (two owners of
	// the same binding would thrash on every deploy).
	//
	// Why native StorageBucket.Https and not an out-of-band setHttpsConfig
	// ApplyT: the pulumi-yandex provider's StorageBucket reads the bucket's
	// HTTPS config on refresh and any field the program omits is treated as
	// drift it must correct. Live-caught 2026-11-26 on simple-forge.ru's apex
	// migration — my setHttpsConfig call set certificateId=fpqu9ukb7q53rmraipgs,
	// Pulumi's next bucket-update stripped it the same deploy (the program
	// didn't declare `https:`, so Pulumi deleted it), and the apex served no
	// HTTPS until the operator re-POSTed setHttpsConfig manually. Declaring
	// Https on the resource makes it the program's property instead of drift.
	if !cfg.EffectiveCDNEnabled() {
		bucketArgs.Https = &sdkYandex.StorageBucketHttpsArgs{
			CertificateId: sdk.String(certIDStr),
		}
	}
	bucket, err := sdkYandex.NewStorageBucket(ctx, bucketName, bucketArgs, append(opts, sdk.DependsOn([]sdk.Resource{roleBinding}))...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create bucket %q for yc-static-website", bucketName)
	}

	// Bundle sync via `aws s3 sync` against YC's S3-compatible endpoint.
	dir, err := directory_checksum.ScanDirectory(cfg.EffectiveBundleDir(), afero.NewOsFs())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to scan bundleDir %q", cfg.EffectiveBundleDir())
	}
	checksums, err := dir.ComputeDirectoryChecksums()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to compute checksums for bundleDir %q", cfg.EffectiveBundleDir())
	}
	sum := md5.Sum([]byte(checksums))
	checksum := hex.EncodeToString(sum[:])
	params.Log.Info(ctx.Context(), "yc-static-website %q bundle checksum: %s (dir=%s)", input.Descriptor.Name, checksum, cfg.EffectiveBundleDir())

	cacheControl := yandex.DefaultCacheControl
	syncEnv := sdk.StringMap{
		"AWS_DEFAULT_REGION":    sdk.String(cfg.EffectiveRegion()),
		"AWS_ACCESS_KEY_ID":     accessKey.AccessKey,
		"AWS_SECRET_ACCESS_KEY": sdk.ToSecret(accessKey.SecretKey).(sdk.StringOutput),
	}
	syncCmd := sdk.Sprintf("aws s3 sync %s s3://%s --endpoint-url %s --delete --cache-control %q",
		cfg.EffectiveBundleDir(), bucket.Bucket, yandex.DefaultStorageEndpoint, cacheControl)
	_, err = local.NewCommand(ctx, fmt.Sprintf("%s-sync", siteName), &local.CommandArgs{
		Create:      syncCmd,
		Update:      syncCmd,
		Triggers:    sdk.ArrayInput(sdk.Array{sdk.String(checksum)}),
		Environment: syncEnv,
	}, sdk.DependsOn([]sdk.Resource{bucket, accessKey}))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to configure bundle sync for yc-static-website %q", input.Descriptor.Name)
	}

	// websiteEndpoint is the CNAME target in the bucket-TLS path AND the
	// CDN origin in the opt-in-CDN path. Must use .website.yandexcloud.net
	// (NOT .storage.yandexcloud.net) — YC Object Storage website hosting is
	// reachable at <bucket>.website.yandexcloud.net but auth.go:124 warns
	// .storage.yandexcloud.net virtual-host style is restricted.
	websiteEndpoint := fmt.Sprintf("%s.website.yandexcloud.net", bucketName)

	if !cfg.EffectiveCDNEnabled() {
		return staticWebsiteBucketTLS(ctx, stack, input, params, cfg, bucket, sa, accessKey, siteName, domain, bucketName, websiteEndpoint, certIDStr, opts)
	}

	// Opt-in CDN path — preserved verbatim. Each `cdn.enabled: true`
	// stack pays a non-refundable ~150 RUB/month CDN package on YC.
	originGroup, err := sdkYandex.NewCdnOriginGroup(ctx, fmt.Sprintf("%s-cdn-origin", siteName), &sdkYandex.CdnOriginGroupArgs{
		Name:         sdk.String(ycName),
		FolderId:     sdk.String(cfg.FolderID),
		ProviderType: sdk.StringPtr("ourcdn"),
		UseNext:      sdk.BoolPtr(true),
		Origins: sdkYandex.CdnOriginGroupOriginArray{
			&sdkYandex.CdnOriginGroupOriginArgs{
				Source: sdk.String(websiteEndpoint),
				Backup: sdk.BoolPtr(false),
			},
		},
	}, append(opts, sdk.DependsOn([]sdk.Resource{bucket}))...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create CDN origin group for yc-static-website %q", input.Descriptor.Name)
	}

	cdnOptions := &sdkYandex.CdnResourceOptionsArgs{
		CustomHostHeader:    sdk.StringPtr(websiteEndpoint),
		RedirectHttpToHttps: sdk.BoolPtr(cfg.EffectiveRedirectHttpToHttps()),
		GzipOn:              sdk.BoolPtr(cfg.EffectiveGzipOn()),
		IgnoreCookie:        sdk.BoolPtr(cfg.EffectiveIgnoreCookie()),
		EdgeCacheSettings:   sdk.IntPtr(cfg.EffectiveEdgeCacheSettings()),
	}
	cdn, err := sdkYandex.NewCdnResource(ctx, fmt.Sprintf("%s-cdn", siteName), &sdkYandex.CdnResourceArgs{
		Cname:          sdk.String(domain),
		FolderId:       sdk.StringPtr(cfg.FolderID),
		Active:         sdk.BoolPtr(true),
		ProviderType:   sdk.StringPtr("ourcdn"),
		OriginProtocol: sdk.StringPtr("http"),
		OriginGroupId: originGroup.ID().ToStringOutput().ApplyT(func(id string) *string {
			s := id
			return &s
		}).(sdk.StringPtrOutput),
		SslCertificate: &sdkYandex.CdnResourceSslCertificateArgs{
			Type:                 sdk.String("certificate_manager"),
			CertificateManagerId: sdk.StringPtr(certIDStr),
		},
		Options: cdnOptions,
	}, append(opts, sdk.DependsOn([]sdk.Resource{bucket, originGroup}))...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create CDN resource for yc-static-website %q", input.Descriptor.Name)
	}

	params.Log.Info(ctx.Context(), "configure DNS record for yc-static-website %q (domain %q) → CDN", input.Descriptor.Name, domain)
	record, err := params.Registrar.NewRecord(ctx, api.DnsRecord{
		Name:     domain,
		Type:     hostnameRecordType(domain, params.Registrar.MainDomain()),
		ValueOut: cdn.ProviderCname,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to provision DNS record for yc-static-website %q", input.Descriptor.Name)
	}

	ctx.Export(fmt.Sprintf("%s-static-website-domain", siteName), sdk.String(domain))
	ctx.Export(fmt.Sprintf("%s-static-website-bucket", siteName), bucket.Bucket)
	ctx.Export(fmt.Sprintf("%s-static-website-cdn-cname", siteName), cdn.ProviderCname)
	ctx.Export(fmt.Sprintf("%s-static-website-url", siteName), sdk.Sprintf("https://%s", domain))

	return &api.ResourceOutput{
		Ref: &StaticWebsiteOutput{
			Bucket:         bucket,
			ServiceAccount: sa,
			AccessKey:      accessKey,
			OriginGroup:    originGroup,
			CdnResource:    cdn,
			Record:         record,
			Domain:         domain,
			CDNEnabled:     true,
		},
	}, nil
}

// staticWebsiteBucketTLS implements the default (no-CDN) path: the bucket
// declares its own TLS binding on the `Https` field against the shared
// wildcard cert, and DNS points directly at <bucket>.website.yandexcloud.net.
//
// Why this path is the default: each `yandex_cdn_resource` carries a
// prepaid ~150 GB + 100M-requests monthly package, charged in full on
// CREATE day, re-billed on the 1st, non-refundable. For landing-page
// traffic (kilobytes to humans, inside YC's free 100 GB/mo egress tier)
// the CDN pays for nothing. For multi-tenant static sites it is also
// strictly incorrect: YC CDN's cache key is URI-only (no host-based
// keying exists in ResourceOptions, and disableCache is accepted but
// stored as nil), so one shared CDN namespace poisons `/` across
// tenants. See forge's `bucket-tls-addendum.md`.
//
// The HTTPS binding is set declaratively on the bucket resource via
// StorageBucketHttpsArgs (see where bucketArgs is built above). An
// earlier revision of this driver bound HTTPS out-of-band via a
// setHttpsConfig ApplyT; Pulumi's bucket refresh then saw the
// certificateId in the YC state, the program didn't declare it, and
// the next bucket-update stripped it — thrashing. Native Https field
// gives Pulumi ownership so it stops deleting what we just set.
//
//nolint:revive // long signature is deliberate — reuses caller's resolved values
func staticWebsiteBucketTLS(
	ctx *sdk.Context,
	_ api.Stack,
	input api.ResourceInput,
	params pApi.ProvisionParams,
	_ *yandex.StaticWebsiteInput,
	bucket *sdkYandex.StorageBucket,
	sa *sdkYandex.IamServiceAccount,
	accessKey *sdkYandex.IamServiceAccountStaticAccessKey,
	siteName, domain, bucketName, _, _ string,
	_ []sdk.ResourceOption,
) (*api.ResourceOutput, error) {
	resName := input.Descriptor.Name
	websiteEndpoint := BucketWebsiteEndpoint(bucketName)

	params.Log.Info(ctx.Context(), "configure DNS record for yc-static-website %q (domain %q) → bucket %s", resName, domain, websiteEndpoint)
	record, err := params.Registrar.NewRecord(ctx, api.DnsRecord{
		Name:     domain,
		Type:     hostnameRecordType(domain, params.Registrar.MainDomain()),
		ValueOut: sdk.String(websiteEndpoint).ToStringOutput(),
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to provision DNS record for yc-static-website %q (bucket-TLS)", input.Descriptor.Name)
	}

	ctx.Export(fmt.Sprintf("%s-static-website-domain", siteName), sdk.String(domain))
	ctx.Export(fmt.Sprintf("%s-static-website-bucket", siteName), bucket.Bucket)
	ctx.Export(fmt.Sprintf("%s-static-website-endpoint", siteName), sdk.String(websiteEndpoint))
	ctx.Export(fmt.Sprintf("%s-static-website-url", siteName), sdk.Sprintf("https://%s", domain))

	return &api.ResourceOutput{
		Ref: &StaticWebsiteOutput{
			Bucket:         bucket,
			ServiceAccount: sa,
			AccessKey:      accessKey,
			Record:         record,
			Domain:         domain,
			CDNEnabled:     false,
		},
	}, nil
}

// sanitiseYcName lowercases and strips non-[a-z0-9-] from a candidate
// YC-side resource name, so an FQDN-derived name is accepted by the
// yandex_iam_service_account / cdn-origin-group validators (both require
// `[a-z][-a-z0-9]*`). Leading digit → prefix with `n`; empty result →
// fall back to `sc-static`.
func sanitiseYcName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "sc-static"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "n" + out
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return strings.Trim(out, "-")
}

// pickWildcardCertID is retained for a future api change that plumbs
// client.yaml's `uses:` through to the static-site converter. For now,
// cfg.CertificateID is operator-populated via StackConfigStatic.Location.
//
//nolint:unused
func pickWildcardCertID(cc pApi.ComputeContext) (string, error) {
	if cc == nil {
		return "", errors.New("compute context is nil")
	}
	for _, v := range cc.EnvVariables() {
		if strings.HasPrefix(v.Name, "YC_WILDCARD_CERT_") && strings.HasSuffix(v.Name, "_ID") && v.Value != "" {
			return v.Value, nil
		}
	}
	return "", errors.New("no YC_WILDCARD_CERT_*_ID env var in compute context")
}
