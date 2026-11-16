// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"

	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

const smartCaptchaType = "yandex:index/smartcaptchaCaptcha:SmartcaptchaCaptcha"

// captchaMocks answers the keys data source the way YC does, so the server-key
// export has something to carry.
type captchaMocks struct {
	*bucketMocks
	keysCalls int
}

func (m *captchaMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	if args.Token == "yandex:index/getSmartcaptchaCaptchaKeys:getSmartcaptchaCaptchaKeys" {
		m.mu.Lock()
		m.keysCalls++
		m.mu.Unlock()
		return resource.PropertyMap{
			"captchaId": args.Args["captchaId"],
			"id":        args.Args["captchaId"],
			"clientKey": resource.NewStringProperty("ysc1_client"),
			"serverKey": resource.NewStringProperty("ysc2_server"),
		}, nil
	}
	return m.bucketMocks.Call(args)
}

func baseSmartCaptchaConfig() *yandex.SmartCaptcha {
	return &yandex.SmartCaptcha{
		AccountConfig: yandex.AccountConfig{CloudID: "test-cloud", FolderID: "test-folder"},
		AllowedSites:  []string{"simple-forge.ru", "www.simple-forge.ru"},
	}
}

func smartCaptchaInput(cfg *yandex.SmartCaptcha) api.ResourceInput {
	return api.ResourceInput{
		Descriptor: &api.ResourceDescriptor{
			Type:   yandex.ResourceTypeSmartCaptcha,
			Name:   "landing-captcha",
			Config: api.Config{Config: cfg},
		},
		StackParams: &api.StackParams{Environment: "test"},
	}
}

func runSmartCaptcha(cfg *yandex.SmartCaptcha) (*captchaMocks, map[string]pulumi.Input, error) {
	mocks := &captchaMocks{bucketMocks: newBucketMocks()}
	var exports map[string]pulumi.Input
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		params, err := bucketProvisionParams(ctx)
		if err != nil {
			return err
		}
		_, err = SmartCaptcha(ctx, api.Stack{Name: "test-stack"}, smartCaptchaInput(cfg), params)
		exports = ctx.GetCurrentExportMap()
		return err
	}, pulumi.WithMocks("project", "stack", mocks))
	return mocks, exports, err
}

// A created captcha: one resource, named, protected at the YC level by
// default, with no import and no Pulumi protect (SC owns its lifecycle).
func TestSmartCaptcha_CreatesCaptcha(t *testing.T) {
	RegisterTestingT(t)

	cfg := baseSmartCaptchaConfig()
	cfg.Complexity = "HARD"
	mocks, exports, err := runSmartCaptcha(cfg)
	Expect(err).To(BeNil())
	Expect(mocks.countOf(smartCaptchaType)).To(Equal(1))

	in := mocks.inputsOf(smartCaptchaType)
	Expect(in["folderId"].StringValue()).To(Equal("test-folder"))
	Expect(in["name"].StringValue()).To(ContainSubstring("landing-captcha"))
	Expect(in["complexity"].StringValue()).To(Equal("HARD"))
	Expect(in["deletionProtection"].BoolValue()).To(BeTrue())
	Expect(in["allowedSites"].ArrayValue()).To(HaveLen(2))

	rpc := mocks.registerRPCOf(smartCaptchaType)
	Expect(rpc.GetImportId()).To(BeEmpty())
	Expect(rpc.GetProtect()).To(BeFalse())

	Expect(mocks.keysCalls).To(Equal(1))
	Expect(exports).To(HaveKey(ContainSubstring("-smartcaptcha-client-key")))
	Expect(exports).To(HaveKey(ContainSubstring("-smartcaptcha-server-key")))
	Expect(exports).To(HaveKey(ContainSubstring("-smartcaptcha-id")))
}

// The adoption safety net: import by id, protect, retain on delete, and
// ignore every field the operator did not name — so the import cannot fail on
// console-managed settings and no later apply can delete the live captcha.
func TestSmartCaptcha_AdoptImportsProtectedAndRetained(t *testing.T) {
	RegisterTestingT(t)

	cfg := baseSmartCaptchaConfig()
	cfg.Name = "forge-landing-ru"
	cfg.Adopt = true
	cfg.CaptchaID = "bpn516733mm8rngo3rjp"
	mocks, _, err := runSmartCaptcha(cfg)
	Expect(err).To(BeNil())
	Expect(mocks.countOf(smartCaptchaType)).To(Equal(1))

	rpc := mocks.registerRPCOf(smartCaptchaType)
	Expect(rpc.GetImportId()).To(Equal("bpn516733mm8rngo3rjp"))
	Expect(rpc.GetProtect()).To(BeTrue())
	Expect(rpc.GetRetainOnDelete()).To(BeTrue())
	Expect(rpc.GetIgnoreChanges()).To(ContainElements("securityRules", "styleJson", "complexity", "deletionProtection"))
	Expect(rpc.GetIgnoreChanges()).NotTo(ContainElement("allowedSites"))
	Expect(rpc.GetIgnoreChanges()).NotTo(ContainElement("name"))

	in := mocks.inputsOf(smartCaptchaType)
	Expect(in["name"].StringValue()).To(Equal("forge-landing-ru"))
	// Unset on adopt: sending a value that differs from live fails the import.
	Expect(in).NotTo(HaveKey(resource.PropertyKey("deletionProtection")))
	Expect(in).NotTo(HaveKey(resource.PropertyKey("complexity")))
}

func TestSmartCaptcha_AdoptWithoutCaptchaIdCreatesNothing(t *testing.T) {
	RegisterTestingT(t)

	cfg := baseSmartCaptchaConfig()
	cfg.Adopt = true
	mocks, _, err := runSmartCaptcha(cfg)
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("captchaId"))
	Expect(mocks.countOf(smartCaptchaType)).To(Equal(0))
}

func TestSmartCaptcha_RejectsMissingFolderBeforeCreatingAnything(t *testing.T) {
	RegisterTestingT(t)

	cfg := baseSmartCaptchaConfig()
	cfg.FolderID = ""
	mocks, _, err := runSmartCaptcha(cfg)
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("folderId"))
	Expect(mocks.countOf(smartCaptchaType)).To(Equal(0))
}
