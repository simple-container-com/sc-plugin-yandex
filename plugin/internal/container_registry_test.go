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

const tokenContainerRegistry = "yandex:index/containerRegistry:ContainerRegistry"

func baseContainerRegistryConfig() *yandex.ContainerRegistry {
	return &yandex.ContainerRegistry{
		AccountConfig: yandex.AccountConfig{
			CloudID:  "test-cloud",
			FolderID: "test-folder",
		},
	}
}

func containerRegistryResourceInput(cfg *yandex.ContainerRegistry) api.ResourceInput {
	return api.ResourceInput{
		Descriptor: &api.ResourceDescriptor{
			Type:   yandex.ResourceTypeYandexContainerRegistry,
			Name:   "images",
			Config: api.Config{Config: cfg},
		},
		StackParams: &api.StackParams{Environment: "test"},
	}
}

func provisionContainerRegistry(cfg *yandex.ContainerRegistry, mocks *bucketMocks) error {
	return pulumi.RunErr(func(ctx *pulumi.Context) error {
		params, err := bucketProvisionParams(ctx)
		if err != nil {
			return err
		}
		_, err = ContainerRegistry(ctx, api.Stack{Name: "test-stack"}, containerRegistryResourceInput(cfg), params)
		return err
	}, pulumi.WithMocks("project", "stack", mocks))
}

// A registry is useless without an identity that can push/pull. Pin the
// full resource set so a refactor that quietly drops one gets caught —
// same shape as TestObjectStorageBucket_ProvisionsIdentityAlongsideBucket.
func TestContainerRegistry_ProvisionsIdentityAlongsideRegistry(t *testing.T) {
	RegisterTestingT(t)

	mocks := newBucketMocks()
	Expect(provisionContainerRegistry(baseContainerRegistryConfig(), mocks)).To(BeNil())

	Expect(mocks.countOf(tokenContainerRegistry)).To(Equal(1))
	Expect(mocks.countOf("yandex:index/iamServiceAccount:IamServiceAccount")).To(Equal(1))
	Expect(mocks.countOf("yandex:index/iamServiceAccountStaticAccessKey:IamServiceAccountStaticAccessKey")).To(Equal(1))

	// Additive binding, never authoritative: FolderIamPolicy would wipe
	// every other binding in the folder.
	Expect(mocks.countOf("yandex:index/resourcemanagerFolderIamMember:ResourcemanagerFolderIamMember")).To(Equal(1))
	Expect(mocks.countOf("yandex:index/resourcemanagerFolderIamPolicy:ResourcemanagerFolderIamPolicy")).To(Equal(0))

	registry := mocks.inputsOf(tokenContainerRegistry)
	Expect(registry["name"].StringValue()).To(Equal("images--test"))
	Expect(registry["folderId"].StringValue()).To(Equal("test-folder"))

	role := mocks.inputsOf("yandex:index/resourcemanagerFolderIamMember:ResourcemanagerFolderIamMember")
	Expect(role["role"].StringValue()).To(Equal(yandex.DefaultContainerRegistryRole))
}

// Operator overrides for name + role must survive the descriptor flattening.
// `storage.viewer`-like narrowing (push → pull) is why Role is configurable
// at all; this test pins the override wires through.
func TestContainerRegistry_HonoursExplicitNameAndRole(t *testing.T) {
	RegisterTestingT(t)

	cfg := baseContainerRegistryConfig()
	cfg.Name = "shared-images"
	cfg.Role = "container-registry.images.puller"

	mocks := newBucketMocks()
	Expect(provisionContainerRegistry(cfg, mocks)).To(BeNil())

	registry := mocks.inputsOf(tokenContainerRegistry)
	Expect(registry["name"].StringValue()).To(Equal("shared-images--test"))

	role := mocks.inputsOf("yandex:index/resourcemanagerFolderIamMember:ResourcemanagerFolderIamMember")
	Expect(role["role"].StringValue()).To(Equal("container-registry.images.puller"))
}

// A missing folder must surface as an error BEFORE any resource is
// registered — not as a Pulumi apply failure mid-deploy.
func TestContainerRegistry_RejectsMissingFolderBeforeCreatingAnything(t *testing.T) {
	RegisterTestingT(t)

	cfg := baseContainerRegistryConfig()
	cfg.FolderID = ""

	mocks := newBucketMocks()
	err := provisionContainerRegistry(cfg, mocks)
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("folderId must be set"))
	Expect(mocks.countOf(tokenContainerRegistry)).To(Equal(0))
	Expect(mocks.countOf("yandex:index/iamServiceAccount:IamServiceAccount")).To(Equal(0))
}
