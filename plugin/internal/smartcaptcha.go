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

// SmartCaptchaOutput is the Ref a yc-smartcaptcha provision returns.
type SmartCaptchaOutput struct {
	Captcha *sdkYandex.SmartcaptchaCaptcha
}

// smartCaptchaUnmanagedFields are the captcha attributes SC never sets. On an
// adopted captcha they are ignored, so whatever was configured in the console
// (security rules, variants, the challenge type) neither fails the import nor
// gets reset by a later apply.
var smartCaptchaUnmanagedFields = []string{
	"challengeType", "preCheckType", "securityRules", "overrideVariants",
	"description", "labels", "turnOffHostnameCheck", "disallowDataProcessing",
	"timeouts",
}

// SmartCaptcha provisions a Yandex SmartCaptcha, or — with adopt: true —
// imports an existing one by captchaId. An adopted captcha is registered with
// Protect + RetainOnDelete: a config mistake cannot delete it, and removing it
// from server.yaml only drops it from Pulumi state.
func SmartCaptcha(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	cfg, ok := input.Descriptor.Config.Config.(*yandex.SmartCaptcha)
	if !ok {
		return nil, errors.Errorf("failed to cast config to yandex.SmartCaptcha for %q", input.Descriptor.Type)
	}
	if params.Provider == nil {
		return nil, errors.Errorf("provider must not be nil for smartcaptcha %q in stack %q", input.Descriptor.Name, stack.Name)
	}
	if err := api.ConvertAuth(cfg, &cfg.AccountConfig); err != nil {
		return nil, errors.Wrapf(err, "failed to convert auth config to yandex.AccountConfig for smartcaptcha %q", input.Descriptor.Name)
	}
	if cfg.FolderID == "" {
		return nil, errors.Errorf("folderId must be set for smartcaptcha %q in stack %q", input.Descriptor.Name, stack.Name)
	}
	if err := cfg.Validate(); err != nil {
		return nil, errors.Wrapf(err, "invalid smartcaptcha %q", input.Descriptor.Name)
	}

	resName := input.ToResName(lo.If(cfg.Name == "", input.Descriptor.Name).Else(cfg.Name))
	args := &sdkYandex.SmartcaptchaCaptchaArgs{
		FolderId: sdk.String(cfg.FolderID),
	}
	opts := []sdk.ResourceOption{sdk.Provider(params.Provider)}

	if cfg.Adopt {
		pApi.LogAdoptionWarnings(ctx, input, params, "SmartCaptcha", cfg.CaptchaID)
		// Only send what the operator named: any value that differs from the
		// live captcha makes Pulumi refuse the import at preview, so an unset
		// field must stay unset AND be ignored.
		ignore := append([]string{}, smartCaptchaUnmanagedFields...)
		if cfg.Name != "" {
			args.Name = sdk.String(cfg.Name)
		} else {
			ignore = append(ignore, "name")
		}
		if len(cfg.AllowedSites) == 0 {
			ignore = append(ignore, "allowedSites")
		}
		if cfg.Complexity == "" {
			ignore = append(ignore, "complexity")
		}
		if cfg.StyleJSON == "" {
			ignore = append(ignore, "styleJson")
		}
		if cfg.EffectiveDeletionProtection() == nil {
			ignore = append(ignore, "deletionProtection")
		}
		opts = append(opts, sdk.Import(sdk.ID(cfg.CaptchaID)), sdk.RetainOnDelete(true))
		opts = append(opts, pApi.AdoptionProtectionOptions(ignore)...)
		params.Log.Info(ctx.Context(), "adopting existing smartcaptcha %q as %q for stack %q", cfg.CaptchaID, resName, stack.Name)
	} else {
		args.Name = sdk.String(lo.If(cfg.Name == "", resName).Else(cfg.Name))
		params.Log.Info(ctx.Context(), "configure smartcaptcha %q for stack %q", resName, stack.Name)
	}

	if len(cfg.AllowedSites) > 0 {
		args.AllowedSites = sdk.ToStringArray(cfg.AllowedSites)
	}
	if cfg.Complexity != "" {
		args.Complexity = sdk.String(cfg.Complexity)
	}
	if cfg.StyleJSON != "" {
		args.StyleJson = sdk.String(cfg.StyleJSON)
	}
	if dp := cfg.EffectiveDeletionProtection(); dp != nil {
		args.DeletionProtection = sdk.Bool(*dp)
	}

	captcha, err := sdkYandex.NewSmartcaptchaCaptcha(ctx, resName, args, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create smartcaptcha %q", resName)
	}

	// The server key is not an attribute of the captcha resource; it comes
	// from the keys data source, keyed by the captcha id (unknown until the
	// captcha exists, hence the output-form invoke).
	keys := sdkYandex.GetSmartcaptchaCaptchaKeysOutput(ctx, sdkYandex.GetSmartcaptchaCaptchaKeysOutputArgs{
		CaptchaId: captcha.ID().ToStringOutput(),
	}, sdk.Provider(params.Provider))

	ctx.Export(toSmartCaptchaIdExport(resName), captcha.ID())
	ctx.Export(toSmartCaptchaClientKeyExport(resName), captcha.ClientKey)
	ctx.Export(toSmartCaptchaServerKeyExport(resName), sdk.ToSecret(keys.ServerKey()))

	return &api.ResourceOutput{Ref: &SmartCaptchaOutput{Captcha: captcha}}, nil
}

func toSmartCaptchaIdExport(resName string) string {
	return fmt.Sprintf("%s-smartcaptcha-id", resName)
}

func toSmartCaptchaClientKeyExport(resName string) string {
	return fmt.Sprintf("%s-smartcaptcha-client-key", resName)
}

func toSmartCaptchaServerKeyExport(resName string) string {
	return fmt.Sprintf("%s-smartcaptcha-server-key", resName)
}
