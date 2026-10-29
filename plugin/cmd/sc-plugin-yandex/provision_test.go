// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"sync"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/samber/lo"
	"github.com/simple-container-com/api/pkg/api"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"

	ycApi "github.com/simple-container-com/sc-plugin-yandex/api"
)

// engineMocks answers like the engine. With preview set it answers like
// `pulumi preview` on a first deploy: no resource exists yet, so none has an id
// or any computed output. The plugin's own context is never a dry-run one (the
// RequestHeader carries no such flag), so the context in these tests is not
// either — the empty ids reach waitForComposite as known values, as they do live.
type engineMocks struct {
	mu      sync.Mutex
	preview bool
	created map[string]int
}

func (m *engineMocks) NewResource(args sdk.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	if m.created == nil {
		m.created = map[string]int{}
	}
	m.created[args.TypeToken]++
	m.mu.Unlock()
	outs := args.Inputs.Copy()
	switch args.TypeToken {
	case "yandex:index/serverlessContainer:ServerlessContainer":
		if !m.preview {
			outs["url"] = resource.NewProperty("https://bba1.containers.yandexcloud.net/")
		}
	case "docker:index/image:Image":
		if !m.preview {
			outs["repoDigest"] = resource.NewProperty("cr.yandex/crp1/two-peaks@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000")
		}
	}
	if m.preview {
		return "", outs, nil
	}
	return args.Name + "-id", outs, nil
}

// Call answers the registrar's DNS zone lookup, which is by YC resource name,
// with the zone that resource serves in the fleet's simple-forge folder.
func (m *engineMocks) Call(args sdk.MockCallArgs) (resource.PropertyMap, error) {
	if args.Token == "yandex:index/getDnsZone:getDnsZone" {
		name := args.Args["name"].StringValue()
		zone := map[string]string{"simple-forge-ru": "simple-forge.ru", "atriumdev-ru": "atriumdev.ru"}[name]
		return resource.PropertyMap{
			"id":   resource.NewProperty("dns-" + name),
			"name": resource.NewProperty(name),
			"zone": resource.NewProperty(zone + "."),
		}, nil
	}
	return args.Args, nil
}

func (m *engineMocks) count(token string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.created[token]
}

// twoPeaksRequest builds the ProvisionRequest SC core sends for two-peaks'
// `staging-ru` stack: the yc-container template of forge/infra converted with
// the plugin's own ToServerlessContainerConfig (what core runs), serialised the
// way core's serializeConfigToInputs does, plus the env its `uses: [mongodb]`
// binding contributes.
// ycCredentials is what `${auth:yc}` resolves to: the account config as an
// opaque blob. The key is a throwaway literal, not a credential.
func ycCredentials() string {
	return api.AuthToString(&ycApi.AccountConfig{
		CloudID:           "b1g-cloud",
		FolderID:          "b1g-folder",
		ServiceAccountKey: `{"id":"k","service_account_id":"sa","private_key":"not-a-key"}`,
	})
}

func twoPeaksRequest(t *testing.T, mutateTpl func(*ycApi.TemplateConfig), mutate func(*api.StackConfigSingleImage)) *scpluginv1.ProvisionRequest {
	t.Helper()
	tpl := &ycApi.TemplateConfig{AccountConfig: ycApi.AccountConfig{Credentials: api.Credentials{Credentials: ycCredentials()}}}
	if mutateTpl != nil {
		mutateTpl(tpl)
	}
	stackCfg := &api.StackConfigSingleImage{
		Uses:        []string{"mongodb"},
		Domain:      "twopeaks-staging.simple-forge.ru",
		BaseDnsZone: "simple-forge.ru",
		MaxMemory:   lo.ToPtr(512),
		Timeout:     lo.ToPtr(300),
		Image: &api.ContainerImage{
			Dockerfile: "Dockerfile",
			Build:      &api.ContainerImageBuild{Args: map[string]string{"COMMIT": "abc123"}},
		},
		Env: map[string]string{
			"DB_PROVIDER":     "mongo",
			"PUBLIC_BASE_URL": "https://twopeaks-staging.simple-forge.ru",
		},
		Secrets: map[string]string{
			"SESSION_SECRET":       "s",
			"ADMIN_OWNER_EMAIL":    "o@example.ru",
			"ADMIN_OWNER_PASSWORD": "p",
		},
	}
	if mutate != nil {
		mutate(stackCfg)
	}
	cfg, err := ycApi.ToServerlessContainerConfig(tpl, stackCfg)
	Expect(err).ToNot(HaveOccurred())
	return &scpluginv1.ProvisionRequest{
		Header:       &scpluginv1.RequestHeader{MonitorAddress: "mock", StackName: "two-peaks", Environment: "staging-ru"},
		ResourceType: ycApi.TemplateTypeYandexServerlessContainer,
		ResourceName: "two-peaks",
		Inputs:       toInputs(t, cfg),
		ComputeEnvVars: []*scpluginv1.ComputeEnvVariable{
			{Name: "MONGO_DATABASE", Value: "two-peaks", ResourceName: "mongodb", ResourceType: "mongodb-atlas", StackName: "two-peaks"},
			{Name: "MONGO_USER", Value: "two-peaks", ResourceName: "mongodb", ResourceType: "mongodb-atlas", StackName: "two-peaks"},
			{Name: "MONGO_URI", Value: "mongodb+srv://u:p@h/two-peaks", Secret: true, ResourceName: "mongodb", ResourceType: "mongodb-atlas", StackName: "two-peaks"},
			{Name: "MONGO_PASSWORD", Value: "p", Secret: true, ResourceName: "mongodb", ResourceType: "mongodb-atlas", StackName: "two-peaks"},
		},
	}
}

// registrarRequest is the ProvisionRegistrarRequest SC core's registrar
// sidecar sends for one of forge/infra's yc-dns registrars.
func registrarRequest(zone, certificateID string) *scpluginv1.ProvisionRegistrarRequest {
	str := func(v string) *scpluginv1.InputValue {
		return &scpluginv1.InputValue{Value: &scpluginv1.InputValue_StringValue{StringValue: v}}
	}
	return &scpluginv1.ProvisionRegistrarRequest{
		Header:        &scpluginv1.RequestHeader{MonitorAddress: "mock"},
		RegistrarType: ycApi.RegistrarTypeYandexDns,
		Config: map[string]*scpluginv1.InputValue{
			"credentials":   str(ycCredentials()),
			"zoneName":      str(zone),
			"certificateId": str(certificateID),
		},
	}
}

// runProvision drives the plugin the way one deploy does: the registrars SC
// core initialised (each RPC in a context of its own, as live), then the
// container's Provision.
func runProvision(p *ycPlugin, regs []*scpluginv1.ProvisionRegistrarRequest, req *scpluginv1.ProvisionRequest, mocks *engineMocks) (resp *scpluginv1.ProvisionResponse, err error) {
	opts := sdk.WithMocks("two-peaks", "staging-ru", mocks)
	for _, reg := range regs {
		if runErr := sdk.RunErr(func(pctx *sdk.Context) error {
			_, err := p.provisionRegistrar(pctx, reg)
			return err
		}, opts); runErr != nil {
			return nil, runErr
		}
	}
	runErr := sdk.RunErr(func(pctx *sdk.Context) error {
		resp, err = p.provisionServerlessContainer(pctx, req)
		return nil
	}, opts)
	if err == nil {
		err = runErr
	}
	return resp, err
}

func newTestPlugin() *ycPlugin { return &ycPlugin{progress: scPlugin.NewPublisher(0)} }

// forge/infra declares two yc-dns registrars. atriumdev.ru is initialised LAST
// here on purpose: the single-slot plugin answered every domain with the last
// one, which would have pointed simple-forge.ru at the atriumdev.ru zone.
func forgeInfraRegistrars() []*scpluginv1.ProvisionRegistrarRequest {
	return []*scpluginv1.ProvisionRegistrarRequest{
		registrarRequest("simple-forge.ru", "fpq-simple-forge-ru"),
		registrarRequest("atriumdev.ru", "fpq-atriumdev-ru"),
	}
}

const tokenApiGateway = "yandex:index/apiGateway:ApiGateway"

// Regression for sc-plugin-yandex v2026.10.29-b913f2a: two-peaks' preview
// (run 40914703224) killed the plugin with exit status 2. Provision never set
// params.Registrar, and the container template called
// params.Registrar.ProvisionDomainForEndpoint for the stack's `domain:` — a nil
// interface call (internal/serverless_container.go, provisionDNSForContainer).
func TestProvisionServerlessContainer_TwoPeaksShape(t *testing.T) {
	for _, preview := range []bool{true, false} {
		t.Run(lo.Ternary(preview, "preview", "up"), func(t *testing.T) {
			RegisterTestingT(t)
			mocks := &engineMocks{preview: preview}
			resp, err := runProvision(newTestPlugin(), forgeInfraRegistrars(), twoPeaksRequest(t, nil, nil), mocks)
			Expect(err).ToNot(HaveOccurred())
			Expect(resp).ToNot(BeNil())
			Expect(mocks.count("yandex:index/serverlessContainer:ServerlessContainer")).To(Equal(1))
			Expect(mocks.count("yandex:index/lockboxSecretVersion:LockboxSecretVersion")).To(Equal(1))
			Expect(mocks.count(tokenApiGateway)).To(Equal(1))
		})
	}
}

// forge-share: same template and parent, no secrets of its own (the Lockbox
// entries are only the mongodb binding's).
func TestProvisionServerlessContainer_ForgeShareShape(t *testing.T) {
	RegisterTestingT(t)
	req := twoPeaksRequest(t, nil, func(c *api.StackConfigSingleImage) {
		c.Domain = "share.simple-forge.ru"
		c.MaxMemory = lo.ToPtr(128)
		c.Timeout = lo.ToPtr(30)
		c.Secrets = nil
		c.Env = map[string]string{"SHARE_BASE_URL": "https://share.simple-forge.ru", "SHARE_MAX_BYTES": "4096"}
	})
	mocks := &engineMocks{preview: true}
	_, err := runProvision(newTestPlugin(), forgeInfraRegistrars(), req, mocks)
	Expect(err).ToNot(HaveOccurred())
	Expect(mocks.count(tokenApiGateway)).To(Equal(1))
}

// The registrar for a domain is chosen by zone, not by which was initialised
// last.
func TestRegistrarForDomain_PicksTheServingZone(t *testing.T) {
	RegisterTestingT(t)
	p := newTestPlugin()
	_, err := runProvision(p, forgeInfraRegistrars(), twoPeaksRequest(t, nil, nil), &engineMocks{})
	Expect(err).ToNot(HaveOccurred())
	Expect(p.registrarForDomain("twopeaks-staging.simple-forge.ru").MainDomain()).To(Equal("simple-forge.ru"))
	Expect(p.registrarForDomain("atriumdev.ru").MainDomain()).To(Equal("atriumdev.ru"))
	Expect(p.registrarForDomain("example.com")).To(BeNil())
	// A DNS RPC for a name under neither zone keeps the old answer.
	Expect(p.registrarFor("example.com").MainDomain()).To(Equal("atriumdev.ru"))
	// Re-initialising a zone does not add a second entry for it.
	Expect(sdk.RunErr(func(pctx *sdk.Context) error {
		_, err := p.provisionRegistrar(pctx, registrarRequest("simple-forge.ru", "fpq-simple-forge-ru"))
		return err
	}, sdk.WithMocks("two-peaks", "staging-ru", &engineMocks{}))).To(Succeed())
	Expect(p.registrars).To(HaveLen(2))
}

// With no registrar for the domain's zone the deploy must fail with an error
// that names the problem, before anything is registered — not panic the plugin.
func TestProvisionServerlessContainer_NoRegistrarForDomain(t *testing.T) {
	for name, regs := range map[string][]*scpluginv1.ProvisionRegistrarRequest{
		"none initialised":     nil,
		"only some other zone": {registrarRequest("atriumdev.ru", "fpq-atriumdev-ru")},
	} {
		t.Run(name, func(t *testing.T) {
			RegisterTestingT(t)
			mocks := &engineMocks{preview: true}
			_, err := runProvision(newTestPlugin(), regs, twoPeaksRequest(t, nil, nil), mocks)
			Expect(err).To(MatchError(ContainSubstring(`domain "twopeaks-staging.simple-forge.ru"`)))
			Expect(err).To(MatchError(ContainSubstring("no registrar serving it")))
			Expect(mocks.count("yandex:index/serverlessContainer:ServerlessContainer")).To(Equal(0))
			Expect(mocks.count("docker:index/image:Image")).To(Equal(0))
		})
	}
}

// A stack without a domain needs no registrar at all.
func TestProvisionServerlessContainer_NoDomainNeedsNoRegistrar(t *testing.T) {
	RegisterTestingT(t)
	req := twoPeaksRequest(t, nil, func(c *api.StackConfigSingleImage) { c.Domain = "" })
	_, err := runProvision(newTestPlugin(), nil, req, &engineMocks{preview: true})
	Expect(err).ToNot(HaveOccurred())
}

// A template naming a pre-existing serviceAccountId and registryId: SC creates
// neither, and waitForComposite dereferenced both unconditionally — the next
// nil deref after the registrar one.
func TestProvisionServerlessContainer_PreExistingServiceAccountAndRegistry(t *testing.T) {
	RegisterTestingT(t)
	req := twoPeaksRequest(t, func(tpl *ycApi.TemplateConfig) {
		tpl.ServiceAccountID = "aje-existing-sa"
		tpl.RegistryID = "crp-existing"
	}, nil)
	mocks := &engineMocks{}
	_, err := runProvision(newTestPlugin(), forgeInfraRegistrars(), req, mocks)
	Expect(err).ToNot(HaveOccurred())
	Expect(mocks.count("yandex:index/iamServiceAccount:IamServiceAccount")).To(Equal(0))
	Expect(mocks.count("yandex:index/containerRegistry:ContainerRegistry")).To(Equal(0))
}
