// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"strings"

	"github.com/pkg/errors"
	"github.com/simple-container-com/api/pkg/api"
)

// ResourceTypeWildcardCert is a parent-infra resource that provisions one
// managed Let's Encrypt certificate covering <zoneName> + *.<zoneName>, with
// the ACME DNS_CNAME challenge written through the plugin's own yc-dns
// registrar. Child static-site stacks reference its cert id via `uses:` to
// avoid each landing provisioning its own cert (and its own DNS-01 record,
// which would contend on a shared `_acme-challenge.<zone>` CNAME and expire
// at ~90 days — see RegistrarConfig.CertificateID doc for the mechanism).
//
// There is deliberately no "shared CDN resource" sibling of this type: YC
// Object Storage does not virtual-host arbitrary bucket names (auth.go:124),
// and `yandex_cdn_resource` rejects `forward_host_header` + `custom_host_header`
// at once (landing/infra/yc/main.tf:133). A shared CDN would therefore have to
// forward every subdomain to one bucket, which is useless for multi-landing.
// Each yc-static-website creates its own CDN resource referencing the cert
// adopted here; YC CDN pricing is per-GB transferred, so one-CDN-per-site
// does not change the bill.
const ResourceTypeWildcardCert = "yc-wildcard-cert"

// WildcardCertConfig describes a managed Let's Encrypt certificate for a
// base DNS zone and all its subdomains.
//
// The driver provisions a single `yandex_cm_certificate` with
// `domains: [<zoneName>, *.<zoneName>]` + `managed.challenge_count: 1`
// (apex + wildcard validate against the SAME `_acme-challenge.<zone>`
// recordset — see landing/infra/yc/main.tf:52-57), writes the DNS_CNAME
// challenge via the plugin's own yc-dns registrar, waits for the cert to
// reach ISSUED status, and exports the cert id for consuming stacks.
type WildcardCertConfig struct {
	AccountConfig `json:",inline" yaml:",inline"`

	// ZoneName is the DNS zone this certificate covers, e.g. `simple-forge.ru`.
	// Must match a yc-dns registrar's zoneName on the same stack — the ACME
	// DNS_CNAME challenge is written through that registrar.
	ZoneName string `json:"zoneName" yaml:"zoneName"`

	// ZoneID is the YC resource id (`dns...`). Set it to skip the lookup-by-name.
	ZoneID string `json:"zoneId,omitempty" yaml:"zoneId,omitempty"`

	// ZoneResourceName is the YC resource name of the zone (not the DNS name).
	// Derived from ZoneName (dots → hyphens) when unset.
	ZoneResourceName string `json:"zoneResourceName,omitempty" yaml:"zoneResourceName,omitempty"`

	// AdoptCertificateID adopts an operator-provided CM cert instead of
	// provisioning a managed one. The cert must cover <ZoneName> + *.<ZoneName>;
	// the driver does not validate domains against the operator's cert.
	AdoptCertificateID string `json:"adoptCertificateId,omitempty" yaml:"adoptCertificateId,omitempty"`
}

func ReadWildcardCertConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &WildcardCertConfig{})
}

// EffectiveZoneResourceName resolves the YC resource name to look the zone up by.
func (c *WildcardCertConfig) EffectiveZoneResourceName() string {
	if c.ZoneResourceName != "" {
		return c.ZoneResourceName
	}
	return strings.ReplaceAll(strings.TrimSuffix(c.ZoneName, "."), ".", "-")
}

func (c *WildcardCertConfig) Validate() error {
	if c.ZoneName == "" {
		return errors.Errorf("zoneName is required: it is the DNS zone this wildcard cert covers, e.g. %q", "simple-forge.ru")
	}
	if c.FolderID == "" && c.ZoneID == "" {
		return errors.Errorf("yc-wildcard-cert for zone %q requires folderId (or an explicit zoneId): "+
			"a zone is looked up within a folder", c.ZoneName)
	}
	return nil
}
