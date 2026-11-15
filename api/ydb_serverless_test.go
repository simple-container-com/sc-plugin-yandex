// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"testing"

	. "github.com/onsi/gomega"
	"github.com/simple-container-com/api/pkg/api"
)

// TestReadYdbServerlessConfig pins the happy-path decode: a `yc-ydb-serverless`
// block in server.yaml decodes into every field the provisioner reads. If a tag
// drifts away from the Pulumi SDK's spelling (e.g. `locationId` vs `location`),
// the field silently decodes to zero and the live deploy produces a database in
// the wrong region — the kind of mistake lint cannot catch.
func TestReadYdbServerlessConfig(t *testing.T) {
	RegisterTestingT(t)

	in := &api.Config{Config: map[string]any{
		"cloudId":            "test-cloud",
		"folderId":           "test-folder",
		"name":               "astrofit",
		"locationId":         "ru-central1",
		"deletionProtection": true,
		"sleepAfter":         3600,
		"labels": map[string]any{
			"env": "prod",
		},
		"serverlessDatabase": map[string]any{
			"throttlingRcuLimit": 100,
			"storageSizeLimit":   50,
		},
	}}

	out, err := ReadYdbServerlessConfig(in)
	Expect(err).To(BeNil())
	cfg, ok := out.Config.(*YdbServerless)
	Expect(ok).To(BeTrue(), "decoded config is %T, want *YdbServerless", out.Config)
	Expect(cfg.CloudID).To(Equal("test-cloud"))
	Expect(cfg.FolderID).To(Equal("test-folder"))
	Expect(cfg.Name).To(Equal("astrofit"))
	Expect(cfg.LocationID).To(Equal("ru-central1"))
	Expect(cfg.DeletionProtection).NotTo(BeNil())
	Expect(*cfg.DeletionProtection).To(BeTrue())
	Expect(cfg.SleepAfter).To(Equal(3600))
	Expect(cfg.Labels).To(HaveKeyWithValue("env", "prod"))
	Expect(cfg.ServerlessDatabase).NotTo(BeNil())
	Expect(cfg.ServerlessDatabase.ThrottlingRcuLimit).NotTo(BeNil())
	Expect(*cfg.ServerlessDatabase.ThrottlingRcuLimit).To(Equal(100))
	Expect(*cfg.ServerlessDatabase.StorageSizeLimit).To(Equal(50))
	Expect(cfg.ServerlessDatabase.HasLimits()).To(BeTrue())
}

// TestYdbServerless_DefaultDeletionProtection is load-bearing: a bare config in
// server.yaml must come back from EffectiveDeletionProtection() as true, so
// `sc destroy` cannot nuke a prod database without an explicit opt-out.
func TestYdbServerless_DefaultDeletionProtection(t *testing.T) {
	RegisterTestingT(t)

	Expect((&YdbServerless{}).EffectiveDeletionProtection()).To(BeTrue())

	off := false
	Expect((&YdbServerless{DeletionProtection: &off}).EffectiveDeletionProtection()).To(BeFalse())

	on := true
	Expect((&YdbServerless{DeletionProtection: &on}).EffectiveDeletionProtection()).To(BeTrue())
}

// TestYdbServerlessLimits_HasLimits short-circuits an empty block: the
// provisioner must not send an empty ServerlessDatabase block to Pulumi — doing
// so pins zero values that YC would otherwise fill from service defaults.
func TestYdbServerlessLimits_HasLimits(t *testing.T) {
	RegisterTestingT(t)

	Expect((*YdbServerlessLimits)(nil).HasLimits()).To(BeFalse())
	Expect((&YdbServerlessLimits{}).HasLimits()).To(BeFalse())

	n := 100
	Expect((&YdbServerlessLimits{ProvisionedRcuLimit: &n}).HasLimits()).To(BeTrue())
}
