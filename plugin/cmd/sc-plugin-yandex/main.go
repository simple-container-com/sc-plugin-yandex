// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// sc-plugin-yandex is the Yandex Cloud driver plugin for Simple Container,
// extracted from pkg/clouds/{yandex,pulumi/yandex} per the Phase 3 extraction
// in forge/docs/design/2026-10-24/sc-plugin-architecture/.
//
// V2 scope — the full in-tree yandex surface:
//   - pulumi:providers:yandex                 (Provider RPC)
//   - yc-dns registrar                        (ProvisionRegistrar + NewDnsRecord +
//     NewOverrideHeaderRule +
//     ProvisionDomainForEndpoint)
//   - yc-bucket                               (Provision + ComputeProcess)
//   - yc-serverless-container                 (Provision)
//   - yc-object-storage state store           (InitStateStore)
//
// Each RPC opens a short-lived pulumi.Context against the host's monitor
// address and reuses the lifted internal/*.go driver code unchanged — the
// extraction is pure code movement, no API changes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/samber/lo"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
	"github.com/simple-container-com/api/pkg/plugin/runtime"

	ycApi "github.com/simple-container-com/sc-plugin-yandex/api"
	ycInternal "github.com/simple-container-com/sc-plugin-yandex/plugin/internal"
)

type ycPlugin struct {
	scpluginv1.UnimplementedSCCloudPluginServer

	mu sync.Mutex
	// registrars holds every yc-dns registrar SC core initialised in this
	// process, in the order it did so. A parent stack may declare several (the
	// fleet's forge/infra declares one for atriumdev.ru and one for
	// simple-forge.ru), and a single slot let the last one initialised answer
	// for a domain in the other's zone.
	registrars []pApi.Registrar
	progress   *scPlugin.Publisher
	// newPulumiContext, when set, replaces dialling the monitor named in the
	// request header. Tests set it to open a context over Pulumi mocks, so a
	// handler runs with the same context lifecycle it has live.
	newPulumiContext func(context.Context, *scpluginv1.RequestHeader) (*sdk.Context, error)
}

// registrarForDomain returns the registrar whose zone serves domain — the
// longest such zone — or nil when none of the initialised ones does.
func (p *ycPlugin) registrarForDomain(domain string) pApi.Registrar {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best pApi.Registrar
	bestLen := -1
	for _, r := range p.registrars {
		zone := r.MainDomain()
		if api.DomainInZone(domain, zone) && len(zone) > bestLen {
			best, bestLen = r, len(zone)
		}
	}
	return best
}

// registrarFor picks the registrar for a DNS RPC from SC core: the one whose
// zone serves name, else — for a name that is not a domain under any of them —
// the most recently initialised, which is what the single-slot plugin did.
func (p *ycPlugin) registrarFor(name string) pApi.Registrar {
	if r := p.registrarForDomain(name); r != nil {
		return r
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.registrars) == 0 {
		return nil
	}
	return p.registrars[len(p.registrars)-1]
}

func (p *ycPlugin) Schema(_ context.Context, _ *scpluginv1.SchemaRequest) (*scpluginv1.SchemaResponse, error) {
	return &scpluginv1.SchemaResponse{
		PluginVersion:   "yandex-0.0.3",
		StateStoreTypes: []string{ycApi.StateStorageTypeYandexObjectStorage},
		// Intentionally NOT advertising ProviderTypes. The lifted yandex
		// Provider.go names its pulumi:providers:yandex resource after
		// pcfg.FolderID — same formula in-tree and here. Having SC core
		// call Provider() first would create the Provider in SC core's
		// pctx, but each plugin Provision RPC opens its own pctx and
		// the ProviderResource handle doesn't cross contexts. The plugin
		// re-runs Provider inside each Provision instead: the URN stays
		// byte-identical (same name, same provider version), and the
		// bucket resource gets a live ProviderResource in the SAME pctx.
		RegistrarTypes: []string{ycApi.RegistrarTypeYandexDns},
		ResourceTypes: []string{
			ycApi.ResourceTypeObjectStorageBucket,
			ycApi.ResourceTypeWildcardCert,
			ycApi.ResourceTypeYandexContainerRegistry,
			ycApi.TemplateTypeYandexServerlessContainer,
			ycApi.TemplateTypeStaticWebsite,
		},
		ComputeProcessorTypes: []string{
			ycApi.ResourceTypeObjectStorageBucket,
			ycApi.ResourceTypeWildcardCert,
		},
		PulumiPlugins: []*scpluginv1.PulumiPluginRequirement{
			// simple-container-com/pulumi-yandex v1.1.0 — the static bridge over
			// terraform-provider-yandex (pulumi/pulumi-yandex is archived at v0.13.0).
			{Package: "yandex", Version: "1.1.0"},
		},
	}, nil
}

// pulumiContextFromHeader opens the handler's Pulumi context via the shared
// api/pkg/plugin/runtime library (yandex's own pre-split helpers moved there
// unchanged: pulumi_wait.go via go:linkname, engine.go for the local Engine
// fallback). Handlers call runtime.Settle on the returned Context before
// responding to drain pending registrations.
//
// The newPulumiContext field remains the test seam — provision_test.go sets
// it to open a context over Pulumi mocks. Passed through as WithContextFactory.
func (p *ycPlugin) pulumiContextFromHeader(ctx context.Context, h *scpluginv1.RequestHeader) (*runtime.Context, error) {
	opts := []runtime.ContextOption{runtime.WithLocalEngineFallback(p.progress)}
	if p.newPulumiContext != nil {
		opts = append(opts, runtime.WithContextFactory(p.newPulumiContext))
	}
	return runtime.NewContext(ctx, h, opts...)
}

func (p *ycPlugin) ProvisionRegistrar(ctx context.Context, req *scpluginv1.ProvisionRegistrarRequest) (resp *scpluginv1.ProvisionRegistrarResponse, err error) {
	p.progress.Info("provision-registrar", fmt.Sprintf("initialising yandex registrar for zone %q", strVal(req.GetConfig()["zoneName"])))
	rctx, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)
	return p.provisionRegistrar(rctx.Pulumi, req)
}

func (p *ycPlugin) provisionRegistrar(pctx *sdk.Context, req *scpluginv1.ProvisionRegistrarRequest) (*scpluginv1.ProvisionRegistrarResponse, error) {
	cfg := inputsToRegistrarConfig(req.GetConfig())
	desc := api.RegistrarDescriptor{
		Type:   ycApi.RegistrarTypeYandexDns,
		Config: api.Config{Config: cfg},
	}
	r, err := ycInternal.Registrar(pctx, desc, pApi.ProvisionParams{Log: logger.New()})
	if err != nil {
		p.progress.Error("provision-registrar", "init failed: "+err.Error())
		return nil, errors.Wrap(err, "init yandex registrar")
	}
	p.mu.Lock()
	// Re-initialising a zone replaces its entry rather than adding a second one.
	p.registrars = append(lo.Filter(p.registrars, func(existing pApi.Registrar, _ int) bool {
		return existing.MainDomain() != r.MainDomain()
	}), r)
	p.mu.Unlock()
	p.progress.Info("provision-registrar", fmt.Sprintf("ready; main domain is %q", r.MainDomain()))
	return &scpluginv1.ProvisionRegistrarResponse{MainDomain: r.MainDomain()}, nil
}

func (p *ycPlugin) NewDnsRecord(ctx context.Context, req *scpluginv1.NewDnsRecordRequest) (resp *scpluginv1.NewDnsRecordResponse, err error) {
	rctx, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)
	r := p.registrarFor(req.GetRecordName())
	if r == nil {
		return nil, errors.New("NewDnsRecord called before ProvisionRegistrar")
	}
	p.progress.Info("dns-record", fmt.Sprintf("creating %s record %q → %q", req.GetRecordType(), req.GetRecordName(), req.GetValueResolved()))
	out, err := r.NewRecord(rctx.Pulumi, api.DnsRecord{
		Name:    req.GetRecordName(),
		Type:    req.GetRecordType(),
		Value:   req.GetValueResolved(),
		Proxied: req.GetProxied(),
	})
	if err != nil {
		p.progress.Error("dns-record", fmt.Sprintf("record %q failed: %v", req.GetRecordName(), err))
		return nil, err
	}
	urn := waitForComposite(out.Ref)
	p.progress.Info("dns-record", fmt.Sprintf("record %q registered (urn=%s)", req.GetRecordName(), urn))
	return &scpluginv1.NewDnsRecordResponse{Urn: urn}, nil
}

func (p *ycPlugin) NewOverrideHeaderRule(ctx context.Context, req *scpluginv1.NewOverrideHeaderRuleRequest) (resp *scpluginv1.NewOverrideHeaderRuleResponse, err error) {
	rctx, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)
	r := p.registrarFor(req.GetFromHost())
	if r == nil {
		return nil, errors.New("NewOverrideHeaderRule called before ProvisionRegistrar")
	}
	rule := pApi.OverrideHeaderRule{
		Name:       req.GetRuleName(),
		FromHost:   req.GetFromHost(),
		ToHost:     sdk.String(req.GetToHostResolved()),
		PathPrefix: req.GetPathPrefix(),
	}
	if ba := req.GetBasicAuth(); ba != nil {
		rule.BasicAuth = &pApi.BasicAuth{Username: ba.GetUsername(), Password: ba.GetPassword(), Realm: ba.GetRealm()}
	}
	if op := req.GetOverridePages(); op != nil {
		rule.OverridePages = &pApi.OverridePagesRule{IndexPage: op.GetIndexPage(), NotFoundPage: op.GetNotFoundPage()}
	}
	p.progress.Info("worker-route", fmt.Sprintf("overriding host %q → %q (rule %q)", rule.FromHost, req.GetToHostResolved(), rule.Name))
	out, err := r.NewOverrideHeaderRule(rctx.Pulumi, api.Stack{Name: req.GetStackName()}, rule)
	if err != nil {
		p.progress.Error("worker-route", fmt.Sprintf("rule %q failed: %v", rule.Name, err))
		return nil, err
	}
	urn := waitForComposite(out.Ref)
	p.progress.Info("worker-route", fmt.Sprintf("rule %q registered (urn=%s)", rule.Name, urn))
	return &scpluginv1.NewOverrideHeaderRuleResponse{Urn: urn}, nil
}

func (p *ycPlugin) ProvisionDomainForEndpoint(ctx context.Context, req *scpluginv1.ProvisionDomainForEndpointRequest) (resp *scpluginv1.ProvisionDomainForEndpointResponse, err error) {
	rctx, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)
	r := p.registrarFor(req.GetDomain())
	if r == nil {
		return nil, errors.New("ProvisionDomainForEndpoint called before ProvisionRegistrar")
	}
	endpoint := pApi.DomainEndpoint{
		Name:       req.GetEndpointName(),
		Domain:     req.GetDomain(),
		TargetHost: sdk.String(req.GetTargetHostResolved()),
		WebSocket:  req.GetWebsocket(),
	}
	p.progress.Info("endpoint", fmt.Sprintf("provisioning domain %q for endpoint %q (target %q)", endpoint.Domain, endpoint.Name, req.GetTargetHostResolved()))
	out, err := r.ProvisionDomainForEndpoint(rctx.Pulumi, api.Stack{Name: req.GetStackName()}, endpoint)
	if err != nil {
		p.progress.Error("endpoint", fmt.Sprintf("endpoint %q failed: %v", endpoint.Name, err))
		return nil, err
	}
	urn := waitForComposite(out.Ref)
	p.progress.Info("endpoint", fmt.Sprintf("endpoint %q bound (urn=%s)", endpoint.Name, urn))
	return &scpluginv1.ProvisionDomainForEndpointResponse{Urn: urn}, nil
}

// Provider — register the explicit pulumi:providers:yandex resource
// against the shared Pulumi monitor. SC core records the returned URN
// and threads it into every downstream Provision call via
// RequestHeader.provider_urns.
func (p *ycPlugin) Provider(ctx context.Context, req *scpluginv1.ProviderRequest) (resp *scpluginv1.ProviderResponse, err error) {
	rctx, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)
	var acct ycApi.AccountConfig
	if err := decodeInputsAs(req.GetInputs(), &acct); err != nil {
		return nil, errors.Wrap(err, "decode provider auth config")
	}
	input := api.ResourceInput{
		Descriptor: &api.ResourceDescriptor{
			Type:   ycApi.ProviderType,
			Name:   req.GetProviderResourceName(),
			Config: api.Config{Config: &acct},
		},
		StackParams: &api.StackParams{StackName: req.GetHeader().GetStackName(), Environment: req.GetHeader().GetEnvironment()},
	}
	p.progress.Info("provider", fmt.Sprintf("initialising yandex provider %q (folder=%q)", req.GetProviderResourceName(), acct.FolderID))
	out, err := ycInternal.Provider(rctx.Pulumi, api.Stack{Name: req.GetHeader().GetStackName()}, input, pApi.ProvisionParams{Log: logger.New()})
	if err != nil {
		p.progress.Error("provider", fmt.Sprintf("provider init failed: %v", err))
		return nil, errors.Wrap(err, "init yandex provider")
	}
	urn := waitForComposite(out.Ref)
	p.progress.Info("provider", fmt.Sprintf("provider %q ready (urn=%s)", req.GetProviderResourceName(), urn))
	return &scpluginv1.ProviderResponse{ProviderUrn: urn}, nil
}

// Provision — dispatch to the lifted bucket / serverless-container
// driver based on resource_type. The sidecar on SC core routes types
// advertised in Schema.resource_types here.
//
// Because Schema omits ProviderTypes, the plugin owns Provider
// construction internally. The provider resource is built in the SAME
// pctx the resource is created in, so params.Provider is a live handle
// (not a cross-pctx URN wrapper). The provider's Pulumi resource name
// is the folder id — identical to pre-split, so the URN is byte-
// identical and no state replace+create fires on first plugin-era deploy.
func (p *ycPlugin) Provision(ctx context.Context, req *scpluginv1.ProvisionRequest) (resp *scpluginv1.ProvisionResponse, err error) {
	rctx, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)
	stack := api.Stack{Name: req.GetHeader().GetStackName()}
	stackParams := &api.StackParams{StackName: req.GetHeader().GetStackName(), Environment: req.GetHeader().GetEnvironment()}
	switch req.GetResourceType() {
	case ycApi.ResourceTypeObjectStorageBucket:
		var bucket ycApi.ObjectStorageBucket
		if err := decodeInputsAs(req.GetInputs(), &bucket); err != nil {
			return nil, errors.Wrap(err, "decode bucket config")
		}
		provider, err := p.buildProvider(rctx.Pulumi, &bucket.AccountConfig, "bucket", req.GetHeader().GetStackName(), req.GetHeader().GetEnvironment(), req.GetResourceType(), req.GetResourceName())
		if err != nil {
			return nil, err
		}
		input := api.ResourceInput{
			Descriptor:  &api.ResourceDescriptor{Type: req.GetResourceType(), Name: req.GetResourceName(), Config: api.Config{Config: &bucket}},
			StackParams: stackParams,
		}
		p.progress.Info("bucket", fmt.Sprintf("provisioning object-storage bucket %q", req.GetResourceName()))
		out, err := ycInternal.ObjectStorageBucket(rctx.Pulumi, stack, input, pApi.ProvisionParams{Log: logger.New(), Provider: provider})
		if err != nil {
			p.progress.Error("bucket", fmt.Sprintf("bucket %q failed: %v", req.GetResourceName(), err))
			return nil, err
		}
		urn := waitForComposite(out.Ref)
		p.progress.Info("bucket", fmt.Sprintf("bucket %q ready (urn=%s)", req.GetResourceName(), urn))
		return &scpluginv1.ProvisionResponse{Urn: urn}, nil
	case ycApi.TemplateTypeYandexServerlessContainer:
		return p.provisionServerlessContainer(rctx.Pulumi, req)
	case ycApi.ResourceTypeYandexContainerRegistry:
		var reg ycApi.ContainerRegistry
		if err := decodeInputsAs(req.GetInputs(), &reg); err != nil {
			return nil, errors.Wrap(err, "decode container-registry config")
		}
		provider, err := p.buildProvider(rctx.Pulumi, &reg.AccountConfig, "container-registry", req.GetHeader().GetStackName(), req.GetHeader().GetEnvironment(), req.GetResourceType(), req.GetResourceName())
		if err != nil {
			return nil, err
		}
		input := api.ResourceInput{
			Descriptor:  &api.ResourceDescriptor{Type: req.GetResourceType(), Name: req.GetResourceName(), Config: api.Config{Config: &reg}},
			StackParams: stackParams,
		}
		p.progress.Info("container-registry", fmt.Sprintf("provisioning container-registry %q", req.GetResourceName()))
		out, err := ycInternal.ContainerRegistry(rctx.Pulumi, stack, input, pApi.ProvisionParams{Log: logger.New(), Provider: provider})
		if err != nil {
			p.progress.Error("container-registry", fmt.Sprintf("container-registry %q failed: %v", req.GetResourceName(), err))
			return nil, err
		}
		urn := waitForComposite(out.Ref)
		p.progress.Info("container-registry", fmt.Sprintf("container-registry %q ready (urn=%s)", req.GetResourceName(), urn))
		return &scpluginv1.ProvisionResponse{Urn: urn}, nil
	case ycApi.ResourceTypeWildcardCert:
		return p.provisionWildcardCert(ctx, rctx.Pulumi, req, stack, stackParams)
	case ycApi.TemplateTypeStaticWebsite:
		return p.provisionStaticWebsite(ctx, rctx.Pulumi, req, stack, stackParams)
	default:
		return nil, errors.Errorf("yandex plugin does not advertise resource type %q", req.GetResourceType())
	}
}

// provisionWildcardCert dispatches the yc-wildcard-cert resource to its
// driver. Opens the plugin's own provider against the cert's AccountConfig
// so the cert + ACME recordset are provisioned in the operator's folder,
// not whatever provider SC core happened to seed on this handler.
//
// The response carries runtime.HarvestExports output as ProvisionResponse.Outputs
// so SC core re-exports the cert id onto the parent stack root for `uses:`
// consumers (trap 3 — api PR #527 plumbing).
func (p *ycPlugin) provisionWildcardCert(stdctx context.Context, pctx *sdk.Context, req *scpluginv1.ProvisionRequest, stack api.Stack, stackParams *api.StackParams) (*scpluginv1.ProvisionResponse, error) {
	var cfg ycApi.WildcardCertConfig
	if err := decodeInputsAs(req.GetInputs(), &cfg); err != nil {
		return nil, errors.Wrap(err, "decode yc-wildcard-cert config")
	}
	input := api.ResourceInput{
		Descriptor:  &api.ResourceDescriptor{Type: req.GetResourceType(), Name: req.GetResourceName(), Config: api.Config{Config: &cfg}},
		StackParams: stackParams,
	}
	p.progress.Info("wildcard-cert", fmt.Sprintf("provisioning yc-wildcard-cert %q for zone %q", req.GetResourceName(), cfg.ZoneName))
	out, err := ycInternal.WildcardCert(pctx, stack, input, pApi.ProvisionParams{Log: logger.New()})
	if err != nil {
		p.progress.Error("wildcard-cert", fmt.Sprintf("%q failed: %v", req.GetResourceName(), err))
		return nil, err
	}
	urn := waitForComposite(out.Ref)
	outputs, err := runtime.HarvestExports(stdctx, pctx)
	if err != nil {
		return nil, errors.Wrap(err, "harvest yc-wildcard-cert exports")
	}
	p.progress.Info("wildcard-cert", fmt.Sprintf("yc-wildcard-cert %q ready (urn=%s, %d output(s))", req.GetResourceName(), urn, len(outputs)))
	return &scpluginv1.ProvisionResponse{Urn: urn, Outputs: outputs}, nil
}

// provisionStaticWebsite dispatches the yc-static-website template to its
// driver. The template reads the shared wildcard cert id via a parent
// StackReference (see static_website.go's SharedCertResource handling).
func (p *ycPlugin) provisionStaticWebsite(stdctx context.Context, pctx *sdk.Context, req *scpluginv1.ProvisionRequest, stack api.Stack, stackParams *api.StackParams) (*scpluginv1.ProvisionResponse, error) {
	var cfg ycApi.StaticWebsiteInput
	if err := decodeInputsAs(req.GetInputs(), &cfg); err != nil {
		return nil, errors.Wrap(err, "decode yc-static-website config")
	}
	provider, err := p.buildProvider(pctx, &cfg.AccountConfig, "static-website", req.GetHeader().GetStackName(), req.GetHeader().GetEnvironment(), req.GetResourceType(), req.GetResourceName())
	if err != nil {
		return nil, err
	}
	input := api.ResourceInput{
		Descriptor:  &api.ResourceDescriptor{Type: req.GetResourceType(), Name: req.GetResourceName(), Config: api.Config{Config: &cfg}},
		StackParams: stackParams,
	}
	// Pick the registrar whose zone serves this stack's domain. Required —
	// the static-site driver creates one DNS record (CNAME or ANAME) via
	// the registrar's NewRecord.
	registrar := p.registrarForDomain(cfg.EffectiveDomain())
	if registrar == nil {
		return nil, errors.Errorf("no yc-dns registrar initialised that serves %q — add a yc-dns registrar for that zone to the parent server.yaml", cfg.EffectiveDomain())
	}
	p.progress.Info("static-website", fmt.Sprintf("provisioning yc-static-website %q for domain %q", req.GetResourceName(), cfg.EffectiveDomain()))
	out, err := ycInternal.StaticWebsite(pctx, stack, input, pApi.ProvisionParams{
		Log:            logger.New(),
		Provider:       provider,
		ComputeContext: runtime.NewProvisionCollector(req),
		Registrar:      registrar,
	})
	if err != nil {
		p.progress.Error("static-website", fmt.Sprintf("%q failed: %v", req.GetResourceName(), err))
		return nil, err
	}
	urn := waitForComposite(out.Ref)
	outputs, err := runtime.HarvestExports(stdctx, pctx)
	if err != nil {
		return nil, errors.Wrap(err, "harvest yc-static-website exports")
	}
	p.progress.Info("static-website", fmt.Sprintf("yc-static-website %q ready (urn=%s, %d output(s))", req.GetResourceName(), urn, len(outputs)))
	return &scpluginv1.ProvisionResponse{Urn: urn, Outputs: outputs}, nil
}

// provisionServerlessContainer is the yc-serverless-container branch of
// Provision, split out so a test can drive it against Pulumi mocks with the
// exact request SC core puts on the bus.
func (p *ycPlugin) provisionServerlessContainer(pctx *sdk.Context, req *scpluginv1.ProvisionRequest) (*scpluginv1.ProvisionResponse, error) {
	stack := api.Stack{Name: req.GetHeader().GetStackName()}
	stackParams := &api.StackParams{StackName: req.GetHeader().GetStackName(), Environment: req.GetHeader().GetEnvironment()}
	// The deploy input is SC core's converted ServerlessContainerInput (the
	// template's account config + the client's whole stackConfig), not the
	// bare TemplateConfig: decoding into TemplateConfig lost the image, env
	// and secrets, and ServerlessContainer refused the type.
	var tpl ycApi.ServerlessContainerInput
	if err := decodeInputsAs(req.GetInputs(), &tpl); err != nil {
		return nil, errors.Wrap(err, "decode serverless-container input")
	}
	provider, err := p.buildProvider(pctx, &tpl.AccountConfig, "serverless-container", req.GetHeader().GetStackName(), req.GetHeader().GetEnvironment(), req.GetResourceType(), req.GetResourceName())
	if err != nil {
		return nil, err
	}
	input := api.ResourceInput{
		Descriptor:  &api.ResourceDescriptor{Type: req.GetResourceType(), Name: req.GetResourceName(), Config: api.Config{Config: &tpl}},
		StackParams: stackParams,
	}
	p.progress.Info("serverless-container", fmt.Sprintf("provisioning serverless container %q", req.GetResourceName()))
	// The in-tree template got SC core's registrar in params.Registrar and handed
	// it the custom domain. Nothing crosses the bus for it, so the plugin uses the
	// yc-dns registrar it holds for that domain's zone. Left nil when it holds
	// none, which ServerlessContainer reports as an error instead of
	// dereferencing it — the nil deref was what took this process down.
	var registrar pApi.Registrar
	if domain := tpl.StackConfig.Domain; domain != "" {
		registrar = p.registrarForDomain(domain)
	}
	out, err := ycInternal.ServerlessContainer(pctx, stack, input, pApi.ProvisionParams{
		Log:            logger.New(),
		Provider:       provider,
		ComputeContext: runtime.NewProvisionCollector(req),
		Registrar:      registrar,
	})
	if err != nil {
		p.progress.Error("serverless-container", fmt.Sprintf("container %q failed: %v", req.GetResourceName(), err))
		return nil, err
	}
	urn := waitForComposite(out.Ref)
	p.progress.Info("serverless-container", fmt.Sprintf("container %q ready (urn=%s)", req.GetResourceName(), urn))
	// The ctx.Export of the container URL lands on this plugin's own Context, which
	// never registers stack outputs — so without this line the deploy log names no
	// URL at all, and a stack without `domain:` (served on its
	// *.containers.yandexcloud.net endpoint) has no way to find where it lives.
	if sc, ok := out.Ref.(*ycInternal.ServerlessContainerOutput); ok && sc.Container != nil {
		if u := waitForRef(sc.Container.Url); u != "" {
			p.progress.Info("serverless-container", fmt.Sprintf("container %q is served at %s", req.GetResourceName(), u))
		}
	}
	return &scpluginv1.ProvisionResponse{Urn: urn}, nil
}

// buildProvider instantiates pulumi:providers:yandex in the given pctx
// via the lifted Provider function. The Pulumi resource name is
// pcfg.FolderID (the lifted driver's convention), so the URN stays
// byte-identical to pre-split. `phase` is used only for progress logs.
// buildProvider instantiates pulumi:providers:yandex in the given pctx
// via the lifted Provider function. Uses SC core's naming formula —
// `<stack>--<resType>--<resName>--<env>--provider` — so the resulting
// Pulumi URN is byte-identical to what SC core's initProvider would have
// produced on the pre-split binary. The lifted driver appends `--<env>`
// via input.ToResName, matching initProvider's suffix behavior.
func (p *ycPlugin) buildProvider(pctx *sdk.Context, acct *ycApi.AccountConfig, phase, stackName, env, resType, resName string) (sdk.ProviderResource, error) {
	providerName := fmt.Sprintf("%s--%s--%s--%s--provider", stackName, resType, resName, env)
	p.progress.Info(phase, fmt.Sprintf("initialising yandex provider %q (folder=%q)", providerName, acct.FolderID))
	input := api.ResourceInput{
		Descriptor: &api.ResourceDescriptor{
			Type:   ycApi.ProviderType,
			Name:   providerName,
			Config: api.Config{Config: acct},
		},
		// Required: the lifted Provider calls input.ToResName which deref's
		// StackParams.Environment. nil StackParams panics the plugin.
		StackParams: &api.StackParams{StackName: stackName, Environment: env},
	}
	out, err := ycInternal.Provider(pctx, api.Stack{Name: stackName}, input, pApi.ProvisionParams{Log: logger.New()})
	if err != nil {
		p.progress.Error(phase, fmt.Sprintf("provider init failed: %v", err))
		return nil, errors.Wrap(err, "init yandex provider")
	}
	prov, ok := out.Ref.(sdk.ProviderResource)
	if !ok {
		return nil, errors.Errorf("yandex Provider returned %T, not sdk.ProviderResource", out.Ref)
	}
	return prov, nil
}

// ComputeProcess — the compute-side injection for a dependency bucket.
// SC core calls this during dependency resolution so the downstream
// serverless-container can see the bucket env vars.
func (p *ycPlugin) ComputeProcess(ctx context.Context, req *scpluginv1.ComputeProcessRequest) (resp *scpluginv1.ComputeProcessResponse, err error) {
	rctx, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)
	// Decode parent env for the StackParams swap: on an isolated-stack
	// consumer (e.g. forge-storage preview on a wf-id env), the bucket
	// exists in the PARENT's env and the compute-processor must look it
	// up under that name, not the child env. Same shape as the aws and
	// mongodb plugin ComputeProcess handlers (sc-plugin-aws v2026.11.01-92f5d1b).
	parent, err := runtime.ParentInfoFromRequest(req)
	if err != nil {
		return nil, err
	}
	stack := api.Stack{Name: req.GetHeader().GetStackName()}
	provisionParams := pApi.ProvisionParams{Log: logger.New(), ParentStack: parent}
	// runtime.RunComputeProcess owns the fn → drain → marshal sequence.
	// ObjectStorageBucketComputeProcessor builds every S3_* env var from
	// INSIDE an ApplyT on the parent bucket ref; the shared Collector
	// captures that output and awaitOutputs drains it before marshalling.
	// Previously the per-plugin collector had AddOutput as a no-op, so
	// those mutations would have silently been dropped — same shape as
	// the sc-plugin-aws trap-16 incident (agent-native-pdf-rendering run
	// f4355798, 2026-11-02). This path was latent only because no stack
	// currently declares uses: [<yc-bucket>].
	var handler runtime.ComputeProcessHandler
	var input api.ResourceInput
	switch req.GetResourceType() {
	case ycApi.ResourceTypeObjectStorageBucket:
		var bucket ycApi.ObjectStorageBucket
		if err := decodeInputsAs(req.GetInputs(), &bucket); err != nil {
			return nil, errors.Wrap(err, "decode bucket config for compute-process")
		}
		input = runtime.ResourceInputForComputeProcess(req, parent, &bucket)
		p.progress.Info("compute-process", fmt.Sprintf("compute-processing bucket %q", req.GetResourceName()))
		handler = ycInternal.ObjectStorageBucketComputeProcessor
	case ycApi.ResourceTypeWildcardCert:
		var cfg ycApi.WildcardCertConfig
		if err := decodeInputsAs(req.GetInputs(), &cfg); err != nil {
			return nil, errors.Wrap(err, "decode yc-wildcard-cert config for compute-process")
		}
		input = runtime.ResourceInputForComputeProcess(req, parent, &cfg)
		p.progress.Info("compute-process", fmt.Sprintf("compute-processing yc-wildcard-cert %q", req.GetResourceName()))
		handler = ycInternal.WildcardCertComputeProcessor
	default:
		return nil, errors.Errorf("yandex plugin does not advertise compute-processor for %q", req.GetResourceType())
	}
	resp, out, err := runtime.RunComputeProcess(ctx, rctx, stack, input, provisionParams, handler)
	if err != nil {
		return nil, err
	}
	resp.Urn = waitForComposite(out.Ref)
	p.progress.Info("compute-process", fmt.Sprintf("emitted %d env var(s) + %d tpl extension(s)", len(resp.GetEnvVars()), len(resp.GetTplExtensions())))
	return resp, nil
}

// ConvertStackConfig is the plugin-side half of SC core's A2 path
// for plugin-owned template types. Dispatches on template_type to
// the right ToXxxConfig closure, marshals the typed Input back as
// an InputValue tree, and lifts the three structured fields SC core
// wraps in pluginPreparedInput (base_dns_zone, uses,
// depends_on_resources). yandex's StackConfigCompose converter does
// not exist today — only single-image (yc-container) and static
// (yc-static-site). A branch for compose is reserved for whenever
// the compose variant lands.
func (p *ycPlugin) ConvertStackConfig(_ context.Context, req *scpluginv1.ConvertStackConfigRequest) (*scpluginv1.ConvertStackConfigResponse, error) {
	templateType := req.GetTemplateType()
	stackName := req.GetStackName()
	stackDir := req.GetStackDir()
	p.progress.Info("convert-stack-config", fmt.Sprintf("template=%q stack=%q", templateType, stackName))

	// Decode template_config into the type each `ToXxxConfig` converter
	// ACTUALLY type-asserts on. Historically this was hoisted above the
	// switch as a single `&ycApi.TemplateConfig{}` decode and the arms
	// relied on `yc-serverless-container`'s converter happening to accept
	// the generic type — `yc-static-website` doesn't, and silently fell
	// into "template config is not of type *yandex.StaticWebsiteTemplateConfig:
	// *yandex.TemplateConfig" on every landing-ru-apex deploy (2026-11-09).
	// Decode inside each arm matches init.go's ConfigRegisterMap wiring:
	// one registered reader per template type, concrete Go type matches.
	templatePlain := scPlugin.InputsToPlain(req.GetTemplateConfig())
	stackPlain := scPlugin.InputsToPlain(req.GetStackConfig())
	var typedInput any
	var cerr error
	switch templateType {
	case ycApi.TemplateTypeYandexServerlessContainer:
		tplCfg := &ycApi.TemplateConfig{}
		if err := yamlRoundTrip(templatePlain, tplCfg); err != nil {
			return nil, errors.Wrapf(err, "decode template_config for %q", templateType)
		}
		stackCfg := &api.StackConfigSingleImage{}
		if err := yamlRoundTrip(stackPlain, stackCfg); err != nil {
			return nil, errors.Wrap(err, "decode StackConfigSingleImage")
		}
		typedInput, cerr = ycApi.ToServerlessContainerConfig(tplCfg, stackCfg)
	case ycApi.TemplateTypeStaticWebsite:
		tplCfg := &ycApi.StaticWebsiteTemplateConfig{}
		if err := yamlRoundTrip(templatePlain, tplCfg); err != nil {
			return nil, errors.Wrapf(err, "decode template_config for %q", templateType)
		}
		stackCfg := &api.StackConfigStatic{}
		if err := yamlRoundTrip(stackPlain, stackCfg); err != nil {
			return nil, errors.Wrap(err, "decode StackConfigStatic")
		}
		typedInput, cerr = ycApi.ToStaticWebsiteConfig(tplCfg, stackDir, stackName, stackCfg)
	default:
		return nil, errors.Errorf("sc-plugin-yandex does not own template type %q", templateType)
	}
	if cerr != nil {
		return nil, errors.Wrapf(cerr, "convert %q for stack %q", templateType, stackName)
	}

	resp := &scpluginv1.ConvertStackConfigResponse{
		Input: scPlugin.PlainToInputs(typedInput),
	}
	if dca, ok := typedInput.(api.DnsConfigAware); ok {
		resp.BaseDnsZone = dca.OverriddenBaseZone()
	}
	if ra, ok := typedInput.(api.ResourceAware); ok {
		resp.Uses = ra.Uses()
	}
	if wdr, ok := typedInput.(api.WithDependsOnResources); ok {
		for _, dep := range wdr.DependsOnResources() {
			raw, jerr := json.Marshal(dep)
			if jerr != nil {
				return nil, errors.Wrap(jerr, "marshal depends-on entry")
			}
			resp.DependsOnResourcesJson = append(resp.DependsOnResourcesJson, raw)
		}
	}
	return resp, nil
}

// yamlRoundTrip decodes a plain map[string]any (what InputsToPlain
// returns from the bus's InputValue tree) into a typed struct via a
// YAML round-trip. Same shape pApi.ConvertConfig uses, lifted locally
// so this handler does not reach into pkg/api private helpers.
func yamlRoundTrip(src any, out any) error {
	b, err := yaml.Marshal(src)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}

// InitStateStore — pre-engine. The sidecar on SC core calls this and
// uses the returned env vars + backend URL to configure the Pulumi
// workspace. The plugin's own InitStateStore mutates process env
// (AWS_* for SigV4), which is fine because SC core and the plugin
// share the engine process via hashicorp/go-plugin.
func (p *ycPlugin) InitStateStore(ctx context.Context, req *scpluginv1.InitStateStoreRequest) (*scpluginv1.InitStateStoreResponse, error) {
	var cfg ycApi.StateStorageConfig
	if err := decodeInputsAs(req.GetConfig(), &cfg); err != nil {
		return nil, errors.Wrap(err, "decode state-storage config")
	}
	p.progress.Info("state-store", fmt.Sprintf("configuring yc-object-storage backend (bucket=%q region=%q)", cfg.BucketName, cfg.EffectiveRegion()))
	if err := ycInternal.InitStateStore(ctx, &cfg, logger.New()); err != nil {
		p.progress.Error("state-store", fmt.Sprintf("init failed: %v", err))
		return nil, err
	}
	// The driver already set AWS_* env vars in-process. Return the
	// Pulumi backend URL so SC core's workspace points at the YC
	// object-storage endpoint.
	return &scpluginv1.InitStateStoreResponse{BackendUrl: cfg.StorageUrl()}, nil
}

// decodeInputsAs lifts the plugin-bus input map into a typed Go value
// via YAML round-trip. YAML preserves `yaml:",inline"` tags that the
// yandex configs lean on heavily (AccountConfig embedded in bucket,
// StateStorageConfig, etc). The sidecar on SC core serializes via
// JSON into the primitives map; here we go map → YAML → typed struct.
func decodeInputsAs[T any](inputs map[string]*scpluginv1.InputValue, out *T) error {
	// InputsToPlain keeps nested maps and lists — a container's whole
	// `stackConfig` — which the old scalar-only decode silently dropped.
	bytes, err := yaml.Marshal(scPlugin.InputsToPlain(inputs))
	if err != nil {
		return err
	}
	return yaml.Unmarshal(bytes, out)
}

func (p *ycPlugin) LogStream(_ *scpluginv1.LogStreamRequest, stream scpluginv1.SCCloudPlugin_LogStreamServer) error {
	ctx := stream.Context()
	for {
		batch, ok := p.progress.Drain()
		if !ok {
			return nil
		}
		for _, ev := range batch {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// inputsToRegistrarConfig lifts the SC-plugin-bus input map into the typed
// yandex RegistrarConfig. V1 reads the four fields the lifted driver needs
// via Registrar(ctx, ...) — zoneName, folderId, serviceAccountKey, certificateId.
// Anything richer (per-record overrides, additional zones) rides server.yaml
// dnsRecords: and is captured on the SC-core side via api.RegistrarConfig's
// DnsRecords() interface — see pkg/clouds/pulumi/plugin_sidecars.go.
func inputsToRegistrarConfig(m map[string]*scpluginv1.InputValue) *ycApi.RegistrarConfig {
	return &ycApi.RegistrarConfig{
		AccountConfig: ycApi.AccountConfig{
			Credentials: api.Credentials{Credentials: strVal(m["credentials"])},
			FolderID:    strVal(m["folderId"]),
		},
		ZoneName:      strVal(m["zoneName"]),
		CertificateID: strVal(m["certificateId"]),
	}
}

func strVal(v *scpluginv1.InputValue) string {
	if v == nil {
		return ""
	}
	return v.GetStringValue()
}

// waitForComposite blocks until the IDs of every known composite output
// field resolve. Needed for ObjectStorageBucket (4 sub-resources),
// ServerlessContainer (container + revision + SA), and any other lifted
// surface that returns a struct of pulumi resources rather than a single
// IDOutput. Returns the first resource's URN for logging; the plugin's
// return value is coarse-grained since the SC-core-side consumer treats
// Ref as a bag.
func waitForComposite(ref any) string {
	if ref == nil {
		return ""
	}
	switch r := ref.(type) {
	case *ycInternal.ObjectStorageBucketOutput:
		return waitForIDs(r.Bucket.ID(), r.ServiceAccount.ID(), r.AccessKey.ID(), r.RoleBinding.ID())
	case *ycInternal.ContainerRegistryOutput:
		return waitForIDs(r.Registry.ID(), r.ServiceAccount.ID(), r.AccessKey.ID(), r.RoleBinding.ID())
	case *ycInternal.ServerlessContainerOutput:
		// Container is always present. Secret + SecretVersion only exist when
		// the config carried secrets. Triggers are a slice — iterate and append IDs.
		// ServiceAccount is nil when the template names a pre-existing
		// serviceAccountId, Registry when it names a registryId: SC created
		// neither, so there is nothing to wait for.
		ids := []sdk.IDOutput{r.Container.ID()}
		if r.ServiceAccount != nil {
			ids = append(ids, r.ServiceAccount.ID())
		}
		if r.Registry != nil {
			ids = append(ids, r.Registry.ID())
		}
		if r.Secret != nil {
			ids = append(ids, r.Secret.ID())
		}
		if r.SecretVersion != nil {
			ids = append(ids, r.SecretVersion.ID())
		}
		for _, t := range r.Triggers {
			if t != nil {
				ids = append(ids, t.ID())
			}
		}
		return waitForIDs(ids...)
	default:
		return waitForRef(ref)
	}
}

// waitForIDs blocks until every passed IDOutput resolves. Each ApplyT
// closure signals once; the aggregator closes the done channel when the
// last signal arrives. Returns the first ID as the headline URN.
func waitForIDs(ids ...sdk.IDOutput) string {
	if len(ids) == 0 {
		return ""
	}
	remaining := len(ids)
	done := make(chan struct{})
	var mu sync.Mutex
	var first string
	for i, id := range ids {
		i, id := i, id
		id.ApplyT(func(idv sdk.ID) sdk.ID {
			mu.Lock()
			if i == 0 {
				first = string(idv)
			}
			remaining--
			if remaining == 0 {
				close(done)
			}
			mu.Unlock()
			return idv
		})
	}
	<-done
	return first
}

// waitForRef blocks until a Pulumi Output in a resource ref resolves. See the
// sister function in sc-plugin-cloudflare for why: Pulumi's SDK fires
// RegisterResource inside an async goroutine, so returning before the output
// resolves and letting pctx.Close() run tears down the engine connection
// mid-registration — the request vanishes silently.
func waitForRef(ref any) string {
	if ref == nil {
		return ""
	}
	switch r := ref.(type) {
	case string:
		return r
	case sdk.IDOutput:
		ch := make(chan string, 1)
		r.ApplyT(func(id sdk.ID) sdk.ID {
			select {
			case ch <- string(id):
			default:
			}
			return id
		})
		return <-ch
	case sdk.URNOutput:
		ch := make(chan string, 1)
		r.ApplyT(func(u sdk.URN) sdk.URN {
			select {
			case ch <- string(u):
			default:
			}
			return u
		})
		return <-ch
	case sdk.StringOutput:
		ch := make(chan string, 1)
		r.ApplyT(func(s string) string {
			select {
			case ch <- s:
			default:
			}
			return s
		})
		return <-ch
	default:
		return fmt.Sprintf("%v", r)
	}
}

func main() {
	impl := &ycPlugin{
		progress: scPlugin.NewPublisher(0),
	}
	scPlugin.Serve(scPlugin.ServerConfig{
		Impl: impl,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "sc-plugin-yandex",
			Level:  hclog.Warn,
			Output: os.Stderr,
		}),
	})
}
