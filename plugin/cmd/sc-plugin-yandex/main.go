// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// sc-plugin-yandex is the Yandex Cloud driver plugin for Simple Container,
// extracted from pkg/clouds/{yandex,pulumi/yandex} per the Phase 3 extraction
// in forge/docs/design/2026-10-24/sc-plugin-architecture/.
//
// V1 scope: registrar surface (type `yc-dns`) only. Follow-ups add:
//   - pulumi:providers:yandex (Provider RPC)
//   - yc-bucket (Provision + ComputeProcess)
//   - yc-serverless-container (Provision)
//   - yc-object-storage state store (InitStateStore)
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
		PluginVersion:  "yandex-0.0.1",
		RegistrarTypes: []string{ycApi.RegistrarTypeYandexDns},
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
	urn := waitForRef(out.Ref)
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
	urn := waitForRef(out.Ref)
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
	urn := waitForRef(out.Ref)
	p.progress.Info("endpoint", fmt.Sprintf("endpoint %q bound (urn=%s)", endpoint.Name, urn))
	return &scpluginv1.ProvisionDomainForEndpointResponse{Urn: urn}, nil
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
