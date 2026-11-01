// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"github.com/pkg/errors"
	"github.com/simple-container-com/api/pkg/api"
)

// TemplateTypeStaticWebsite is a per-stack template that deploys a static
// site to Yandex Cloud: a public bucket, a CDN resource adopting the shared
// wildcard cert, a DNS CNAME (ANAME at apex) via the plugin's own yc-dns
// registrar, and a bundle sync via `local.NewCommand` + `aws s3 sync`
// against YC's S3-compatible endpoint. One Pulumi resource per site
// regardless of file count — see landing/infra/yc/main.tf for the
// Terraform shape this mirrors.
//
// Why not one state entry per bundled file: the user has lived through
// per-object Pulumi state bloat on a past project (staticapp.ru's
// predecessor) and the pain scales with bundle size. The `aws s3 sync`
// shell-out is incremental server-side so operators don't pay for the
// shortcut.
const TemplateTypeStaticWebsite = "yc-static-website"

// StaticWebsiteTemplateConfig is the PARENT-side template config:
// `templates.<name>: {type: yc-static-website, config: {...}}` in
// server.yaml. Holds credentials + optional CDN defaults every per-stack
// site inherits.
type StaticWebsiteTemplateConfig struct {
	AccountConfig `json:",inline" yaml:",inline"`

	// EdgeCacheSettings is the CDN edge TTL in seconds. Defaults to 3600.
	EdgeCacheSettings int `json:"edgeCacheSettings,omitempty" yaml:"edgeCacheSettings,omitempty"`

	// GzipOn enables CDN-side gzip compression. Defaults true.
	GzipOn *bool `json:"gzipOn,omitempty" yaml:"gzipOn,omitempty"`

	// IgnoreCookie strips Cookie headers from CDN cache keys. Defaults true.
	IgnoreCookie *bool `json:"ignoreCookie,omitempty" yaml:"ignoreCookie,omitempty"`

	// RedirectHttpToHttps serves 301s on :80 to :443. Defaults true.
	RedirectHttpToHttps *bool `json:"redirectHttpToHttps,omitempty" yaml:"redirectHttpToHttps,omitempty"`

	// ForceDestroy allows `sc destroy` to delete a bucket that still has
	// objects. Defaults off.
	ForceDestroy bool `json:"forceDestroy,omitempty" yaml:"forceDestroy,omitempty"`
}

func ReadStaticWebsiteConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &StaticWebsiteTemplateConfig{})
}

// StaticWebsiteInput is what the provisioning tier receives — a fully
// resolved template config + the client's StackConfigStatic. The compute
// context on the Provision RPC carries the shared wildcard cert id
// (emitted by WildcardCertComputeProcessor as the
// `YC_WILDCARD_CERT_<name>_ID` env var that the driver reads).
type StaticWebsiteInput struct {
	AccountConfig `json:",inline" yaml:",inline"`

	EdgeCacheSettings   int   `json:"edgeCacheSettings,omitempty" yaml:"edgeCacheSettings,omitempty"`
	GzipOn              *bool `json:"gzipOn,omitempty" yaml:"gzipOn,omitempty"`
	IgnoreCookie        *bool `json:"ignoreCookie,omitempty" yaml:"ignoreCookie,omitempty"`
	RedirectHttpToHttps *bool `json:"redirectHttpToHttps,omitempty" yaml:"redirectHttpToHttps,omitempty"`
	ForceDestroy        bool  `json:"forceDestroy,omitempty" yaml:"forceDestroy,omitempty"`

	// CertificateID is the YC Certificate Manager id of the wildcard cert
	// this site's CDN resource attaches to. Clients populate it on the
	// StackConfigStatic via a bucketName override (see
	// ToStaticWebsiteConfig below for the field lift) because the
	// client.yaml's `type: static` schema has no cert field of its own
	// and `uses:` isn't honored by the SC-core static-site deploy path.
	//
	// Operators resolve this to the cert id adopted by the parent's
	// yc-dns registrar: `certificateId: fpqd8kktgdat8rn8jtjb` in the
	// client.yaml's `config:` block, OR via `${env:…}` + a GH-level
	// variable. A future api change can plumb this via the client's
	// `uses:` block, mirroring how the mongodb plugin's ComputeProcess
	// shape works.
	CertificateID string `json:"certificateId,omitempty" yaml:"certificateId,omitempty"`

	// Top-level mirrors of StackConfig.Site fields — the plugin bus's
	// SC-core-side JSON marshal drops `Site` as a NAMED field with
	// `json:",inline"` (trap 6), so the converter populates these on the
	// bus-outbound side and the driver reads them with StackConfig.Site
	// fallback. Clients NEVER author these — they're set by
	// ToStaticWebsiteConfig only. Same workaround sc-plugin-cloudflare
	// uses (see api/pages.go `_pages_*` prefix).
	SiteDomain        string `json:"siteDomain,omitempty"        yaml:"siteDomain,omitempty"`
	SiteBaseDnsZone   string `json:"siteBaseDnsZone,omitempty"   yaml:"siteBaseDnsZone,omitempty"`
	SiteIndexDocument string `json:"siteIndexDocument,omitempty" yaml:"siteIndexDocument,omitempty"`
	SiteErrorDocument string `json:"siteErrorDocument,omitempty" yaml:"siteErrorDocument,omitempty"`
	SiteBundleDir     string `json:"siteBundleDir,omitempty"     yaml:"siteBundleDir,omitempty"`

	// StackConfig is the client stack's StackConfigStatic.
	StackConfig api.StackConfigStatic `json:"stackConfig" yaml:"stackConfig"`
}

// EffectiveDomain returns the fully-qualified site hostname, preferring the
// top-level SiteDomain populated by the converter over Site.Domain (which
// loses to trap 6 on the SC-core JSON marshal).
func (i *StaticWebsiteInput) EffectiveDomain() string {
	if i.SiteDomain != "" {
		return i.SiteDomain
	}
	return i.StackConfig.Site.Domain
}

func (i *StaticWebsiteInput) EffectiveBaseDnsZone() string {
	if i.SiteBaseDnsZone != "" {
		return i.SiteBaseDnsZone
	}
	return i.StackConfig.Site.BaseDnsZone
}

func (i *StaticWebsiteInput) EffectiveBundleDir() string {
	if i.SiteBundleDir != "" {
		return i.SiteBundleDir
	}
	return i.StackConfig.BundleDir
}

func (i *StaticWebsiteInput) Uses() []string {
	return nil
}

func (i *StaticWebsiteInput) DependsOnResources() []api.StackConfigDependencyResource {
	return nil
}

func (i *StaticWebsiteInput) OverriddenBaseZone() string {
	return i.StackConfig.Site.BaseDnsZone
}

const (
	DefaultIndexPage         = "index.html"
	DefaultCacheControl      = "max-age=300"
	DefaultEdgeCacheSettings = 3600
)

func (i *StaticWebsiteInput) EffectiveIndexPage() string {
	if i.SiteIndexDocument != "" {
		return i.SiteIndexDocument
	}
	if i.StackConfig.Site.IndexDocument != "" {
		return i.StackConfig.Site.IndexDocument
	}
	return DefaultIndexPage
}

func (i *StaticWebsiteInput) EffectiveErrorPage() string {
	if i.SiteErrorDocument != "" {
		return i.SiteErrorDocument
	}
	if i.StackConfig.Site.ErrorDocument != "" {
		return i.StackConfig.Site.ErrorDocument
	}
	return i.EffectiveIndexPage()
}

func (i *StaticWebsiteInput) EffectiveEdgeCacheSettings() int {
	if i.EdgeCacheSettings <= 0 {
		return DefaultEdgeCacheSettings
	}
	return i.EdgeCacheSettings
}

func (i *StaticWebsiteInput) EffectiveGzipOn() bool {
	if i.GzipOn == nil {
		return true
	}
	return *i.GzipOn
}

func (i *StaticWebsiteInput) EffectiveIgnoreCookie() bool {
	if i.IgnoreCookie == nil {
		return true
	}
	return *i.IgnoreCookie
}

func (i *StaticWebsiteInput) EffectiveRedirectHttpToHttps() bool {
	if i.RedirectHttpToHttps == nil {
		return true
	}
	return *i.RedirectHttpToHttps
}

// ToStaticWebsiteConfig converts a `yc-static-website` template + a
// StackConfigStatic client stack into the StaticWebsiteInput the
// Provision RPC receives.
func ToStaticWebsiteConfig(tpl any, rootDir, stackName string, stackCfg *api.StackConfigStatic) (any, error) {
	templateCfg, ok := tpl.(*StaticWebsiteTemplateConfig)
	if !ok {
		return nil, errors.Errorf("template config is not of type *yandex.StaticWebsiteTemplateConfig: %T", tpl)
	}
	if templateCfg == nil {
		return nil, errors.New("template config is nil")
	}
	if stackCfg == nil {
		return nil, errors.New("stack config is nil")
	}
	accountConfig := &AccountConfig{}
	if err := api.ConvertAuth(&templateCfg.AccountConfig, accountConfig); err != nil {
		return nil, errors.Wrap(err, "failed to convert yandex account config for yc-static-website template")
	}
	templateCfg.AccountConfig = *accountConfig
	if stackCfg.Site.Domain == "" {
		return nil, errors.Errorf("yc-static-website stack %q is missing a domain", stackName)
	}
	if stackCfg.BundleDir == "" {
		return nil, errors.Errorf("yc-static-website stack %q is missing a bundleDir", stackName)
	}
	// The cert id arrives via the client stack's StackConfigStatic.Location
	// field — the only free-form string field on the static-site shape
	// that is NOT inside the trap-6-affected `Site` subtree. Operators
	// write `location: fpqd8kktgdat8rn8jtjb` (the YC CM cert id).
	// Alternative: StackConfigStatic.BucketName, same shape. We pick
	// Location because its name reads more naturally for "where TLS
	// comes from" than BucketName does.
	//
	// Future: when the api's PrepareStaticForDeploy signature grows to
	// pass the ClientDescriptor's `uses:` through, this can be replaced
	// with a WildcardCertComputeProcessor-emitted env var.
	certID := stackCfg.Location
	return &StaticWebsiteInput{
		AccountConfig:       templateCfg.AccountConfig,
		EdgeCacheSettings:   templateCfg.EdgeCacheSettings,
		GzipOn:              templateCfg.GzipOn,
		IgnoreCookie:        templateCfg.IgnoreCookie,
		RedirectHttpToHttps: templateCfg.RedirectHttpToHttps,
		ForceDestroy:        templateCfg.ForceDestroy,
		CertificateID:       certID,
		// Trap-6 workaround: populate top-level mirrors so the driver on
		// the plugin side reads the real Site values even after the
		// `Site` subtree is dropped by SC core's JSON marshal.
		SiteDomain:        stackCfg.Site.Domain,
		SiteBaseDnsZone:   stackCfg.Site.BaseDnsZone,
		SiteIndexDocument: stackCfg.Site.IndexDocument,
		SiteErrorDocument: stackCfg.Site.ErrorDocument,
		SiteBundleDir:     stackCfg.BundleDir,
		StackConfig:       *stackCfg,
	}, nil
}
