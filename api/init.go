// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"github.com/simple-container-com/api/pkg/api"
)

// ProviderType is the discriminator every Yandex Cloud config reports from
// ProviderType(), and the directory generated schemas land in (docs/schemas/yandex).
const ProviderType = "yandex"

func init() {
	api.RegisterProviderConfig(api.ConfigRegisterMap{
		AuthTypeYandexServiceAccount:          ReadAuthServiceAccountConfig,
		SecretsTypeYandexLockbox:              ReadSecretsConfig,
		TemplateTypeYandexServerlessContainer: ReadTemplateConfig,
		ResourceTypeObjectStorageBucket:       ReadObjectStorageBucketConfig,
		RegistrarTypeYandexDns:                ReadRegistrarConfig,
	})

	api.RegisterProvisionerFieldConfig(api.ProvisionerFieldConfigRegister{
		StateStorageTypeYandexObjectStorage: ReadStateStorageConfig,
		SecretsProviderTypeYandexKms:        ReadSecretsProviderConfig,
	})

	// A Serverless Container is the fleet's Lambda replacement, so it converts
	// from a single-image client stack. There is deliberately no compose or
	// static-site converter yet: neither has a YC shape decided (see the design
	// doc's open question on API Gateway).
	api.RegisterCloudSingleImageConverter(api.CloudSingleImageConfigRegister{
		TemplateTypeYandexServerlessContainer: ToServerlessContainerConfig,
	})

	// Fool-protection: when SC core has dropped the in-tree yandex
	// subtree, a server.yaml using any `type: yc-*` (or `type: yandex`
	// provider) but lacking a `plugins.yandex` block would silently fail
	// at provision time. This registration lets SC core's
	// ValidatePluginRequirements emit a copy-paste-ready error at
	// server.yaml parse time instead.
	api.RegisterPluginRequirement(api.PluginRequirement{
		PluginName: "yandex",
		OwnedTypes: []string{
			ProviderType,                          // "yandex" — the pulumi provider
			RegistrarTypeYandexDns,                // "yc-dns"
			ResourceTypeObjectStorageBucket,       // "yc-bucket"
			TemplateTypeYandexServerlessContainer, // "yc-serverless-container"
			StateStorageTypeYandexObjectStorage,   // "yc-object-storage" (state backend)
			SecretsProviderTypeYandexKms,          // "yc-kms" (secrets provider)
			SecretsTypeYandexLockbox,              // "yc-lockbox" (runtime secret store)
			AuthTypeYandexServiceAccount,          // "yc-service-account" (auth block)
		},
		DefaultSource: api.PluginSource{
			Type:       "github-release",
			Repository: "simple-container-com/sc-plugin-yandex",
		},
	})
}
