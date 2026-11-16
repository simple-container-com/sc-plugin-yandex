// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"github.com/pkg/errors"
	"github.com/simple-container-com/api/pkg/api"
)

// ResourceTypeSmartCaptcha is the type string a server.yaml names to provision
// (or adopt) a Yandex SmartCaptcha instance.
const ResourceTypeSmartCaptcha = "yc-smartcaptcha"

// SmartCaptchaComplexities are the values YC accepts for Complexity.
var SmartCaptchaComplexities = []string{"EASY", "MEDIUM", "HARD", "FORCE_HARD"}

// SmartCaptcha is a Yandex SmartCaptcha instance. The client key it exports is
// public (it ships in the page); the server key is exported as a Pulumi secret.
//
// Adopt + CaptchaID import an instance created outside SC (console, yc CLI)
// into state without recreating it. An adopted captcha is protected and
// retained on delete, so neither a config mistake nor `sc destroy` can remove
// the live captcha — the worst case is that SC forgets about it.
type SmartCaptcha struct {
	AccountConfig `json:",inline" yaml:",inline"`

	// Name is the captcha's YC name (folder-unique, 3-63 chars). When adopting
	// it must equal the live name, or the import is refused at preview.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// AllowedSites are the host names the captcha may be rendered on.
	AllowedSites []string `json:"allowedSites,omitempty" yaml:"allowedSites,omitempty"`
	// Complexity is one of SmartCaptchaComplexities. Empty means YC's default
	// (MEDIUM) on create, and "leave as is" on adopt.
	Complexity string `json:"complexity,omitempty" yaml:"complexity,omitempty"`
	// StyleJSON is the appearance JSON the YC console generates. Empty leaves
	// the current style alone.
	StyleJSON string `json:"styleJson,omitempty" yaml:"styleJson,omitempty"`
	// DeletionProtection maps to YC's own deletion_protection flag. Defaults to
	// TRUE for a captcha SC creates; for an adopted one, nil leaves the live
	// value untouched (sending a value that differs from live fails the import).
	DeletionProtection *bool `json:"deletionProtection,omitempty" yaml:"deletionProtection,omitempty"`
	// Adopt imports the existing captcha named by CaptchaID instead of
	// creating one.
	Adopt bool `json:"adopt,omitempty" yaml:"adopt,omitempty"`
	// CaptchaID is the YC id of the captcha to adopt (e.g. bpn516733mm8rngo3rjp).
	CaptchaID string `json:"captchaId,omitempty" yaml:"captchaId,omitempty"`
}

// EffectiveDeletionProtection is the value to send to YC, or nil to send none.
// A created captcha defaults to protected; an adopted one only gets a value
// when the operator named one.
func (c *SmartCaptcha) EffectiveDeletionProtection() *bool {
	if c.DeletionProtection != nil {
		v := *c.DeletionProtection
		return &v
	}
	if c.Adopt {
		return nil
	}
	v := true
	return &v
}

// Validate checks the config shape before anything is registered with Pulumi.
func (c *SmartCaptcha) Validate() error {
	if c.Adopt && c.CaptchaID == "" {
		return errors.New("captchaId is required when adopt=true")
	}
	if !c.Adopt && c.CaptchaID != "" {
		return errors.New("captchaId is only meaningful with adopt=true; set adopt: true to import it")
	}
	if c.Complexity != "" {
		ok := false
		for _, v := range SmartCaptchaComplexities {
			if v == c.Complexity {
				ok = true
				break
			}
		}
		if !ok {
			return errors.Errorf("complexity %q is not one of %v", c.Complexity, SmartCaptchaComplexities)
		}
	}
	return nil
}

func ReadSmartCaptchaConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &SmartCaptcha{})
}
