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

// YdbServerlessOutput is what the parent stack exports for a yc-ydb-serverless
// resource. Unlike bucket there is no SA / key pair: YDB authenticates via the
// consumer's metadata-service token, so the only identities created here are
// the database itself. The IAM binding between consumer and database is emitted
// by the ComputeProcessor, scoped to one container's SA.
type YdbServerlessOutput struct {
	Database *sdkYandex.YdbDatabaseServerless
}

// YdbServerless provisions a Yandex Database (serverless mode). No service
// account, no key — see §Pulumi plumbing in
// forge/docs/design/2026-11-15/sc-plugin-yandex-ydb/README.md.
func YdbServerless(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	cfg, ok := input.Descriptor.Config.Config.(*yandex.YdbServerless)
	if !ok {
		return nil, errors.Errorf("failed to cast config to yandex.YdbServerless for %q", input.Descriptor.Type)
	}
	if params.Provider == nil {
		return nil, errors.Errorf("provider must not be nil for ydb-serverless %q in stack %q", input.Descriptor.Name, stack.Name)
	}
	// Same `${auth:yc}` dance bucket needs — the credentials blob SC resolves
	// into this field does not populate the sibling ids on its own.
	if err := api.ConvertAuth(cfg, &cfg.AccountConfig); err != nil {
		return nil, errors.Wrapf(err, "failed to convert auth config to yandex.AccountConfig for ydb-serverless %q", input.Descriptor.Name)
	}
	if cfg.FolderID == "" {
		return nil, errors.Errorf("folderId must be set for ydb-serverless %q in stack %q", input.Descriptor.Name, stack.Name)
	}

	resName := input.ToResName(lo.If(cfg.Name == "", input.Descriptor.Name).Else(cfg.Name))
	opts := []sdk.ResourceOption{sdk.Provider(params.Provider)}

	params.Log.Info(ctx.Context(), "configure yandex database (serverless) %q for stack %q", resName, stack.Name)

	dbArgs := &sdkYandex.YdbDatabaseServerlessArgs{
		Name:               sdk.String(resName),
		FolderId:           sdk.String(cfg.FolderID),
		DeletionProtection: sdk.Bool(cfg.EffectiveDeletionProtection()),
	}
	if cfg.LocationID != "" {
		dbArgs.LocationId = sdk.String(cfg.LocationID)
	}
	if cfg.SleepAfter > 0 {
		dbArgs.SleepAfter = sdk.Int(cfg.SleepAfter)
	}
	if len(cfg.Labels) > 0 {
		dbArgs.Labels = sdk.ToStringMap(cfg.Labels)
	}
	if cfg.ServerlessDatabase.HasLimits() {
		dbArgs.ServerlessDatabases = sdkYandex.YdbDatabaseServerlessServerlessDatabaseArray{
			&sdkYandex.YdbDatabaseServerlessServerlessDatabaseArgs{
				EnableThrottlingRcuLimit: sdk.BoolPtrFromPtr(cfg.ServerlessDatabase.EnableThrottlingRcuLimit),
				ProvisionedRcuLimit:      sdk.IntPtrFromPtr(cfg.ServerlessDatabase.ProvisionedRcuLimit),
				StorageSizeLimit:         sdk.IntPtrFromPtr(cfg.ServerlessDatabase.StorageSizeLimit),
				ThrottlingRcuLimit:       sdk.IntPtrFromPtr(cfg.ServerlessDatabase.ThrottlingRcuLimit),
			},
		}
	}

	db, err := sdkYandex.NewYdbDatabaseServerless(ctx, resName, dbArgs, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create ydb-serverless %q", resName)
	}

	// The earlier state-of-the-world claim (pulumi-yandex missing
	// ydbFullEndpoint / ydbApiEndpoint outputs) was wrong — live state on
	// 2026-11-16 shows them populated. The previous workaround used
	// `db.DatabasePath.ApplyT(...).(sdk.StringOutput)`, and that type
	// assertion silently panics because ApplyT returns pulumi.AnyOutput,
	// not pulumi.StringOutput — the panic runs inside Pulumi's resolver
	// goroutine and gets swallowed, dropping ALL five ctx.Export calls
	// on this provisioner. Stack outputs were missing ever since.
	//
	// Export the SDK fields directly.
	ctx.Export(toYdbFullEndpointExport(resName), db.YdbFullEndpoint)
	ctx.Export(toYdbDatabasePathExport(resName), db.DatabasePath)
	ctx.Export(toYdbDatabaseIdExport(resName), db.ID())
	ctx.Export(toYdbDocumentApiEndpointExport(resName), db.DocumentApiEndpoint)
	ctx.Export(toYdbLocationIdExport(resName), db.LocationId)

	return &api.ResourceOutput{
		Ref: &YdbServerlessOutput{Database: db},
	}, nil
}

func toYdbFullEndpointExport(resName string) string {
	return fmt.Sprintf("%s-ydb-full-endpoint", resName)
}

func toYdbDatabasePathExport(resName string) string {
	return fmt.Sprintf("%s-ydb-database-path", resName)
}

func toYdbDatabaseIdExport(resName string) string {
	return fmt.Sprintf("%s-ydb-database-id", resName)
}

func toYdbDocumentApiEndpointExport(resName string) string {
	return fmt.Sprintf("%s-ydb-document-api-endpoint", resName)
}

func toYdbLocationIdExport(resName string) string {
	return fmt.Sprintf("%s-ydb-location-id", resName)
}
