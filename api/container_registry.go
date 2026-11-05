// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"github.com/simple-container-com/api/pkg/api"
)

// ResourceTypeYandexContainerRegistry is the YC Container Registry — the
// private OCI registry under `cr.yandex` that every YC-hosted compute
// workload pulls its image from.
//
// Spelled `yc-container-registry` for symmetry with aws' `ecr-repository`
// (the same role on AWS) and `gcp-artifact-registry` on GCP. Not `yc-ecr`
// or `yc-registry` — cmd/schema-gen's guessProviderFromResourceType matches
// `ecr` substrings as AWS and would file the generated schema under
// docs/schemas/aws, and a bare `yc-registry` reads ambiguously once SC
// grows Yandex-DataLens / Yandex-DataTransfer surfaces.
//
// This is A1 of [[sc-yc-managed-k8s]] (roadmap item), landing the first
// standalone YC resource that is NOT already embedded inside another
// template's driver. The existing `yc-serverless-container` template's
// `buildAndPushDockerImage` helper creates a per-stack registry inline,
// but that registry's lifetime is tied to its consumer. The standalone
// resource type below is what a managed-k8s stack (A2) will pull images
// from in the next slice, and what any operator who wants a shared
// registry across multiple stacks declares today.
const ResourceTypeYandexContainerRegistry = "yc-container-registry"

// ContainerRegistry is a standalone Yandex Container Registry plus the
// service account + folder-level IAM a consumer uses to pull images.
//
// Shape notes:
//   - YC Container Registry is a *folder-scoped* resource with a globally
//     unique id. The id is the first path segment under `cr.yandex/<id>/<repo>`.
//   - There is no per-registry IAM binding on YC — permissions are granted
//     at folder scope via `container-registry.images.pusher` /
//     `container-registry.images.puller`. The convention here matches
//     `yc-bucket`: a dedicated service account + a static key pair the
//     consumer stack signs docker-push/pull with.
type ContainerRegistry struct {
	AccountConfig `json:",inline" yaml:",inline"`

	// Name overrides the descriptor name as the actual registry name. YC
	// registry names are scoped to the folder, not globally unique.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`

	// Role is the folder-level IAM role granted to the registry's service
	// account. `container-registry.images.pusher` is write-only to every
	// registry in the folder; narrow to `container-registry.images.puller`
	// for a read-only consumer (e.g. a managed-k8s node group that pulls
	// but does not push).
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
}

// DefaultContainerRegistryRole is the folder-level role a registry's own
// service account gets when the config does not name one. Pusher rather
// than puller: a bare `yc-container-registry` block's natural consumer is
// a build pipeline that pushes to it, same default as ECR's.
const DefaultContainerRegistryRole = "container-registry.images.pusher"

// EffectiveRole resolves Role, falling back to DefaultContainerRegistryRole.
func (r *ContainerRegistry) EffectiveRole() string {
	if r.Role == "" {
		return DefaultContainerRegistryRole
	}
	return r.Role
}

func ReadContainerRegistryConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &ContainerRegistry{})
}
