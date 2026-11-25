// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"testing"

	. "github.com/onsi/gomega"
	"github.com/simple-container-com/api/pkg/api"
)

func TestEffectiveCDNEnabled_Default(t *testing.T) {
	g := NewWithT(t)
	in := &StaticWebsiteInput{}
	g.Expect(in.EffectiveCDNEnabled()).To(BeFalse(), "default must be bucket-TLS")
}

func TestEffectiveCDNEnabled_Mirror(t *testing.T) {
	g := NewWithT(t)
	in := &StaticWebsiteInput{CDNEnabled: true}
	g.Expect(in.EffectiveCDNEnabled()).To(BeTrue(), "trap-6 top-level mirror wins")
}

func TestEffectiveCDNEnabled_NestedBlock(t *testing.T) {
	g := NewWithT(t)
	in := &StaticWebsiteInput{CDN: &StaticWebsiteCDNConfig{Enabled: true}}
	g.Expect(in.EffectiveCDNEnabled()).To(BeTrue(), "nested CDN.Enabled reaches the driver when the mirror is absent")
}

func TestEffectiveCDNEnabled_NestedDisabled(t *testing.T) {
	g := NewWithT(t)
	in := &StaticWebsiteInput{CDN: &StaticWebsiteCDNConfig{Enabled: false}}
	g.Expect(in.EffectiveCDNEnabled()).To(BeFalse())
}

func TestToStaticWebsiteConfig_PropagatesCDNFlag(t *testing.T) {
	g := NewWithT(t)
	tpl := &StaticWebsiteTemplateConfig{
		AccountConfig: AccountConfig{FolderID: "b1gfolder"},
		CDN:           &StaticWebsiteCDNConfig{Enabled: true},
	}
	stackCfg := &api.StackConfigStatic{
		BundleDir: "./dist",
		Site: api.StaticSiteConfig{
			Domain:      "example.simple-forge.ru",
			BaseDnsZone: "simple-forge.ru",
		},
	}
	got, err := ToStaticWebsiteConfig(tpl, "/tmp", "s", stackCfg)
	g.Expect(err).NotTo(HaveOccurred())

	in, ok := got.(*StaticWebsiteInput)
	g.Expect(ok).To(BeTrue())
	g.Expect(in.CDNEnabled).To(BeTrue(), "top-level trap-6 mirror populated for SC core marshal")
	g.Expect(in.CDN).NotTo(BeNil())
	g.Expect(in.CDN.Enabled).To(BeTrue())
	g.Expect(in.EffectiveCDNEnabled()).To(BeTrue())
}

func TestToStaticWebsiteConfig_DefaultsCDNOff(t *testing.T) {
	g := NewWithT(t)
	tpl := &StaticWebsiteTemplateConfig{
		AccountConfig: AccountConfig{FolderID: "b1gfolder"},
	}
	stackCfg := &api.StackConfigStatic{
		BundleDir: "./dist",
		Site: api.StaticSiteConfig{
			Domain:      "example.simple-forge.ru",
			BaseDnsZone: "simple-forge.ru",
		},
	}
	got, err := ToStaticWebsiteConfig(tpl, "/tmp", "s", stackCfg)
	g.Expect(err).NotTo(HaveOccurred())

	in := got.(*StaticWebsiteInput)
	g.Expect(in.CDNEnabled).To(BeFalse())
	g.Expect(in.CDN).To(BeNil())
	g.Expect(in.EffectiveCDNEnabled()).To(BeFalse(), "no CDN block = bucket-TLS")
}
