// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"fmt"
	"net"
	"regexp"

	"github.com/pkg/errors"
	"github.com/samber/lo"

	"github.com/simple-container-com/api/pkg/api"
)

// ResourceTypePostgres is a Yandex Managed Service for PostgreSQL cluster with one
// database and one user, exposed to `uses:` consumers as PG* env vars.
const ResourceTypePostgres = "yc-postgres"

const (
	DefaultPostgresVersion  = "16"
	DefaultPostgresPreset   = "s2.micro"
	DefaultPostgresDiskSize = 10
	DefaultPostgresDiskType = "network-ssd"
	// PostgresPort is the Managed PostgreSQL connection-pooler (Odyssey) port, the
	// one YC documents for clients. 5432 bypasses the pooler and is not opened by
	// the security group this resource creates.
	PostgresPort = 6432
	// PostgresCACertURL is YC's root CA. sslmode=verify-full needs it on the
	// client; sslmode=require (what consumers get by default) does not.
	PostgresCACertURL = "https://storage.yandexcloud.net/cloud-certs/CA.pem"
	// DefaultPostgresSubnetCidr is the range of the subnet created when the config
	// names no subnet.
	DefaultPostgresSubnetCidr = "10.130.0.0/24"
)

// PostgresConfig is the server.yaml config of a `yc-postgres` resource.
//
// Deliberately minimal: one cluster, one database, one user. What is NOT here, and
// why, is in the reference docs — HA beyond listing extra hosts, backup window /
// retention, maintenance window, extensions, per-consumer users, and private-only
// networking (SC has no YC VPC model yet).
type PostgresConfig struct {
	AccountConfig `json:",inline" yaml:",inline"`

	// Name overrides the cluster name (default: the resource name + environment).
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// Version is the PostgreSQL major version: 13..17.
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	// ResourcePreset is the host class, e.g. s2.micro (2 vCPU, 8 GB), b2.medium
	// (burstable, cheapest).
	ResourcePreset string `json:"resourcePreset,omitempty" yaml:"resourcePreset,omitempty"`
	// DiskSize is in GB.
	DiskSize int `json:"diskSize,omitempty" yaml:"diskSize,omitempty"`
	// DiskType is network-ssd, network-hdd, network-ssd-nonreplicated or local-ssd.
	DiskType string `json:"diskType,omitempty" yaml:"diskType,omitempty"`
	// Environment is PRODUCTION or PRESTABLE (YC's own flag; PRESTABLE gets new
	// minor versions first).
	Environment string `json:"environment,omitempty" yaml:"environment,omitempty"`

	// Zones lists one host per entry. One zone (the default, ru-central1-a) is a
	// single host with no failover; more add replicas YC fails over to.
	Zones []string `json:"zones,omitempty" yaml:"zones,omitempty"`
	// NetworkID / SubnetIDs attach the cluster to an existing VPC; SubnetIDs is
	// keyed by zone. With NetworkID empty a dedicated network and one subnet per
	// zone are created.
	NetworkID string            `json:"networkId,omitempty" yaml:"networkId,omitempty"`
	SubnetIDs map[string]string `json:"subnetIds,omitempty" yaml:"subnetIds,omitempty"`

	Database string `json:"database" yaml:"database"`
	Username string `json:"username" yaml:"username"`
	// Password is the user's password; use `${secret:...}`. 8-128 characters.
	Password string `json:"password" yaml:"password"`

	// PublicAccess gives each host a public IP. Default true: a Serverless
	// Container is not in any VPC SC manages, so a public host is how it reaches
	// the cluster. See AllowedCidrs for what that costs.
	PublicAccess *bool `json:"publicAccess,omitempty" yaml:"publicAccess,omitempty"`
	// AllowedCidrs is who may connect to port 6432. Default 0.0.0.0/0: Serverless
	// Containers egress from shared YC addresses with no stable range, so a
	// narrower list would lock them out. Narrow it when every consumer has a
	// known egress IP.
	AllowedCidrs []string `json:"allowedCidrs,omitempty" yaml:"allowedCidrs,omitempty"`
	// ServerlessAccess turns on the cluster's "access from Serverless" flag.
	ServerlessAccess *bool `json:"serverlessAccess,omitempty" yaml:"serverlessAccess,omitempty"`
	// DeletionProtection refuses `sc destroy` of the cluster.
	DeletionProtection bool `json:"deletionProtection,omitempty" yaml:"deletionProtection,omitempty"`
}

func ReadPostgresConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &PostgresConfig{})
}

func (c *PostgresConfig) EffectiveVersion() string {
	return lo.If(c.Version == "", DefaultPostgresVersion).Else(c.Version)
}

func (c *PostgresConfig) EffectivePreset() string {
	return lo.If(c.ResourcePreset == "", DefaultPostgresPreset).Else(c.ResourcePreset)
}

func (c *PostgresConfig) EffectiveDiskSize() int {
	return lo.If(c.DiskSize <= 0, DefaultPostgresDiskSize).Else(c.DiskSize)
}

func (c *PostgresConfig) EffectiveDiskType() string {
	return lo.If(c.DiskType == "", DefaultPostgresDiskType).Else(c.DiskType)
}

func (c *PostgresConfig) EffectiveEnvironment() string {
	return lo.If(c.Environment == "", "PRODUCTION").Else(c.Environment)
}

func (c *PostgresConfig) EffectiveZones() []string {
	if len(c.Zones) == 0 {
		return []string{c.EffectiveZone()}
	}
	return c.Zones
}

func (c *PostgresConfig) EffectivePublicAccess() bool {
	return c.PublicAccess == nil || *c.PublicAccess
}

func (c *PostgresConfig) EffectiveServerlessAccess() bool {
	return c.ServerlessAccess == nil || *c.ServerlessAccess
}

func (c *PostgresConfig) EffectiveAllowedCidrs() []string {
	if len(c.AllowedCidrs) == 0 {
		return []string{"0.0.0.0/0"}
	}
	return c.AllowedCidrs
}

// pgIdentifierRegexp is YC's rule for database and user names.
var pgIdentifierRegexp = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)

// Validate rejects what YC would refuse minutes into a cluster create.
func (c *PostgresConfig) Validate() error {
	if !lo.Contains([]string{"13", "14", "15", "16", "17"}, c.EffectiveVersion()) {
		return errors.Errorf("yc-postgres: version %q is not one of 13..17", c.EffectiveVersion())
	}
	if !pgIdentifierRegexp.MatchString(c.Database) {
		return errors.Errorf("yc-postgres: database %q must be 1-63 of [a-zA-Z0-9_-]", c.Database)
	}
	if !pgIdentifierRegexp.MatchString(c.Username) {
		return errors.Errorf("yc-postgres: username %q must be 1-63 of [a-zA-Z0-9_-]", c.Username)
	}
	if l := len(c.Password); l < 8 || l > 128 {
		return errors.Errorf("yc-postgres: password must be 8-128 characters (got %d); use ${secret:...}", l)
	}
	if !lo.Contains([]string{"PRODUCTION", "PRESTABLE"}, c.EffectiveEnvironment()) {
		return errors.Errorf("yc-postgres: environment %q is not PRODUCTION or PRESTABLE", c.Environment)
	}
	if len(lo.Uniq(c.EffectiveZones())) != len(c.EffectiveZones()) {
		return errors.Errorf("yc-postgres: zones %v repeat a zone; list one host per zone", c.Zones)
	}
	if c.NetworkID != "" {
		for _, z := range c.EffectiveZones() {
			if c.SubnetIDs[z] == "" {
				return errors.Errorf("yc-postgres: networkId is set, so subnetIds needs a subnet for zone %q", z)
			}
		}
	}
	for _, cidr := range c.EffectiveAllowedCidrs() {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return errors.Errorf("yc-postgres: allowedCidrs entry %q is not a CIDR", cidr)
		}
	}
	return nil
}

// PostgresMasterHost is the special FQDN that always resolves to the current
// master, so consumers survive a failover without a config change.
func PostgresMasterHost(clusterID string) string {
	return fmt.Sprintf("c-%s.rw.mdb.yandexcloud.net", clusterID)
}
