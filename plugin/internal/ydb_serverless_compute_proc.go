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
	"github.com/simple-container-com/api/pkg/util"
	sdkYandex "github.com/simple-container-com/pulumi-yandex/sdk/go/yandex"

	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

// ContainerServiceAccountCtx is what serverless_container.go hands to the
// Collector's RunPostProcessors after it has created the container's SA. The
// wrapper is deliberate: a PostProcessor closure receives `onObject any` only,
// and creating a Pulumi resource needs a live `*sdk.Context` that cannot cross
// an RPC boundary. The YDB ComputeProcessor parks its closure with THIS struct
// as `forType`; serverless_container.go passes an instance as `onObject` so the
// closure captures the container's identity and the live ctx together.
type ContainerServiceAccountCtx struct {
	Ctx           *sdk.Context
	SA            *sdkYandex.IamServiceAccount
	ContainerName string
	FolderID      string
	// Opts carries the provider handle the container provisioner is already
	// using; the YDB binding must share it so the IAM resource lands in the
	// same folder as the database.
	Opts []sdk.ResourceOption
}

// YdbServerlessComputeProcessor injects a parent-provisioned YDB into a
// consuming stack's environment AND parks a post-processor that creates the
// DB-scoped IAM binding once the container's SA exists.
//
// The env var shape matches what go-ydb and ydb-nodejs-sdk read by default —
// YDB_ENDPOINT, YDB_DATABASE, YDB_METADATA_CREDENTIALS — mirroring the
// bucket's S3_* convention (same reason: so unmodified application code works
// against the provisioned resource).
//
// ⚠️ yandex.YdbDatabaseIamBinding is AUTHORITATIVE for the (database, role)
// pair — see §Pulumi plumbing in docs/design/2026-11-15/sc-plugin-yandex-ydb.
// SC owns ydb.editor on every database it creates; grants added out of band
// (console, yc CLI, direct API) are wiped on the next deploy until the
// pulumi-yandex SDK ships an additive YdbDatabaseIamMember.
func YdbServerlessComputeProcessor(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, collector pApi.ComputeContextCollector, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	if params.ParentStack == nil {
		return nil, errors.Errorf("parent stack must not be nil for compute processor for %q", stack.Name)
	}
	parentStackName := params.ParentStack.StackName

	cfg, ok := input.Descriptor.Config.Config.(*yandex.YdbServerless)
	if !ok {
		return nil, errors.Errorf("failed to convert ydb-serverless config for %q", input.Descriptor.Type)
	}

	resName := input.ToResName(lo.If(cfg.Name == "", input.Descriptor.Name).Else(cfg.Name))

	suffix := lo.If(params.ParentStack.DependsOnResource != nil, "--"+lo.FromPtr(params.ParentStack.DependsOnResource).Name).Else("")
	params.Log.Info(ctx.Context(), "getting parent's (%q) outputs for yandex ydb %q (%q)", params.ParentStack.FullReference, resName, suffix)
	parentRef, err := sdk.NewStackReference(ctx, fmt.Sprintf("%s--%s--%s%s--yc-ydb-ref", stack.Name, params.ParentStack.StackName, input.Descriptor.Name, suffix), &sdk.StackReferenceArgs{
		Name: sdk.String(params.ParentStack.FullReference).ToStringOutput(),
	})
	if err != nil {
		return nil, err
	}

	readOutput := func(export string, secret bool) (string, error) {
		val, err := pApi.GetParentOutput(parentRef, export, params.ParentStack.FullReference, secret)
		if err != nil {
			return "", errors.Wrapf(err, "failed to get %q from parent stack for %q", export, stack.Name)
		} else if val == "" {
			return "", errors.Errorf("%q is empty for %q", export, stack.Name)
		}
		return val, nil
	}

	resEndpoint, err := readOutput(toYdbFullEndpointExport(resName), false)
	if err != nil {
		return nil, err
	}
	resDatabase, err := readOutput(toYdbDatabasePathExport(resName), false)
	if err != nil {
		return nil, err
	}
	resDatabaseID, err := readOutput(toYdbDatabaseIdExport(resName), false)
	if err != nil {
		return nil, err
	}

	// The IAM binding runs in the container's pctx once its SA exists. Park a
	// closure keyed on ContainerServiceAccountCtx so serverless_container.go can
	// invoke it via RunPostProcessors — the ONLY mechanism in the collector
	// interface that crosses the RPC boundary and carries a live ctx.
	//
	// Capture only data (strings), not the compute-process ctx: that ctx closes
	// before the closure fires. ydb.editor is hard-coded here because the
	// one role on one database is the whole point — adding a knob for
	// ydb.viewer is additive and a follow-up.
	bindingSuffix := input.Descriptor.Name
	params.Log.Warn(ctx.Context(), "yc-ydb-serverless %q: SC will own ydb.editor on this database; out-of-band console/CLI grants will be wiped on next deploy (authoritative binding — see docs/design/2026-11-15/sc-plugin-yandex-ydb)", resName)
	collector.AddPostProcessor(&ContainerServiceAccountCtx{}, func(onObject any) error {
		saCtx, ok := onObject.(*ContainerServiceAccountCtx)
		if !ok {
			return errors.Errorf("ydb compute-processor post-processor expected *ContainerServiceAccountCtx, got %T", onObject)
		}
		member := saCtx.SA.ID().ToStringOutput().ApplyT(func(id string) string {
			return fmt.Sprintf("serviceAccount:%s", id)
		}).(sdk.StringOutput)
		bindingName := fmt.Sprintf("%s-ydb-editor-%s", saCtx.ContainerName, bindingSuffix)
		_, bindErr := sdkYandex.NewYdbDatabaseIamBinding(saCtx.Ctx, bindingName, &sdkYandex.YdbDatabaseIamBindingArgs{
			DatabaseId: sdk.String(resDatabaseID).ToStringOutput(),
			Role:       sdk.String(yandex.DefaultYdbRole),
			Members:    sdk.StringArray{member},
		}, append(saCtx.Opts, sdk.DependsOn([]sdk.Resource{saCtx.SA}))...)
		if bindErr != nil {
			return errors.Wrapf(bindErr, "failed to grant %q on ydb %q to container %q", yandex.DefaultYdbRole, resName, saCtx.ContainerName)
		}
		return nil
	})

	collector.AddOutput(ctx, parentRef.Name.ApplyT(func(refName any) any {
		// Plain env — YDB_METADATA_CREDENTIALS is a go-ydb / ydb-nodejs-sdk flag
		// that selects metadata-service auth (the container's own SA). Not a
		// credential value; safe as plain env.
		collector.AddEnvVariableIfNotExist(util.ToEnvVariableName("YDB_METADATA_CREDENTIALS"), "1",
			input.Descriptor.Type, input.Descriptor.Name, parentStackName)
		// Secret env — the endpoint and database path carry the cluster id and
		// ARE usable by anything with metadata-service auth in the folder, so
		// they stay on the Lockbox path (not the plain env listing).
		for name, value := range map[string]string{
			fmt.Sprintf("YDB_%s_ENDPOINT", resName): resEndpoint,
			fmt.Sprintf("YDB_%s_DATABASE", resName): resDatabase,
			"YDB_ENDPOINT":                          resEndpoint,
			"YDB_DATABASE":                          resDatabase,
		} {
			collector.AddSecretEnvVariableIfNotExist(util.ToEnvVariableName(name), value,
				input.Descriptor.Type, input.Descriptor.Name, parentStackName)
		}

		collector.AddResourceTplExtension(input.Descriptor.Name, map[string]string{
			"endpoint":    resEndpoint,
			"database":    resDatabase,
			"database-id": resDatabaseID,
		})

		return nil
	}))

	return &api.ResourceOutput{
		Ref: parentStackName,
	}, nil
}
