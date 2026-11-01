// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"sync"

	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// capturingCollector mirrors sc-plugin-aws/plugin/cmd/sc-plugin-aws/collector.go
// (trap 14 shape). Two pools:
//
//   - incoming (seeded from ProvisionRequest.compute_env_vars) — what the
//     per-stack yc-static-website driver reads via EnvVariables()/
//     SecretEnvVariables() to inject into the deployed site's bucket (via
//     local.NewCommand env), e.g. the shared wildcard-cert id emitted by
//     WildcardCertComputeProcessor as a resource tpl extension.
//
//   - outgoing (AddEnvVariableIfNotExist / AddSecretEnvVariableIfNotExist
//
//   - AddResourceTplExtension / AddDependencyTplExtension) — what
//     ComputeProcess returns in ComputeProcessResponse for SC core to
//     re-emit on the consuming stack.
//
// The split exists because a plugin that both PROVISIONS templates (reads
// incoming) AND runs COMPUTE PROCESSORS (writes outgoing) in the same
// deploy must not confuse the two pools. See the aws plugin's doc +
// forge/docs/design/2026-11-01/plugin-extraction-playbook/ trap 14 for the
// forge-notifier incident this shape prevents.
type capturingCollector struct {
	mu sync.Mutex

	incomingEnvVars []pApi.ComputeEnvVariable
	incomingSecrets []pApi.ComputeEnvVariable

	outgoingEnvVars []pApi.ComputeEnvVariable
	resourceTpl     []resourceTplEntry
	dependencyTpl   []dependencyTplEntry
}

type resourceTplEntry struct {
	resName string
	values  map[string]string
}

type dependencyTplEntry struct {
	depName string
	resName string
	values  map[string]string
}

func newCapturingCollector() *capturingCollector {
	return &capturingCollector{}
}

// newProvisionCollector seeds the INCOMING pool from the Provision request's
// compute env vars so the per-stack yc-static-website driver reads what its
// `uses:` resources contributed. Unconditional across cfg types — the trap
// 14 fix.
func newProvisionCollector(req *scpluginv1.ProvisionRequest) *capturingCollector {
	c := newCapturingCollector()
	for _, v := range req.GetComputeEnvVars() {
		if v == nil || v.GetName() == "" {
			continue
		}
		entry := pApi.ComputeEnvVariable{
			Name:         v.GetName(),
			Value:        v.GetValue(),
			Secret:       v.GetSecret(),
			ResourceName: v.GetResourceName(),
			ResourceType: v.GetResourceType(),
			StackName:    v.GetStackName(),
		}
		if v.GetSecret() {
			c.incomingSecrets = append(c.incomingSecrets, entry)
		} else {
			c.incomingEnvVars = append(c.incomingEnvVars, entry)
		}
	}
	return c
}

func (c *capturingCollector) EnvVariables() []pApi.ComputeEnvVariable {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]pApi.ComputeEnvVariable(nil), c.incomingEnvVars...)
}

func (c *capturingCollector) SecretEnvVariables() []pApi.ComputeEnvVariable {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]pApi.ComputeEnvVariable(nil), c.incomingSecrets...)
}

func (c *capturingCollector) AddEnvVariableIfNotExist(name, value, resType, resName, stackName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outgoingEnvVars = append(c.outgoingEnvVars, pApi.ComputeEnvVariable{
		Name: name, Value: value, ResourceType: resType, ResourceName: resName, StackName: stackName, Secret: false,
	})
}

func (c *capturingCollector) AddSecretEnvVariableIfNotExist(name, value, resType, resName, stackName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outgoingEnvVars = append(c.outgoingEnvVars, pApi.ComputeEnvVariable{
		Name: name, Value: value, ResourceType: resType, ResourceName: resName, StackName: stackName, Secret: true,
	})
}

func (c *capturingCollector) AddResourceTplExtension(resName string, values map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resourceTpl = append(c.resourceTpl, resourceTplEntry{resName: resName, values: values})
}

func (c *capturingCollector) AddDependencyTplExtension(depName, resName string, values map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dependencyTpl = append(c.dependencyTpl, dependencyTplEntry{depName: depName, resName: resName, values: values})
}

func (c *capturingCollector) AddDependency(sdk.Resource)                       {}
func (c *capturingCollector) Dependencies() []sdk.Resource                     { return nil }
func (c *capturingCollector) AddOutput(_ *sdk.Context, _ sdk.Output)           {}
func (c *capturingCollector) Outputs() []sdk.Output                            { return nil }
func (c *capturingCollector) ResolvePlaceholders(any) error                    { return nil }
func (c *capturingCollector) AddPreProcessor(any, pApi.PreProcessor)           {}
func (c *capturingCollector) AddPostProcessor(any, pApi.PostProcessor)         {}
func (c *capturingCollector) GetPreProcessors(any) ([]pApi.PreProcessor, bool) { return nil, false }

func (c *capturingCollector) GetPostProcessors(any) ([]pApi.PostProcessor, bool) { return nil, false }
func (c *capturingCollector) RunPreProcessors(any, any) error                    { return nil }
func (c *capturingCollector) RunPostProcessors(any, any) error                   { return nil }
func (c *capturingCollector) ResourceOutputs() pApi.ResourcesOutputs             { return nil }

func (c *capturingCollector) ToResponse() *scpluginv1.ComputeProcessResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp := &scpluginv1.ComputeProcessResponse{}
	for _, v := range c.outgoingEnvVars {
		resp.EnvVars = append(resp.EnvVars, &scpluginv1.ComputeEnvVariable{
			Name:         v.Name,
			Value:        v.Value,
			Secret:       v.Secret,
			ResourceName: v.ResourceName,
			ResourceType: v.ResourceType,
			StackName:    v.StackName,
		})
	}
	for _, t := range c.resourceTpl {
		resp.TplExtensions = append(resp.TplExtensions, &scpluginv1.TemplateExtension{
			Kind:    scpluginv1.TemplateExtensionKind_TEMPLATE_EXTENSION_KIND_RESOURCE,
			ResName: t.resName,
			Values:  t.values,
		})
	}
	for _, t := range c.dependencyTpl {
		resp.TplExtensions = append(resp.TplExtensions, &scpluginv1.TemplateExtension{
			Kind:    scpluginv1.TemplateExtensionKind_TEMPLATE_EXTENSION_KIND_DEPENDENCY,
			DepName: t.depName,
			ResName: t.resName,
			Values:  t.values,
		})
	}
	return resp
}
