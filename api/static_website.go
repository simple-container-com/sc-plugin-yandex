// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"path"
	"strings"

	"github.com/pkg/errors"

	"github.com/simple-container-com/api/pkg/api"
)

// TemplateTypeYandexStaticWebsite serves a `type: static` client stack from a Yandex
// Object Storage bucket with website hosting, the YC analogue of aws-static-website.
//
// The shape is the one the hand-written two-peaks deploy (simple-container-landing,
// demos/two-peaks/scripts/deploy-yc.sh) runs in production, and the reason it is not
// Cloudflare-fronted the way the AWS sites are: Russian ISPs cut Cloudflare responses
// after 16 KB (forge docs/design/2026-10-11/ru-captcha-off-cloudflare). So:
//
//   - the bucket is named after the domain — YC serves bucket-hosted HTTPS only when
//     the bucket name equals the Host;
//   - HTTPS comes from a Certificate Manager certificate attached to the bucket;
//   - DNS is a CNAME `<domain>.` → `<domain>.website.yandexcloud.net.` in a yc-dns
//     zone.
const TemplateTypeYandexStaticWebsite = "yc-static-website"

// WebsiteEndpointSuffix is the host YC serves bucket websites under; a bucket named
// `x.example.ru` answers at `x.example.ru.website.yandexcloud.net`.
const WebsiteEndpointSuffix = "website.yandexcloud.net"

const (
	DefaultIndexDocument = "index.html"

	// CacheControlImmutable is for content-hashed bundles: a new build ships new
	// names, so an old name never changes content.
	CacheControlImmutable = "public, max-age=31536000, immutable"
	// CacheControlNoCache forces revalidation. The index document must carry it, or
	// a browser keeps referencing bundles a later deploy has deleted.
	CacheControlNoCache = "no-cache"
	// CacheControlDefault is a short cache for everything not hashed (images,
	// favicons, robots.txt): they can be regenerated under the same name.
	CacheControlDefault = "public, max-age=3600"

	// DefaultHashedAssetsPrefix is where Vite (and most bundlers) put content-hashed
	// output.
	DefaultHashedAssetsPrefix = "assets/"
)

// StaticWebsiteTemplateConfig is the server.yaml half of a `yc-static-website`
// template: credentials, the DNS zone the site's record goes into, and how the
// certificate is obtained. Per-site values (domain, bundleDir, index/error documents)
// come from the client stack, exactly as for aws-static-website.
type StaticWebsiteTemplateConfig struct {
	AccountConfig `json:",inline" yaml:",inline"`

	// ZoneName is the DNS zone the CNAME is written into, e.g. `simple-forge.ru`. The
	// zone is looked up, never created, with the same rules as the yc-dns registrar.
	ZoneName string `json:"zoneName,omitempty" yaml:"zoneName,omitempty"`
	// ZoneID pins the zone by id; ZoneResourceName overrides the YC resource name
	// derived from ZoneName (`simple-forge.ru` → `simple-forge-ru`).
	ZoneID           string `json:"zoneId,omitempty" yaml:"zoneId,omitempty"`
	ZoneResourceName string `json:"zoneResourceName,omitempty" yaml:"zoneResourceName,omitempty"`
	// RecordTtl is the CNAME's TTL in seconds; DefaultRecordTtl when unset.
	RecordTtl int `json:"recordTtl,omitempty" yaml:"recordTtl,omitempty"`

	// CertificateID attaches an existing Certificate Manager certificate (e.g. a
	// wildcard for the zone). CertificateName does the same, looked up by name in
	// FolderID. With neither set a Let's Encrypt certificate is requested for the
	// site's domain with a DNS challenge answered in the same zone.
	CertificateID   string `json:"certificateId,omitempty" yaml:"certificateId,omitempty"`
	CertificateName string `json:"certificateName,omitempty" yaml:"certificateName,omitempty"`

	// SpaFallback serves the index document for every unknown path, so a
	// client-side router can take over (/trips/42 → index.html). It applies only
	// when the client stack names no errorDocument of its own.
	SpaFallback bool `json:"spaFallback,omitempty" yaml:"spaFallback,omitempty"`

	// CacheControl overrides the upload's Cache-Control rules; the first matching
	// rule wins. Unset keeps the defaults: HTML and the index document no-cache,
	// `assets/**` immutable, everything else one hour.
	CacheControl []CacheControlRule `json:"cacheControl,omitempty" yaml:"cacheControl,omitempty"`

	// MaxSize caps the bucket in bytes; zero is unlimited.
	MaxSize int `json:"maxSize,omitempty" yaml:"maxSize,omitempty"`
	// KeepOnDestroy refuses to delete a non-empty bucket on `sc destroy`. The
	// default deletes it with its content: everything in it is a build output that
	// the next deploy uploads again.
	KeepOnDestroy bool `json:"keepOnDestroy,omitempty" yaml:"keepOnDestroy,omitempty"`
}

// CacheControlRule maps object keys to a Cache-Control value.
//
// Pattern is matched against the object key (the path relative to bundleDir, with
// forward slashes). A pattern ending in `/` or `/**` matches every key under that
// prefix; anything else is a path.Match glob tried against the full key and, when
// it has no `/`, against the base name too — so `*.html` matches nested pages.
type CacheControlRule struct {
	Pattern string `json:"pattern" yaml:"pattern"`
	Value   string `json:"value" yaml:"value"`
}

// Matches reports whether key falls under the rule.
func (r CacheControlRule) Matches(key string) bool {
	p := strings.TrimPrefix(r.Pattern, "/")
	if strings.HasSuffix(p, "/**") || strings.HasSuffix(p, "/") {
		return strings.HasPrefix(key, strings.TrimSuffix(p, "**"))
	}
	if ok, _ := path.Match(p, key); ok {
		return true
	}
	if !strings.Contains(p, "/") {
		ok, _ := path.Match(p, path.Base(key))
		return ok
	}
	return false
}

func ReadStaticWebsiteTemplateConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &StaticWebsiteTemplateConfig{})
}

// StaticWebsiteInput is what the provisioning tier receives for a static stack on
// this template.
//
// Every field carries the same json and yaml name on purpose. The input crosses the
// plugin bus as JSON and is decoded on the plugin side through YAML, so a field
// whose two tags disagree — api.StackConfigStatic's `Site` is `json:",inline"`, which
// encoding/json treats as a nested `Site` object while YAML flattens it — arrives
// empty. That is why the static config is copied into Site/BundleDir here rather
// than embedded.
type StaticWebsiteInput struct {
	StaticWebsiteTemplateConfig `json:",inline" yaml:",inline"`

	Site      api.StaticSiteConfig `json:"site" yaml:"site"`
	BundleDir string               `json:"bundleDir" yaml:"bundleDir"`
	StackDir  string               `json:"stackDir" yaml:"stackDir"`
	StackName string               `json:"stackName" yaml:"stackName"`
}

// OverriddenBaseZone implements api.DnsConfigAware, as aws.StaticSiteInput does.
func (i *StaticWebsiteInput) OverriddenBaseZone() string {
	return i.Site.BaseDnsZone
}

// BucketName is the site's domain: YC serves bucket-hosted HTTPS only when the
// bucket name equals the requested host.
func (i *StaticWebsiteInput) BucketName() string {
	return strings.ToLower(strings.TrimSuffix(i.Site.Domain, "."))
}

// WebsiteEndpoint is the CNAME target, with the trailing dot YC DNS wants.
func (i *StaticWebsiteInput) WebsiteEndpoint() string {
	return i.BucketName() + "." + WebsiteEndpointSuffix + "."
}

// EffectiveIndexDocument falls back to index.html.
func (i *StaticWebsiteInput) EffectiveIndexDocument() string {
	if i.Site.IndexDocument != "" {
		return i.Site.IndexDocument
	}
	return DefaultIndexDocument
}

// EffectiveErrorDocument is the client's errorDocument, else the index document
// when the template asks for SPA fallback, else empty (YC's own 404 page).
func (i *StaticWebsiteInput) EffectiveErrorDocument() string {
	if i.Site.ErrorDocument != "" {
		return i.Site.ErrorDocument
	}
	if i.SpaFallback {
		return i.EffectiveIndexDocument()
	}
	return ""
}

// EffectiveBundleDir resolves bundleDir against the stack directory.
func (i *StaticWebsiteInput) EffectiveBundleDir() string {
	if i.BundleDir == "" || path.IsAbs(i.BundleDir) {
		return i.BundleDir
	}
	return path.Join(i.StackDir, i.BundleDir)
}

// CacheControlFor returns the Cache-Control value for an object key.
func (i *StaticWebsiteInput) CacheControlFor(key string) string {
	rules := i.CacheControl
	if len(rules) == 0 {
		rules = []CacheControlRule{
			// Every HTML page, not only the root index: a pre-rendered page
			// references hashed bundles exactly as the index does, so a cached copy
			// outlives the bundles a later deploy deletes.
			{Pattern: "*.html", Value: CacheControlNoCache},
			{Pattern: i.EffectiveIndexDocument(), Value: CacheControlNoCache},
			{Pattern: DefaultHashedAssetsPrefix, Value: CacheControlImmutable},
		}
	}
	for _, r := range rules {
		if r.Matches(key) {
			return r.Value
		}
	}
	return CacheControlDefault
}

// ZoneRegistrarConfig expresses the site's zone as the RegistrarConfig the zone
// lookup already validates, so a static site and a yc-dns registrar resolve a zone
// the same way.
func (i *StaticWebsiteInput) ZoneRegistrarConfig() *RegistrarConfig {
	return &RegistrarConfig{
		AccountConfig:    i.AccountConfig,
		ZoneName:         i.ZoneName,
		ZoneID:           i.ZoneID,
		ZoneResourceName: i.ZoneResourceName,
		RecordTtl:        i.RecordTtl,
	}
}

// Validate rejects a site that cannot be served, at config time.
func (i *StaticWebsiteInput) Validate() error {
	domain := i.BucketName()
	if domain == "" {
		return errors.New("yc-static-website: `domain` is required in the client stack — the bucket is named after it")
	}
	if len(domain) > 63 {
		return errors.Errorf("yc-static-website: domain %q is longer than 63 characters, the Object Storage bucket name limit", domain)
	}
	if i.BundleDir == "" {
		return errors.New("yc-static-website: `bundleDir` is required in the client stack")
	}
	if i.ZoneName == "" {
		return errors.Errorf("yc-static-website: `zoneName` is required in the template — the DNS zone %q's record goes into", domain)
	}
	zone := strings.ToLower(strings.TrimSuffix(i.ZoneName, "."))
	if domain != zone && !strings.HasSuffix(domain, "."+zone) {
		return errors.Errorf("yc-static-website: domain %q is not inside zone %q", domain, zone)
	}
	if domain == zone {
		// A CNAME cannot sit at a zone apex.
		return errors.Errorf("yc-static-website: domain %q is the zone apex; a CNAME cannot be placed there — use a subdomain", domain)
	}
	if i.CertificateID != "" && i.CertificateName != "" {
		return errors.New("yc-static-website: set certificateId or certificateName, not both")
	}
	for _, r := range i.CacheControl {
		if r.Pattern == "" || r.Value == "" {
			return errors.Errorf("yc-static-website: cacheControl rule %+v needs both pattern and value", r)
		}
	}
	return nil
}

// ToStaticWebsiteConfig converts a `yc-static-website` template plus a static client
// stack into the provisioning input.
func ToStaticWebsiteConfig(tpl any, stackDir, stackName string, stackCfg *api.StackConfigStatic) (any, error) {
	templateCfg, ok := tpl.(*StaticWebsiteTemplateConfig)
	if !ok {
		return nil, errors.Errorf("template config is not of type *yandex.StaticWebsiteTemplateConfig")
	}
	if templateCfg == nil {
		return nil, errors.Errorf("template config is nil")
	}
	if stackCfg == nil {
		return nil, errors.Errorf("stack config is nil")
	}

	accountConfig := &AccountConfig{}
	if err := api.ConvertAuth(&templateCfg.AccountConfig, accountConfig); err != nil {
		return nil, errors.Wrapf(err, "failed to convert yandex account config")
	}
	tplCopy := *templateCfg
	tplCopy.AccountConfig = *accountConfig

	res := &StaticWebsiteInput{
		StaticWebsiteTemplateConfig: tplCopy,
		Site:                        stackCfg.Site,
		BundleDir:                   stackCfg.BundleDir,
		StackDir:                    stackDir,
		StackName:                   stackName,
	}
	// Fail while the operator is looking at the config, not mid-provision.
	if err := res.Validate(); err != nil {
		return nil, err
	}
	return res, nil
}
