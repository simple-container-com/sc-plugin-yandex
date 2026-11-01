// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/samber/lo"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"

	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

// envVarWildcardCertID is the env var the yc-static-website driver reads
// for the shared wildcard cert id. One per uses: resource, named by the
// resource (so a stack that uses two wildcard-cert resources gets both).
func envVarWildcardCertID(resourceName string) string {
	return fmt.Sprintf("YC_WILDCARD_CERT_%s_ID", strings.ToUpper(strings.ReplaceAll(resourceName, "-", "_")))
}

func envVarWildcardCertZoneName(resourceName string) string {
	return fmt.Sprintf("YC_WILDCARD_CERT_%s_ZONE", strings.ToUpper(strings.ReplaceAll(resourceName, "-", "_")))
}

// WildcardCertComputeProcessor exposes a parent-provisioned yc-wildcard-cert
// resource to a child stack. The child reads the cert id via a resource
// template extension so `${resource:<name>.certificateId}` interpolates in
// client.yaml, and via YC_WILDCARD_CERT_<name>_ID env variables so a child
// driver can shell out with the id directly.
//
// Nothing but ids and the zone name cross the bus. There is no credential
// to leak and nothing is secret: a cert id and the zone it covers are both
// discoverable from any YC console session the operator already has.
func WildcardCertComputeProcessor(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, collector pApi.ComputeContextCollector, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	if params.ParentStack == nil {
		return nil, errors.Errorf("parent stack must not be nil for yc-wildcard-cert compute processor for %q", stack.Name)
	}
	cfg, ok := input.Descriptor.Config.Config.(*yandex.WildcardCertConfig)
	if !ok {
		return nil, errors.Errorf("failed to convert config for yc-wildcard-cert resource %q", input.Descriptor.Name)
	}
	parentStackName := params.ParentStack.StackName
	resName := input.ToResName(input.Descriptor.Name)

	suffix := lo.If(params.ParentStack.DependsOnResource != nil, "--"+lo.FromPtr(params.ParentStack.DependsOnResource).Name).Else("")
	params.Log.Info(ctx.Context(), "getting parent's (%q) outputs for yc-wildcard-cert %q (%q)", params.ParentStack.FullReference, resName, suffix)
	parentRef, err := sdk.NewStackReference(ctx, fmt.Sprintf("%s--%s--%s%s--yc-wildcard-cert-ref", stack.Name, params.ParentStack.StackName, input.Descriptor.Name, suffix), &sdk.StackReferenceArgs{
		Name: sdk.String(params.ParentStack.FullReference).ToStringOutput(),
	})
	if err != nil {
		return nil, err
	}

	readOutput := func(export string) (string, error) {
		val, err := pApi.GetParentOutput(parentRef, export, params.ParentStack.FullReference, false)
		if err != nil {
			return "", errors.Wrapf(err, "failed to read %q from parent stack for yc-wildcard-cert %q", export, resName)
		} else if val == "" {
			return "", errors.Errorf("%q from parent stack is empty — did the parent provision the yc-wildcard-cert resource?", export)
		}
		return val, nil
	}

	certID, err := readOutput(toWildcardCertIDExport(resName))
	if err != nil {
		return nil, err
	}
	zoneName, err := readOutput(toWildcardCertZoneNameExport(resName))
	if err != nil {
		return nil, err
	}

	// Both reads above are already resolved strings (GetParentOutput is
	// synchronous via its own ApplyT-wait). Fan them out synchronously —
	// no outer ApplyT wrapper because collector.AddOutput is a no-op in
	// the capturingCollector and a dead-stripped Output chain would never
	// fire the body (trap 15 class — see
	// forge/docs/design/2026-11-01/plugin-extraction-playbook/ #15).
	collector.AddResourceTplExtension(input.Descriptor.Name, map[string]string{
		"certificateId": certID,
		"zoneName":      zoneName,
	})
	collector.AddEnvVariableIfNotExist(
		envVarWildcardCertID(input.Descriptor.Name), certID,
		input.Descriptor.Type, input.Descriptor.Name, parentStackName,
	)
	collector.AddEnvVariableIfNotExist(
		envVarWildcardCertZoneName(input.Descriptor.Name), zoneName,
		input.Descriptor.Type, input.Descriptor.Name, parentStackName,
	)
	_ = cfg

	return &api.ResourceOutput{Ref: parentStackName}, nil
}
