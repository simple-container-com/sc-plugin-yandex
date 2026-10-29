// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/samber/lo"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	sdkYandex "github.com/simple-container-com/pulumi-yandex/sdk/go/yandex"
	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

// StaticWebsiteOutput is every resource a static site registers, and the upload
// result. The plugin awaits all of it before its RPC returns.
type StaticWebsiteOutput struct {
	Bucket          *sdkYandex.StorageBucket
	ServiceAccount  *sdkYandex.IamServiceAccount
	AccessKey       *sdkYandex.IamServiceAccountStaticAccessKey
	UploaderBinding *sdkYandex.StorageBucketIamBinding
	// Certificate and ChallengeRecord exist only for a managed (Let's Encrypt)
	// certificate; an adopted one is looked up, not created.
	Certificate     *sdkYandex.CmCertificate
	ChallengeRecord *sdkYandex.DnsRecordset
	Record          *sdkYandex.DnsRecordset
	// Upload resolves to a summary once the bundle is in the bucket.
	Upload sdk.StringOutput
}

// Outputs lists what to await.
func (o *StaticWebsiteOutput) Outputs() []sdk.Output {
	outs := []sdk.Output{
		o.Bucket.ID(), o.ServiceAccount.ID(), o.AccessKey.ID(), o.UploaderBinding.ID(), o.Record.ID(), o.Upload,
	}
	if o.Certificate != nil {
		outs = append(outs, o.Certificate.ID())
	}
	if o.ChallengeRecord != nil {
		outs = append(outs, o.ChallengeRecord.ID())
	}
	return outs
}

// uploaderRole is what the site's own uploader identity holds, on its bucket only:
// put, list and delete objects. Narrower than the folder-wide storage.editor a
// yc-bucket's account gets, because this key exists for exactly one bucket.
const uploaderRole = "storage.editor"

// StaticWebsite provisions a `yc-static-website` stack: the website bucket named
// after the domain, a Certificate Manager certificate on it, the CNAME, and the
// bundle upload. See yandex.TemplateTypeYandexStaticWebsite for why it is shaped
// this way.
func StaticWebsite(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	cfg, ok := input.Descriptor.Config.Config.(*yandex.StaticWebsiteInput)
	if !ok {
		return nil, errors.Errorf("failed to cast config to *yandex.StaticWebsiteInput for %q", input.Descriptor.Type)
	}
	if params.Provider == nil {
		return nil, errors.Errorf("provider must not be nil for static website %q", stack.Name)
	}
	if err := api.ConvertAuth(cfg, &cfg.AccountConfig); err != nil {
		return nil, errors.Wrapf(err, "failed to convert auth config for static website %q", stack.Name)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.FolderID == "" {
		return nil, errors.Errorf("folderId must be set for static website %q", stack.Name)
	}
	log := params.Log
	opts := []sdk.ResourceOption{sdk.Provider(params.Provider)}
	domain := cfg.BucketName()
	resName := siteResourceName(domain)

	zoneCfg := cfg.ZoneRegistrarConfig()
	if err := zoneCfg.Validate(); err != nil {
		return nil, err
	}
	zone, err := lookupZone(ctx, zoneCfg, params.Provider)
	if err != nil {
		return nil, err
	}
	zoneID, err := zoneIDOf(zone, zoneCfg)
	if err != nil {
		return nil, err
	}
	ttl := sdk.Int(zoneCfg.EffectiveRecordTtl())

	out := &StaticWebsiteOutput{}

	certID, err := siteCertificate(ctx, cfg, domain, resName, zoneID, ttl, out, params.Provider)
	if err != nil {
		return nil, err
	}

	sa, err := sdkYandex.NewIamServiceAccount(ctx, resName+"-uploader", &sdkYandex.IamServiceAccountArgs{
		Name:        sdk.String(serviceAccountName(resName + "-uploader")),
		FolderId:    sdk.String(cfg.FolderID),
		Description: sdk.String(fmt.Sprintf("uploads the static site %s (stack %s)", domain, stack.Name)),
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create uploader service account for %q", domain)
	}
	out.ServiceAccount = sa

	key, err := sdkYandex.NewIamServiceAccountStaticAccessKey(ctx, resName+"-uploader-key", &sdkYandex.IamServiceAccountStaticAccessKeyArgs{
		ServiceAccountId: sa.ID().ToStringOutput(),
		Description:      sdk.String(fmt.Sprintf("static key uploading %s", domain)),
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create uploader key for %q", domain)
	}
	out.AccessKey = key

	website := &sdkYandex.StorageBucketWebsiteArgs{IndexDocument: sdk.String(cfg.EffectiveIndexDocument())}
	if errDoc := cfg.EffectiveErrorDocument(); errDoc != "" {
		website.ErrorDocument = sdk.String(errDoc)
	}
	bucketArgs := &sdkYandex.StorageBucketArgs{
		Bucket:   sdk.String(domain),
		FolderId: sdk.String(cfg.FolderID),
		// Everything in the bucket is build output the next deploy re-uploads.
		ForceDestroy: sdk.Bool(!cfg.KeepOnDestroy),
		// Public read of objects — and only that: listing the bucket and reading
		// its settings stay private.
		AnonymousAccessFlags: &sdkYandex.StorageBucketAnonymousAccessFlagsArgs{
			Read:       sdk.Bool(true),
			List:       sdk.Bool(false),
			ConfigRead: sdk.Bool(false),
		},
		Website: website,
		Https:   &sdkYandex.StorageBucketHttpsArgs{CertificateId: certID},
	}
	if cfg.MaxSize > 0 {
		bucketArgs.MaxSize = sdk.Int(cfg.MaxSize)
	}
	log.Info(ctx.Context(), "configure yandex static website bucket %q for stack %q", domain, stack.Name)
	bucket, err := sdkYandex.NewStorageBucket(ctx, resName, bucketArgs, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create website bucket %q", domain)
	}
	out.Bucket = bucket

	binding, err := sdkYandex.NewStorageBucketIamBinding(ctx, resName+"-uploader-binding", &sdkYandex.StorageBucketIamBindingArgs{
		Bucket:  bucket.Bucket,
		Role:    sdk.String(uploaderRole),
		Members: sdk.StringArray{sa.ID().ToStringOutput().ApplyT(func(id string) string { return "serviceAccount:" + id }).(sdk.StringOutput)},
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to bind uploader role on bucket %q", domain)
	}
	out.UploaderBinding = binding

	record, err := sdkYandex.NewDnsRecordset(ctx, resName+"-cname", &sdkYandex.DnsRecordsetArgs{
		ZoneId: sdk.String(zoneID),
		Name:   sdk.String(fqdn(domain)),
		Type:   sdk.String("CNAME"),
		Datas:  sdk.StringArray{sdk.String(cfg.WebsiteEndpoint())},
		Ttl:    ttl,
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create CNAME for %q", domain)
	}
	out.Record = record

	out.Upload = sdk.All(bucket.Bucket, key.AccessKey, key.SecretKey, binding.ID()).ApplyT(func(args []any) (string, error) {
		// The engine creates nothing during a preview, so nothing outside it may
		// either. On an existing site every input above is already known in a
		// preview, so without this guard `sc deploy -P` would publish the bundle.
		if ctx.DryRun() {
			return "upload skipped (preview)", nil
		}
		bucketName, accessKey, secretKey := args[0].(string), args[1].(string), args[2].(string)
		log.Info(ctx.Context(), "uploading %q to bucket %q", cfg.EffectiveBundleDir(), bucketName)
		res, err := syncSite(ctx.Context(), newSiteS3Client(cfg.EffectiveRegion(), accessKey, secretKey), bucketName, cfg)
		if err != nil {
			return "", errors.Wrapf(err, "failed to upload static site %q", domain)
		}
		msg := fmt.Sprintf("uploaded %d objects, deleted %d stale", res.Uploaded, res.Deleted)
		log.Info(ctx.Context(), "static site %q: %s", domain, msg)
		return msg, nil
	}).(sdk.StringOutput)

	ctx.Export(fmt.Sprintf("%s-website-endpoint", resName), sdk.String(strings.TrimSuffix(cfg.WebsiteEndpoint(), ".")))
	ctx.Export(fmt.Sprintf("%s-url", resName), sdk.String("https://"+domain))

	return &api.ResourceOutput{Ref: out}, nil
}

// siteCertificate resolves the certificate id the bucket serves HTTPS with: the
// configured id, a lookup by name, or a new Let's Encrypt certificate validated
// through a DNS-CNAME challenge in the site's zone.
func siteCertificate(ctx *sdk.Context, cfg *yandex.StaticWebsiteInput, domain, resName, zoneID string, ttl sdk.IntInput, out *StaticWebsiteOutput, provider sdk.ProviderResource) (sdk.StringInput, error) {
	opts := []sdk.ResourceOption{sdk.Provider(provider)}
	switch {
	case cfg.CertificateID != "":
		return sdk.String(cfg.CertificateID), nil
	case cfg.CertificateName != "":
		cert, err := sdkYandex.LookupCmCertificate(ctx, &sdkYandex.LookupCmCertificateArgs{
			Name:     lo.ToPtr(cfg.CertificateName),
			FolderId: lo.ToPtr(cfg.FolderID),
		}, sdk.Provider(provider))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to look up certificate %q in folder %q", cfg.CertificateName, cfg.FolderID)
		}
		if cert.Status != "" && cert.Status != "ISSUED" {
			return nil, errors.Errorf("certificate %q is %s, not ISSUED — the bucket cannot serve HTTPS with it", cfg.CertificateName, cert.Status)
		}
		return sdk.String(cert.Id), nil
	}

	cert, err := sdkYandex.NewCmCertificate(ctx, resName+"-cert", &sdkYandex.CmCertificateArgs{
		Name:     sdk.String(certificateName(resName)),
		FolderId: sdk.String(cfg.FolderID),
		Domains:  sdk.StringArray{sdk.String(domain)},
		Managed:  &sdkYandex.CmCertificateManagedArgs{ChallengeType: sdk.String("DNS_CNAME")},
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to request a certificate for %q", domain)
	}
	out.Certificate = cert
	challenge := cert.Challenges.Index(sdk.Int(0))
	record, err := sdkYandex.NewDnsRecordset(ctx, resName+"-acme", &sdkYandex.DnsRecordsetArgs{
		ZoneId: sdk.String(zoneID),
		Name:   challenge.DnsName().Elem(),
		Type:   challenge.DnsType().Elem(),
		Datas:  sdk.StringArray{challenge.DnsValue().Elem()},
		Ttl:    ttl,
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create the certificate challenge record for %q", domain)
	}
	out.ChallengeRecord = record

	// Attaching an unvalidated certificate to a bucket is refused, so the id the
	// bucket gets is read back through a lookup that blocks until Let's Encrypt
	// has validated the challenge. Deriving the lookup's input from the record's id
	// is what orders it after the record exists.
	validatedID := sdk.All(cert.ID(), record.ID()).ApplyT(func(args []any) string {
		return string(args[0].(sdk.ID))
	}).(sdk.StringOutput)
	validated := sdkYandex.LookupCmCertificateOutput(ctx, sdkYandex.LookupCmCertificateOutputArgs{
		CertificateId:  validatedID,
		WaitValidation: sdk.Bool(true),
	}, sdk.Provider(provider))
	return validated.Id(), nil
}

var nonNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// siteResourceName is the Pulumi resource name stem for a domain:
// `twopeaks.simple-forge.ru` → `twopeaks-simple-forge-ru`.
func siteResourceName(domain string) string {
	return strings.Trim(nonNameChars.ReplaceAllString(strings.ReplaceAll(strings.ToLower(domain), ".", "-"), "-"), "-")
}

// serviceAccountName fits YC's IAM name rule: 3-63 characters, lowercase letters,
// digits and hyphens, starting with a letter and not ending with a hyphen.
func serviceAccountName(name string) string {
	return yandexName(name, "sa-")
}

// certificateName fits Certificate Manager's name rule, which is the same as IAM's.
func certificateName(resName string) string {
	return yandexName(resName+"-cert", "cert-")
}

func yandexName(name, letterPrefix string) string {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = letterPrefix + name
	}
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}
