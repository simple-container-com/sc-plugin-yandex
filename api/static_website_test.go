// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	"github.com/simple-container-com/api/pkg/api"
)

func staticTemplate(extra map[string]any) api.StackDescriptor {
	cfg := map[string]any{
		"cloudId":  "b1gcloud",
		"folderId": "b1gfolder",
		"zoneName": "simple-forge.ru",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return api.StackDescriptor{Type: TemplateTypeYandexStaticWebsite, Config: api.Config{Config: cfg}}
}

// The real path SC takes for a `type: static` stack on this template: registered
// config reader, then the registered static-site converter.
func TestPrepareStaticForDeploy_YcStaticWebsite(t *testing.T) {
	RegisterTestingT(t)

	out, err := api.PrepareStaticForDeploy(context.Background(), "/stacks/two-peaks", "two-peaks",
		staticTemplate(map[string]any{"certificateName": "simple-forge-ru-cert", "spaFallback": true}),
		&api.StackConfigStatic{
			BundleDir: "dist",
			Site:      api.StaticSiteConfig{Domain: "TwoPeaks.simple-forge.ru"},
		}, "forge/clients")
	Expect(err).ToNot(HaveOccurred())
	Expect(out.Type).To(Equal(TemplateTypeYandexStaticWebsite))

	in, ok := out.Config.Config.(*StaticWebsiteInput)
	Expect(ok).To(BeTrue())
	Expect(in.FolderID).To(Equal("b1gfolder"))
	Expect(in.CertificateName).To(Equal("simple-forge-ru-cert"))
	Expect(in.BucketName()).To(Equal("twopeaks.simple-forge.ru"))
	Expect(in.WebsiteEndpoint()).To(Equal("twopeaks.simple-forge.ru.website.yandexcloud.net."))
	Expect(in.EffectiveBundleDir()).To(Equal("/stacks/two-peaks/dist"))
	Expect(in.EffectiveIndexDocument()).To(Equal("index.html"))
	Expect(in.EffectiveErrorDocument()).To(Equal("index.html"), "spaFallback serves the index for unknown paths")
}

// The input crosses the plugin bus as JSON and is decoded through YAML on the
// plugin side. A field whose json and yaml names disagree arrives empty, silently.
func TestStaticWebsiteInput_SurvivesJSONToYAMLRoundTrip(t *testing.T) {
	RegisterTestingT(t)

	in := StaticWebsiteInput{
		StaticWebsiteTemplateConfig: StaticWebsiteTemplateConfig{
			AccountConfig:    AccountConfig{CloudID: "c", FolderID: "f"},
			ZoneName:         "simple-forge.ru",
			CertificateID:    "fpq123",
			SpaFallback:      true,
			RecordTtl:        600,
			CacheControl:     []CacheControlRule{{Pattern: "img/", Value: "public, max-age=60"}},
			MaxSize:          1024,
			ZoneResourceName: "zr",
		},
		Site:      api.StaticSiteConfig{Domain: "a.simple-forge.ru", IndexDocument: "index.html", ErrorDocument: "404.html"},
		BundleDir: "dist",
		StackDir:  "/s",
		StackName: "a",
	}
	b, err := json.Marshal(&in)
	Expect(err).ToNot(HaveOccurred())
	var plain map[string]any
	Expect(json.Unmarshal(b, &plain)).To(Succeed())
	y, err := yaml.Marshal(plain)
	Expect(err).ToNot(HaveOccurred())
	var out StaticWebsiteInput
	Expect(yaml.Unmarshal(y, &out)).To(Succeed())
	Expect(out).To(Equal(in))
}

func TestStaticWebsiteInput_ErrorDocument(t *testing.T) {
	RegisterTestingT(t)

	in := StaticWebsiteInput{}
	Expect(in.EffectiveErrorDocument()).To(BeEmpty(), "no SPA fallback and no errorDocument: YC's own 404")
	in.Site.IndexDocument = "main.html"
	in.SpaFallback = true
	Expect(in.EffectiveErrorDocument()).To(Equal("main.html"))
	in.Site.ErrorDocument = "404.html"
	Expect(in.EffectiveErrorDocument()).To(Equal("404.html"), "the client's own errorDocument wins")
}

func TestStaticWebsiteInput_CacheControlDefaults(t *testing.T) {
	RegisterTestingT(t)

	in := StaticWebsiteInput{}
	Expect(in.CacheControlFor("index.html")).To(Equal(CacheControlNoCache))
	Expect(in.CacheControlFor("assets/index-3f9a.js")).To(Equal(CacheControlImmutable))
	Expect(in.CacheControlFor("assets/img/logo-1a2b.svg")).To(Equal(CacheControlImmutable))
	Expect(in.CacheControlFor("images/peak.jpg")).To(Equal(CacheControlDefault))
	Expect(in.CacheControlFor("about/index.html")).To(Equal(CacheControlNoCache),
		"a pre-rendered page references hashed bundles just like the index does")
}

func TestStaticWebsiteInput_CacheControlRules(t *testing.T) {
	RegisterTestingT(t)

	in := StaticWebsiteInput{StaticWebsiteTemplateConfig: StaticWebsiteTemplateConfig{CacheControl: []CacheControlRule{
		{Pattern: "*.html", Value: "no-cache"},
		{Pattern: "/static/**", Value: "immutable"},
		{Pattern: "fonts/*.woff2", Value: "fonts"},
	}}}
	Expect(in.CacheControlFor("index.html")).To(Equal("no-cache"))
	Expect(in.CacheControlFor("blog/post.html")).To(Equal("no-cache"), "a slash-less glob also matches the base name")
	Expect(in.CacheControlFor("static/a/b.js")).To(Equal("immutable"))
	Expect(in.CacheControlFor("fonts/x.woff2")).To(Equal("fonts"))
	Expect(in.CacheControlFor("fonts/sub/x.woff2")).To(Equal(CacheControlDefault))
	Expect(in.CacheControlFor("robots.txt")).To(Equal(CacheControlDefault))
}

func TestStaticWebsiteInput_Validate(t *testing.T) {
	RegisterTestingT(t)

	valid := func() *StaticWebsiteInput {
		return &StaticWebsiteInput{
			StaticWebsiteTemplateConfig: StaticWebsiteTemplateConfig{ZoneName: "simple-forge.ru."},
			Site:                        api.StaticSiteConfig{Domain: "twopeaks.simple-forge.ru"},
			BundleDir:                   "dist",
		}
	}
	Expect(valid().Validate()).To(Succeed())

	for name, mutate := range map[string]func(*StaticWebsiteInput){
		"no domain":         func(i *StaticWebsiteInput) { i.Site.Domain = "" },
		"no bundleDir":      func(i *StaticWebsiteInput) { i.BundleDir = "" },
		"no zone":           func(i *StaticWebsiteInput) { i.ZoneName = "" },
		"outside the zone":  func(i *StaticWebsiteInput) { i.Site.Domain = "twopeaks.example.com" },
		"suffix not a zone": func(i *StaticWebsiteInput) { i.Site.Domain = "evilsimple-forge.ru" },
		"apex":              func(i *StaticWebsiteInput) { i.Site.Domain = "simple-forge.ru" },
		"both certs":        func(i *StaticWebsiteInput) { i.CertificateID = "x"; i.CertificateName = "y" },
		"empty cache rule":  func(i *StaticWebsiteInput) { i.CacheControl = []CacheControlRule{{Pattern: "x"}} },
		"long domain": func(i *StaticWebsiteInput) {
			i.Site.Domain = "a-very-long-subdomain-name-that-goes-over-the-limit-xx.simple-forge.ru"
		},
	} {
		in := valid()
		mutate(in)
		Expect(in.Validate()).To(HaveOccurred(), name)
	}
}

func TestToStaticWebsiteConfig_RejectsWrongTemplate(t *testing.T) {
	RegisterTestingT(t)

	_, err := ToStaticWebsiteConfig(&TemplateConfig{}, "", "s", &api.StackConfigStatic{})
	Expect(err).To(HaveOccurred())
	_, err = ToStaticWebsiteConfig(&StaticWebsiteTemplateConfig{}, "", "s", nil)
	Expect(err).To(HaveOccurred())
}
