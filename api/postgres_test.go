// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	"github.com/simple-container-com/api/pkg/api"
)

func validPostgres() *PostgresConfig {
	return &PostgresConfig{
		AccountConfig: AccountConfig{CloudID: "c", FolderID: "f"},
		Database:      "app",
		Username:      "app",
		Password:      "correct-horse-battery",
	}
}

func TestPostgresConfig_Defaults(t *testing.T) {
	RegisterTestingT(t)

	c := validPostgres()
	Expect(c.Validate()).To(Succeed())
	Expect(c.EffectiveVersion()).To(Equal("16"))
	Expect(c.EffectivePreset()).To(Equal("s2.micro"))
	Expect(c.EffectiveDiskSize()).To(Equal(10))
	Expect(c.EffectiveDiskType()).To(Equal("network-ssd"))
	Expect(c.EffectiveEnvironment()).To(Equal("PRODUCTION"))
	Expect(c.EffectiveZones()).To(Equal([]string{"ru-central1-a"}))
	Expect(c.EffectivePublicAccess()).To(BeTrue())
	Expect(c.EffectiveServerlessAccess()).To(BeTrue())
	Expect(c.EffectiveAllowedCidrs()).To(Equal([]string{"0.0.0.0/0"}))

	off := false
	c.PublicAccess = &off
	Expect(c.EffectivePublicAccess()).To(BeFalse())
}

func TestPostgresConfig_Validate(t *testing.T) {
	RegisterTestingT(t)

	for name, mutate := range map[string]func(*PostgresConfig){
		"old version":        func(c *PostgresConfig) { c.Version = "11" },
		"no database":        func(c *PostgresConfig) { c.Database = "" },
		"bad user":           func(c *PostgresConfig) { c.Username = "app user" },
		"short password":     func(c *PostgresConfig) { c.Password = "short" },
		"bad environment":    func(c *PostgresConfig) { c.Environment = "staging" },
		"repeated zone":      func(c *PostgresConfig) { c.Zones = []string{"ru-central1-a", "ru-central1-a"} },
		"network w/o subnet": func(c *PostgresConfig) { c.NetworkID = "enp1" },
		"bad cidr":           func(c *PostgresConfig) { c.AllowedCidrs = []string{"10.0.0.1"} },
	} {
		c := validPostgres()
		mutate(c)
		Expect(c.Validate()).To(HaveOccurred(), name)
	}

	c := validPostgres()
	c.NetworkID = "enp1"
	c.Zones = []string{"ru-central1-a", "ru-central1-b"}
	c.SubnetIDs = map[string]string{"ru-central1-a": "e9b1", "ru-central1-b": "e2l1"}
	Expect(c.Validate()).To(Succeed())
}

// The config crosses the plugin bus as JSON and is decoded through YAML.
func TestPostgresConfig_SurvivesJSONToYAMLRoundTrip(t *testing.T) {
	RegisterTestingT(t)

	on := true
	in := PostgresConfig{
		AccountConfig:      AccountConfig{CloudID: "c", FolderID: "f"},
		Name:               "db",
		Version:            "15",
		ResourcePreset:     "b2.medium",
		DiskSize:           20,
		DiskType:           "network-hdd",
		Environment:        "PRESTABLE",
		Zones:              []string{"ru-central1-a", "ru-central1-b"},
		NetworkID:          "enp",
		SubnetIDs:          map[string]string{"ru-central1-a": "s1"},
		Database:           "app",
		Username:           "u",
		Password:           "pppppppp",
		PublicAccess:       &on,
		AllowedCidrs:       []string{"1.2.3.0/24"},
		ServerlessAccess:   &on,
		DeletionProtection: true,
	}
	b, err := json.Marshal(&in)
	Expect(err).ToNot(HaveOccurred())
	var plain map[string]any
	Expect(json.Unmarshal(b, &plain)).To(Succeed())
	y, err := yaml.Marshal(plain)
	Expect(err).ToNot(HaveOccurred())
	var out PostgresConfig
	Expect(yaml.Unmarshal(y, &out)).To(Succeed())
	Expect(out).To(Equal(in))
}

func TestPostgresConfig_Registered(t *testing.T) {
	RegisterTestingT(t)

	Expect(api.GetRegisteredProviderConfigs()).To(HaveKey(ResourceTypePostgres))
	Expect(PostgresMasterHost("c9q1")).To(Equal("c-c9q1.rw.mdb.yandexcloud.net"))
}
