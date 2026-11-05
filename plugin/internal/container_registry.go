// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"fmt"

	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/samber/lo"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	sdkYandex "github.com/simple-container-com/pulumi-yandex/sdk/go/yandex"

	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

// ContainerRegistryOutput is what the parent stack exports for a registry.
type ContainerRegistryOutput struct {
	Registry       *sdkYandex.ContainerRegistry
	ServiceAccount *sdkYandex.IamServiceAccount
	AccessKey      *sdkYandex.IamServiceAccountStaticAccessKey
	RoleBinding    *sdkYandex.ResourcemanagerFolderIamMember
}

// ContainerRegistry provisions a standalone Yandex Container Registry
// together with a dedicated service account and the static key pair a
// consumer signs docker-push/pull with.
//
// Shape mirrors ObjectStorageBucket's: there is no per-registry IAM on
// YC, so authorization is a folder-level role binding (additive, via
// ResourcemanagerFolderIamMember — never ...FolderIamPolicy, which is
// authoritative and would wipe every other binding in the folder).
//
// `cr.yandex/<id>/<repo>` is the pull address: ID() rather than
// RegistryId, for the same reason as `buildAndPushDockerImage` — the
// latter is bridged from a TF field used as both input and output, and
// is not guaranteed populated on a plain create. The resource id of
// yandex_container_registry IS the registry id.
func ContainerRegistry(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	cfg, ok := input.Descriptor.Config.Config.(*yandex.ContainerRegistry)
	if !ok {
		return nil, errors.Errorf("failed to cast config to yandex.ContainerRegistry for %q", input.Descriptor.Type)
	}
	if params.Provider == nil {
		return nil, errors.Errorf("provider must not be nil for container-registry %q in stack %q", input.Descriptor.Name, stack.Name)
	}
	// Rehydrate the account config out of `credentials: "${auth:yc}"`,
	// exactly as Provider / ObjectStorageBucket do. See bucket.go for
	// the 2026-09-26 live-catch: SC resolves `${auth:...}` into the
	// Credentials blob but not into the sibling fields.
	if err := api.ConvertAuth(cfg, &cfg.AccountConfig); err != nil {
		return nil, errors.Wrapf(err, "failed to convert auth config to yandex.AccountConfig for container-registry %q", input.Descriptor.Name)
	}
	if cfg.FolderID == "" {
		return nil, errors.Errorf("folderId must be set for container-registry %q in stack %q", input.Descriptor.Name, stack.Name)
	}

	registryName := input.ToResName(lo.If(cfg.Name == "", input.Descriptor.Name).Else(cfg.Name))
	if err := validateYcResourceName(registryName); err != nil {
		return nil, errors.Wrapf(err, "invalid container-registry name for stack %q", stack.Name)
	}

	opts := []sdk.ResourceOption{sdk.Provider(params.Provider)}

	params.Log.Info(ctx.Context(), "configure yandex container registry %q for stack %q...", registryName, stack.Name)
	registry, err := sdkYandex.NewContainerRegistry(ctx, registryName, &sdkYandex.ContainerRegistryArgs{
		Name:     sdk.String(registryName),
		FolderId: sdk.String(cfg.FolderID),
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create container registry %q for stack %q", registryName, stack.Name)
	}

	// Service account + folder role + static key, same shape as bucket.
	// The role binding goes before the static key: the static key gives
	// access to the registry through that role, and YC's IAM is
	// eventually consistent enough that ordering matters.
	sa, err := sdkYandex.NewIamServiceAccount(ctx, fmt.Sprintf("%s-sa", registryName), &sdkYandex.IamServiceAccountArgs{
		Name:        sdk.String(fmt.Sprintf("%s-sa", registryName)),
		FolderId:    sdk.String(cfg.FolderID),
		Description: sdk.String(fmt.Sprintf("service account for container-registry %s (stack %s)", registryName, stack.Name)),
	}, append(opts, sdk.DependsOn([]sdk.Resource{registry}))...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create service account for container-registry %q", registryName)
	}

	roleBinding, err := sdkYandex.NewResourcemanagerFolderIamMember(ctx, fmt.Sprintf("%s-role", registryName), &sdkYandex.ResourcemanagerFolderIamMemberArgs{
		FolderId: sdk.String(cfg.FolderID),
		Role:     sdk.String(cfg.EffectiveRole()),
		Member:   sa.ID().ToStringOutput().ApplyT(func(id string) string { return fmt.Sprintf("serviceAccount:%s", id) }).(sdk.StringOutput),
	}, append(opts, sdk.DependsOn([]sdk.Resource{sa}))...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to bind role %q for container-registry %q", cfg.EffectiveRole(), registryName)
	}

	accessKey, err := sdkYandex.NewIamServiceAccountStaticAccessKey(ctx, fmt.Sprintf("%s-key", registryName), &sdkYandex.IamServiceAccountStaticAccessKeyArgs{
		ServiceAccountId: sa.ID().ToStringOutput(),
		Description:      sdk.String(fmt.Sprintf("static key for container-registry %s", registryName)),
	}, append(opts, sdk.DependsOn([]sdk.Resource{roleBinding}))...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create static key for container-registry %q", registryName)
	}

	// Export the registry id (= the `cr.yandex/<id>/...` path segment)
	// so a `uses:` consumer can reference it without a second lookup.
	// Name scheme mirrors ObjectStorageBucket's outputs.
	ctx.Export(fmt.Sprintf("%s-container-registry-id", registryName), registry.ID().ToStringOutput())
	ctx.Export(fmt.Sprintf("%s-container-registry-host", registryName),
		registry.ID().ToStringOutput().ApplyT(func(id string) string {
			return fmt.Sprintf("%s/%s", ContainerRegistryHost, id)
		}).(sdk.StringOutput))

	return &api.ResourceOutput{
		Ref: &ContainerRegistryOutput{
			Registry:       registry,
			ServiceAccount: sa,
			AccessKey:      accessKey,
			RoleBinding:    roleBinding,
		},
	}, nil
}
