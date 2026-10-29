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
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"

	ycApi "github.com/simple-container-com/sc-plugin-yandex/api"
	ycInternal "github.com/simple-container-com/sc-plugin-yandex/plugin/internal"
)

type ycPlugin struct {
	scpluginv1.UnimplementedSCCloudPluginServer

	mu        sync.Mutex
	registrar pApi.Registrar
	config    *ycApi.RegistrarConfig
	progress  *scPlugin.Publisher
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
		RegistrarTypes:        []string{ycApi.RegistrarTypeYandexDns},
		ResourceTypes:         []string{ycApi.ResourceTypeObjectStorageBucket, ycApi.TemplateTypeYandexServerlessContainer, ycApi.TemplateTypeYandexStaticWebsite},
		ComputeProcessorTypes: []string{ycApi.ResourceTypeObjectStorageBucket},
		PulumiPlugins: []*scpluginv1.PulumiPluginRequirement{
			// simple-container-com/pulumi-yandex v1.1.0 — the static bridge over
			// terraform-provider-yandex (pulumi/pulumi-yandex is archived at v0.13.0).
			{Package: "yandex", Version: "1.1.0"},
		},
	}, nil
}

func (p *ycPlugin) pulumiContextFromHeader(ctx context.Context, h *scpluginv1.RequestHeader) (*sdk.Context, func(), error) {
	if h.GetMonitorAddress() == "" {
		return nil, nil, errors.New("no monitor address in request header")
	}
	pctx, err := sdk.NewContext(ctx, sdk.RunInfo{
		MonitorAddr: h.GetMonitorAddress(),
		Project:     h.GetProjectName(),
		Stack:       h.GetStackName(),
		// A preview must not run side effects outside the engine (the static
		// site's upload checks ctx.DryRun()); core sets this from its own context.
		DryRun: h.GetDryRun(),
	})
	if err != nil {
		return nil, nil, errors.Wrap(err, "open pulumi context from monitor address")
	}
	return pctx, func() { pctx.Close() }, nil
}

func (p *ycPlugin) ProvisionRegistrar(ctx context.Context, req *scpluginv1.ProvisionRegistrarRequest) (*scpluginv1.ProvisionRegistrarResponse, error) {
	p.progress.Info("provision-registrar", fmt.Sprintf("initialising yandex registrar for zone %q", strVal(req.GetConfig()["zoneName"])))
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()

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
	p.registrar = r
	p.config = cfg
	p.mu.Unlock()
	p.progress.Info("provision-registrar", fmt.Sprintf("ready; main domain is %q", r.MainDomain()))
	return &scpluginv1.ProvisionRegistrarResponse{MainDomain: r.MainDomain()}, nil
}

func (p *ycPlugin) NewDnsRecord(ctx context.Context, req *scpluginv1.NewDnsRecordRequest) (*scpluginv1.NewDnsRecordResponse, error) {
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()
	p.mu.Lock()
	r := p.registrar
	p.mu.Unlock()
	if r == nil {
		return nil, errors.New("NewDnsRecord called before ProvisionRegistrar")
	}
	p.progress.Info("dns-record", fmt.Sprintf("creating %s record %q → %q", req.GetRecordType(), req.GetRecordName(), req.GetValueResolved()))
	out, err := r.NewRecord(pctx, api.DnsRecord{
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

func (p *ycPlugin) NewOverrideHeaderRule(ctx context.Context, req *scpluginv1.NewOverrideHeaderRuleRequest) (*scpluginv1.NewOverrideHeaderRuleResponse, error) {
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()
	p.mu.Lock()
	r := p.registrar
	p.mu.Unlock()
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
	out, err := r.NewOverrideHeaderRule(pctx, api.Stack{Name: req.GetStackName()}, rule)
	if err != nil {
		p.progress.Error("worker-route", fmt.Sprintf("rule %q failed: %v", rule.Name, err))
		return nil, err
	}
	urn := waitForComposite(out.Ref)
	p.progress.Info("worker-route", fmt.Sprintf("rule %q registered (urn=%s)", rule.Name, urn))
	return &scpluginv1.NewOverrideHeaderRuleResponse{Urn: urn}, nil
}

func (p *ycPlugin) ProvisionDomainForEndpoint(ctx context.Context, req *scpluginv1.ProvisionDomainForEndpointRequest) (*scpluginv1.ProvisionDomainForEndpointResponse, error) {
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()
	p.mu.Lock()
	r := p.registrar
	p.mu.Unlock()
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
	out, err := r.ProvisionDomainForEndpoint(pctx, api.Stack{Name: req.GetStackName()}, endpoint)
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
func (p *ycPlugin) Provider(ctx context.Context, req *scpluginv1.ProviderRequest) (*scpluginv1.ProviderResponse, error) {
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()
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
	out, err := ycInternal.Provider(pctx, api.Stack{Name: req.GetHeader().GetStackName()}, input, pApi.ProvisionParams{Log: logger.New()})
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
func (p *ycPlugin) Provision(ctx context.Context, req *scpluginv1.ProvisionRequest) (*scpluginv1.ProvisionResponse, error) {
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()
	stack := api.Stack{Name: req.GetHeader().GetStackName()}
	stackParams := &api.StackParams{StackName: req.GetHeader().GetStackName(), Environment: req.GetHeader().GetEnvironment()}
	switch req.GetResourceType() {
	case ycApi.ResourceTypeObjectStorageBucket:
		var bucket ycApi.ObjectStorageBucket
		if err := decodeInputsAs(req.GetInputs(), &bucket); err != nil {
			return nil, errors.Wrap(err, "decode bucket config")
		}
		provider, err := p.buildProvider(pctx, &bucket.AccountConfig, "bucket", req.GetHeader().GetStackName(), req.GetHeader().GetEnvironment(), req.GetResourceType(), req.GetResourceName())
		if err != nil {
			return nil, err
		}
		input := api.ResourceInput{
			Descriptor:  &api.ResourceDescriptor{Type: req.GetResourceType(), Name: req.GetResourceName(), Config: api.Config{Config: &bucket}},
			StackParams: stackParams,
		}
		p.progress.Info("bucket", fmt.Sprintf("provisioning object-storage bucket %q", req.GetResourceName()))
		out, err := ycInternal.ObjectStorageBucket(pctx, stack, input, pApi.ProvisionParams{Log: logger.New(), Provider: provider})
		if err != nil {
			p.progress.Error("bucket", fmt.Sprintf("bucket %q failed: %v", req.GetResourceName(), err))
			return nil, err
		}
		urn := waitForComposite(out.Ref)
		p.progress.Info("bucket", fmt.Sprintf("bucket %q ready (urn=%s)", req.GetResourceName(), urn))
		return &scpluginv1.ProvisionResponse{Urn: urn}, nil
	case ycApi.TemplateTypeYandexServerlessContainer:
		var tpl ycApi.TemplateConfig
		if err := decodeInputsAs(req.GetInputs(), &tpl); err != nil {
			return nil, errors.Wrap(err, "decode serverless-container template config")
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
		out, err := ycInternal.ServerlessContainer(pctx, stack, input, pApi.ProvisionParams{Log: logger.New(), Provider: provider})
		if err != nil {
			p.progress.Error("serverless-container", fmt.Sprintf("container %q failed: %v", req.GetResourceName(), err))
			return nil, err
		}
		urn := waitForComposite(out.Ref)
		p.progress.Info("serverless-container", fmt.Sprintf("container %q ready (urn=%s)", req.GetResourceName(), urn))
		return &scpluginv1.ProvisionResponse{Urn: urn}, nil
	case ycApi.TemplateTypeYandexStaticWebsite:
		var site ycApi.StaticWebsiteInput
		if err := decodeInputsAs(req.GetInputs(), &site); err != nil {
			return nil, errors.Wrap(err, "decode static website config")
		}
		provider, err := p.buildProvider(pctx, &site.AccountConfig, "static-website", req.GetHeader().GetStackName(), req.GetHeader().GetEnvironment(), req.GetResourceType(), req.GetResourceName())
		if err != nil {
			return nil, err
		}
		input := api.ResourceInput{
			Descriptor:  &api.ResourceDescriptor{Type: req.GetResourceType(), Name: req.GetResourceName(), Config: api.Config{Config: &site}},
			StackParams: stackParams,
		}
		p.progress.Info("static-website", fmt.Sprintf("provisioning static website %q", site.Site.Domain))
		out, err := ycInternal.StaticWebsite(pctx, stack, input, pApi.ProvisionParams{Log: logger.New(), Provider: provider})
		if err != nil {
			p.progress.Error("static-website", fmt.Sprintf("static website %q failed: %v", site.Site.Domain, err))
			return nil, err
		}
		// AwaitAll, not waitForComposite: in a preview a new site's ids resolve
		// unknown, and an ApplyT-based wait would block forever on them.
		if err := scPlugin.AwaitAll(ctx, out.Ref.(*ycInternal.StaticWebsiteOutput).Outputs()...); err != nil {
			p.progress.Error("static-website", fmt.Sprintf("static website %q failed: %v", site.Site.Domain, err))
			return nil, err
		}
		urn, err := scPlugin.AwaitURN(ctx, out.Ref.(*ycInternal.StaticWebsiteOutput).Bucket)
		if err != nil {
			return nil, err
		}
		p.progress.Info("static-website", fmt.Sprintf("static website https://%s ready (urn=%s)", site.Site.Domain, urn))
		return &scpluginv1.ProvisionResponse{Urn: urn}, nil
	default:
		return nil, errors.Errorf("yandex plugin does not advertise resource type %q", req.GetResourceType())
	}
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
func (p *ycPlugin) ComputeProcess(ctx context.Context, req *scpluginv1.ComputeProcessRequest) (*scpluginv1.ComputeProcessResponse, error) {
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()
	if req.GetResourceType() != ycApi.ResourceTypeObjectStorageBucket {
		return nil, errors.Errorf("yandex plugin does not advertise compute-processor for %q", req.GetResourceType())
	}
	var bucket ycApi.ObjectStorageBucket
	if err := decodeInputsAs(req.GetInputs(), &bucket); err != nil {
		return nil, errors.Wrap(err, "decode bucket config for compute-process")
	}
	input := api.ResourceInput{
		Descriptor:  &api.ResourceDescriptor{Type: req.GetResourceType(), Name: req.GetResourceName(), Config: api.Config{Config: &bucket}},
		StackParams: &api.StackParams{StackName: req.GetHeader().GetStackName(), Environment: req.GetHeader().GetEnvironment()},
	}
	stack := api.Stack{Name: req.GetHeader().GetStackName()}
	p.progress.Info("compute-process", fmt.Sprintf("compute-processing bucket %q", req.GetResourceName()))
	out, err := ycInternal.ObjectStorageBucketComputeProcessor(pctx, stack, input, nil, pApi.ProvisionParams{Log: logger.New()})
	if err != nil {
		return nil, err
	}
	urn := waitForComposite(out.Ref)
	return &scpluginv1.ComputeProcessResponse{Urn: urn}, nil
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
	// InputsToPlain keeps nested maps and lists — a static site's `site` block, a
	// container's whole `stackConfig` — which a scalar-only decode dropped.
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
	case *ycInternal.ServerlessContainerOutput:
		// Container is always present. ServiceAccount + Registry are always
		// created. Secret + SecretVersion only exist when the config carried
		// secrets. Triggers are a slice — iterate and append IDs.
		ids := []sdk.IDOutput{r.Container.ID(), r.ServiceAccount.ID(), r.Registry.ID()}
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
