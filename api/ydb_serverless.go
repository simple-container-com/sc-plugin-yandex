// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"github.com/simple-container-com/api/pkg/api"
)

// ResourceTypeYdbServerless is the type string a server.yaml names to provision
// a serverless Yandex Database. Dedicated mode is out of scope for v1 — see
// forge/docs/design/2026-11-15/sc-plugin-yandex-ydb/README.md §Non-goals.
const ResourceTypeYdbServerless = "yc-ydb-serverless"

// DefaultYdbRole is the IAM role the container's auto-created service account
// gets on this database when a client stack lists it under `uses:`. ydb.editor
// is read+write on the one database (not folder-wide); narrow to ydb.viewer by
// grepping the ComputeProcessor if you ever need a read-only consumer.
const DefaultYdbRole = "ydb.editor"

// YdbServerless is a Yandex Database (serverless mode) plus the metadata a
// consuming stack needs to reach it. The grant from consumer SA to database
// lives in the ComputeProcessor, not here: the SA is created at client-stack
// deploy time and the binding is scoped to that one container.
type YdbServerless struct {
	AccountConfig `json:",inline" yaml:",inline"`

	// Name overrides the descriptor name as the actual YDB database name. YC
	// database names are folder-unique.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// LocationID picks the YDB location (e.g. ru-central1). Defaults to YC's
	// own default when empty — YC's SDK resolves it to the cloud's primary
	// region, so leaving it empty is the right default for a single-region
	// stack.
	LocationID string `json:"locationId,omitempty" yaml:"locationId,omitempty"`
	// DeletionProtection flips Pulumi's deletion_protection flag on the
	// database. Defaults to TRUE at this layer — `sc destroy` should not nuke a
	// prod DB silently. Set it to false explicitly to allow teardown.
	DeletionProtection *bool `json:"deletionProtection,omitempty" yaml:"deletionProtection,omitempty"`
	// SleepAfter is the YDB serverless auto-sleep delay, in seconds. Zero means
	// YC's own default.
	SleepAfter int `json:"sleepAfter,omitempty" yaml:"sleepAfter,omitempty"`
	// Labels are key/value label pairs assigned to the resource.
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	// ServerlessDatabase carries the provisioned-RCU knobs for the serverless
	// database. nil means pay-per-request (the common case); set it to cap
	// throughput or storage.
	ServerlessDatabase *YdbServerlessLimits `json:"serverlessDatabase,omitempty" yaml:"serverlessDatabase,omitempty"`
}

// YdbServerlessLimits are the knobs under
// yandex.YdbDatabaseServerlessServerlessDatabase. All optional; YC fills the
// unset ones from the service defaults.
type YdbServerlessLimits struct {
	EnableThrottlingRcuLimit *bool `json:"enableThrottlingRcuLimit,omitempty" yaml:"enableThrottlingRcuLimit,omitempty"`
	ProvisionedRcuLimit      *int  `json:"provisionedRcuLimit,omitempty" yaml:"provisionedRcuLimit,omitempty"`
	StorageSizeLimit         *int  `json:"storageSizeLimit,omitempty" yaml:"storageSizeLimit,omitempty"`
	ThrottlingRcuLimit       *int  `json:"throttlingRcuLimit,omitempty" yaml:"throttlingRcuLimit,omitempty"`
}

// EffectiveDeletionProtection is the value sent to Pulumi. Returns true when
// DeletionProtection is nil — the safer default; a bare `yc-ydb-serverless`
// stanza in server.yaml must not be destructible by `sc destroy` without the
// operator opting in.
func (y *YdbServerless) EffectiveDeletionProtection() bool {
	if y.DeletionProtection == nil {
		return true
	}
	return *y.DeletionProtection
}

// HasLimits reports whether the optional ServerlessDatabase block carries any
// field — used by the provisioner to decide whether to emit the block at all,
// since an empty block pins zero values that YC would otherwise fill in.
func (l *YdbServerlessLimits) HasLimits() bool {
	if l == nil {
		return false
	}
	return l.EnableThrottlingRcuLimit != nil || l.ProvisionedRcuLimit != nil ||
		l.StorageSizeLimit != nil || l.ThrottlingRcuLimit != nil
}

func ReadYdbServerlessConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &YdbServerless{})
}
