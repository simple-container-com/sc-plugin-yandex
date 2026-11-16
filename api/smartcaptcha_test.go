// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"testing"

	. "github.com/onsi/gomega"
	"github.com/simple-container-com/api/pkg/api"
)

// TestReadSmartCaptchaConfig pins the YAML tags the provisioner reads. A tag
// drifting from `allowedSites` / `captchaId` would decode to zero and an
// adopt would silently become a create.
func TestReadSmartCaptchaConfig(t *testing.T) {
	RegisterTestingT(t)

	in := &api.Config{Config: map[string]any{
		"cloudId":            "test-cloud",
		"folderId":           "test-folder",
		"name":               "forge-landing-ru",
		"allowedSites":       []any{"simple-forge.ru", "www.simple-forge.ru"},
		"complexity":         "MEDIUM",
		"styleJson":          "{}",
		"deletionProtection": true,
		"adopt":              true,
		"captchaId":          "bpn-test",
	}}

	out, err := ReadSmartCaptchaConfig(in)
	Expect(err).To(BeNil())
	cfg, ok := out.Config.(*SmartCaptcha)
	Expect(ok).To(BeTrue(), "decoded config is %T, want *SmartCaptcha", out.Config)
	Expect(cfg.FolderID).To(Equal("test-folder"))
	Expect(cfg.Name).To(Equal("forge-landing-ru"))
	Expect(cfg.AllowedSites).To(Equal([]string{"simple-forge.ru", "www.simple-forge.ru"}))
	Expect(cfg.Complexity).To(Equal("MEDIUM"))
	Expect(cfg.StyleJSON).To(Equal("{}"))
	Expect(cfg.DeletionProtection).NotTo(BeNil())
	Expect(*cfg.DeletionProtection).To(BeTrue())
	Expect(cfg.Adopt).To(BeTrue())
	Expect(cfg.CaptchaID).To(Equal("bpn-test"))
	Expect(cfg.Validate()).To(Succeed())
}

func TestSmartCaptcha_EffectiveDeletionProtection(t *testing.T) {
	RegisterTestingT(t)

	// Created: defaults on.
	Expect(*(&SmartCaptcha{}).EffectiveDeletionProtection()).To(BeTrue())
	// Adopted with nothing named: send nothing, keep the live value.
	Expect((&SmartCaptcha{Adopt: true}).EffectiveDeletionProtection()).To(BeNil())
	off := false
	Expect(*(&SmartCaptcha{DeletionProtection: &off}).EffectiveDeletionProtection()).To(BeFalse())
	Expect(*(&SmartCaptcha{Adopt: true, DeletionProtection: &off}).EffectiveDeletionProtection()).To(BeFalse())
}

func TestSmartCaptcha_Validate(t *testing.T) {
	RegisterTestingT(t)

	Expect((&SmartCaptcha{}).Validate()).To(Succeed())
	Expect((&SmartCaptcha{Adopt: true}).Validate()).To(MatchError(ContainSubstring("captchaId is required")))
	Expect((&SmartCaptcha{CaptchaID: "x"}).Validate()).To(MatchError(ContainSubstring("adopt=true")))
	Expect((&SmartCaptcha{Complexity: "medium"}).Validate()).To(MatchError(ContainSubstring("complexity")))
	Expect((&SmartCaptcha{Complexity: "FORCE_HARD"}).Validate()).To(Succeed())
}
