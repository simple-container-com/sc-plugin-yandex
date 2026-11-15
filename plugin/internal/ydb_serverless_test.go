// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"

	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

func baseYdbServerlessConfig() *yandex.YdbServerless {
	return &yandex.YdbServerless{
		AccountConfig: yandex.AccountConfig{
			CloudID:  "test-cloud",
			FolderID: "test-folder",
		},
	}
}

func ydbResourceInput(cfg *yandex.YdbServerless) api.ResourceInput {
	return api.ResourceInput{
		Descriptor: &api.ResourceDescriptor{
			Type:   yandex.ResourceTypeYdbServerless,
			Name:   "astrofit-db",
			Config: api.Config{Config: cfg},
		},
		StackParams: &api.StackParams{Environment: "test"},
	}
}

// Pins that YdbServerless creates exactly one database resource under the
// configured folder. A reshape that quietly dropped the resource (e.g. hidden
// behind an unused branch) would otherwise ship a green deploy with nothing
// provisioned.
func TestYdbServerless_ProvisionsDatabase(t *testing.T) {
	RegisterTestingT(t)

	mocks := newBucketMocks()
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		params, err := bucketProvisionParams(ctx)
		if err != nil {
			return err
		}
		_, err = YdbServerless(ctx, api.Stack{Name: "test-stack"}, ydbResourceInput(baseYdbServerlessConfig()), params)
		return err
	}, pulumi.WithMocks("project", "stack", mocks))

	Expect(err).To(BeNil())
	Expect(mocks.countOf("yandex:index/ydbDatabaseServerless:YdbDatabaseServerless")).To(Equal(1))

	// The database provisioner mints NO service account, NO authorized key —
	// the whole point of the yc-ydb-serverless shape. A future refactor that
	// snuck one in would break the metadata-service auth story.
	Expect(mocks.countOf("yandex:index/iamServiceAccount:IamServiceAccount")).To(Equal(0))
	Expect(mocks.countOf("yandex:index/iamServiceAccountKey:IamServiceAccountKey")).To(Equal(0))
}

// Pins the db args the provisioner builds from a populated config. Without this
// a `locationId` typo in the Go struct's YAML tag would ship a database in
// whatever region YC picks as the default — silent and confusing.
func TestYdbServerless_HonoursConfiguredFields(t *testing.T) {
	RegisterTestingT(t)

	cfg := baseYdbServerlessConfig()
	cfg.Name = "custom-db"
	cfg.LocationID = "ru-central1"
	cfg.SleepAfter = 3600
	cfg.Labels = map[string]string{"env": "prod"}
	off := false
	cfg.DeletionProtection = &off

	mocks := newBucketMocks()
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		params, err := bucketProvisionParams(ctx)
		if err != nil {
			return err
		}
		_, err = YdbServerless(ctx, api.Stack{Name: "test-stack"}, ydbResourceInput(cfg), params)
		return err
	}, pulumi.WithMocks("project", "stack", mocks))

	Expect(err).To(BeNil())
	db := mocks.inputsOf("yandex:index/ydbDatabaseServerless:YdbDatabaseServerless")
	Expect(db).NotTo(BeNil())
	Expect(db["name"].StringValue()).To(ContainSubstring("custom-db"))
	Expect(db["folderId"].StringValue()).To(Equal("test-folder"))
	Expect(db["locationId"].StringValue()).To(Equal("ru-central1"))
	Expect(db["sleepAfter"].NumberValue()).To(Equal(float64(3600)))
	Expect(db["deletionProtection"].BoolValue()).To(BeFalse())
	Expect(db["labels"].ObjectValue()["env"].StringValue()).To(Equal("prod"))
}

// Pin the deletion-protection default: a config that does not name the field
// must still refuse to delete. The alternative (default false) was live-caught
// as unsafe during design — a stack that silently destroyed its prod DB on a
// cleanup `sc destroy` would be the worst kind of fleet failure.
func TestYdbServerless_DefaultsDeletionProtectionOn(t *testing.T) {
	RegisterTestingT(t)

	mocks := newBucketMocks()
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		params, err := bucketProvisionParams(ctx)
		if err != nil {
			return err
		}
		_, err = YdbServerless(ctx, api.Stack{Name: "test-stack"}, ydbResourceInput(baseYdbServerlessConfig()), params)
		return err
	}, pulumi.WithMocks("project", "stack", mocks))

	Expect(err).To(BeNil())
	db := mocks.inputsOf("yandex:index/ydbDatabaseServerless:YdbDatabaseServerless")
	Expect(db).NotTo(BeNil())
	Expect(db["deletionProtection"].BoolValue()).To(BeTrue())
}

func TestYdbServerless_RejectsMissingFolderBeforeCreatingAnything(t *testing.T) {
	RegisterTestingT(t)

	cfg := baseYdbServerlessConfig()
	cfg.FolderID = ""

	mocks := newBucketMocks()
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		params, err := bucketProvisionParams(ctx)
		if err != nil {
			return err
		}
		_, err = YdbServerless(ctx, api.Stack{Name: "test-stack"}, ydbResourceInput(cfg), params)
		return err
	}, pulumi.WithMocks("project", "stack", mocks))

	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("folderId"))
	Expect(mocks.countOf("yandex:index/ydbDatabaseServerless:YdbDatabaseServerless")).To(Equal(0))
}
