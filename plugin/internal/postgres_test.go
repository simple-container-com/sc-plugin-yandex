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

func postgresInput(cfg *yandex.PostgresConfig) api.ResourceInput {
	return api.ResourceInput{
		Descriptor:  &api.ResourceDescriptor{Type: yandex.ResourceTypePostgres, Name: "main-db", Config: api.Config{Config: cfg}},
		StackParams: &api.StackParams{Environment: "prod"},
	}
}

func basePostgres() *yandex.PostgresConfig {
	return &yandex.PostgresConfig{
		AccountConfig: yandex.AccountConfig{CloudID: "test-cloud", FolderID: "test-folder"},
		Database:      "app",
		Username:      "app",
		Password:      "correct-horse-battery",
	}
}

func runPostgres(cfg *yandex.PostgresConfig) (*bucketMocks, error) {
	mocks := newBucketMocks()
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		params, err := bucketProvisionParams(ctx)
		if err != nil {
			return err
		}
		_, err = Postgres(ctx, api.Stack{Name: "clients"}, postgresInput(cfg), params)
		return err
	}, pulumi.WithMocks("project", "stack", mocks))
	return mocks, err
}

func TestPostgres_DefaultsCreateNetworkAndPublicSingleHost(t *testing.T) {
	RegisterTestingT(t)

	mocks, err := runPostgres(basePostgres())
	Expect(err).ToNot(HaveOccurred())

	Expect(mocks.countOf("yandex:index/vpcNetwork:VpcNetwork")).To(Equal(1))
	Expect(mocks.countOf("yandex:index/vpcSubnet:VpcSubnet")).To(Equal(1))
	subnet := mocks.inputsOf("yandex:index/vpcSubnet:VpcSubnet")
	Expect(subnet["zone"].StringValue()).To(Equal("ru-central1-a"))

	sg := mocks.inputsOf("yandex:index/vpcSecurityGroup:VpcSecurityGroup")
	ingress := sg["ingresses"].ArrayValue()
	Expect(ingress).To(HaveLen(1), "only the pooler port is opened")
	Expect(ingress[0].ObjectValue()["port"].NumberValue()).To(Equal(float64(yandex.PostgresPort)))
	Expect(ingress[0].ObjectValue()["v4CidrBlocks"].ArrayValue()[0].StringValue()).To(Equal("0.0.0.0/0"))

	cluster := mocks.inputsOf("yandex:index/mdbPostgresqlCluster:MdbPostgresqlCluster")
	Expect(cluster["environment"].StringValue()).To(Equal("PRODUCTION"))
	conf := cluster["config"].ObjectValue()
	Expect(conf["version"].StringValue()).To(Equal("16"))
	res := conf["resources"].ObjectValue()
	Expect(res["resourcePresetId"].StringValue()).To(Equal("s2.micro"))
	Expect(res["diskSize"].NumberValue()).To(Equal(10.0))
	Expect(conf["access"].ObjectValue()["serverless"].BoolValue()).To(BeTrue())
	hosts := cluster["hosts"].ArrayValue()
	Expect(hosts).To(HaveLen(1))
	Expect(hosts[0].ObjectValue()["assignPublicIp"].BoolValue()).To(BeTrue())

	user := mocks.inputsOf("yandex:index/mdbPostgresqlUser:MdbPostgresqlUser")
	Expect(user["name"].StringValue()).To(Equal("app"))
	Expect(user["password"].IsSecret()).To(BeTrue(), "the password must never reach state in plain text")
	db := mocks.inputsOf("yandex:index/mdbPostgresqlDatabase:MdbPostgresqlDatabase")
	Expect(db["name"].StringValue()).To(Equal("app"))
	Expect(db["owner"].StringValue()).To(Equal("app"))
}

func TestPostgres_ExistingNetworkAndReplicas(t *testing.T) {
	RegisterTestingT(t)

	cfg := basePostgres()
	cfg.NetworkID = "enp-existing"
	cfg.Zones = []string{"ru-central1-a", "ru-central1-b"}
	cfg.SubnetIDs = map[string]string{"ru-central1-a": "e9b-a", "ru-central1-b": "e2l-b"}
	off := false
	cfg.PublicAccess = &off
	cfg.AllowedCidrs = []string{"10.0.0.0/8"}
	mocks, err := runPostgres(cfg)
	Expect(err).ToNot(HaveOccurred())

	Expect(mocks.countOf("yandex:index/vpcNetwork:VpcNetwork")).To(Equal(0))
	Expect(mocks.countOf("yandex:index/vpcSubnet:VpcSubnet")).To(Equal(0))
	cluster := mocks.inputsOf("yandex:index/mdbPostgresqlCluster:MdbPostgresqlCluster")
	Expect(cluster["networkId"].StringValue()).To(Equal("enp-existing"))
	hosts := cluster["hosts"].ArrayValue()
	Expect(hosts).To(HaveLen(2))
	Expect(hosts[1].ObjectValue()["subnetId"].StringValue()).To(Equal("e2l-b"))
	Expect(hosts[0].ObjectValue()["assignPublicIp"].BoolValue()).To(BeFalse())
}

func TestPostgres_RejectsInvalidConfigBeforeCreatingAnything(t *testing.T) {
	RegisterTestingT(t)

	cfg := basePostgres()
	cfg.Password = "short"
	mocks, err := runPostgres(cfg)
	Expect(err).To(MatchError(ContainSubstring("password")))
	Expect(mocks.countOf("yandex:index/mdbPostgresqlCluster:MdbPostgresqlCluster")).To(Equal(0))
}

// The consumer contract mirrors aws-rds-postgres: bare PG* plus a _<RESOURCE> copy,
// the password and the URL secret.
func TestPostgresConnection_ConsumerEnv(t *testing.T) {
	RegisterTestingT(t)

	c := PostgresConnection{Host: "c-c9q.rw.mdb.yandexcloud.net", Port: "6432", Database: "app", User: "app", Password: "p@ss/word"}
	env, secret := c.ConsumerEnv("main-db")
	Expect(env["PGHOST"]).To(Equal("c-c9q.rw.mdb.yandexcloud.net"))
	Expect(env["PGHOST_MAIN_DB"]).To(Equal("c-c9q.rw.mdb.yandexcloud.net"))
	Expect(env["PGPORT"]).To(Equal("6432"))
	Expect(env["PGSSLMODE"]).To(Equal("require"))
	Expect(env["DATABASE_URL"]).To(Equal("postgresql://app:p%40ss%2Fword@c-c9q.rw.mdb.yandexcloud.net:6432/app?sslmode=require"))
	Expect(secret).To(HaveKey("PGPASSWORD"))
	Expect(secret).To(HaveKey("PGPASSWORD_MAIN_DB"))
	Expect(secret).To(HaveKey("DATABASE_URL"))
	Expect(secret).ToNot(HaveKey("PGHOST"))
	Expect(c.Placeholders()["password"]).To(Equal("p@ss/word"))
}
