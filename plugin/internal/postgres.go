// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/pkg/errors"
	"github.com/samber/lo"

	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	"github.com/simple-container-com/api/pkg/util"
	sdkYandex "github.com/simple-container-com/pulumi-yandex/sdk/go/yandex"
	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

// PostgresOutput is every resource a yc-postgres registers.
type PostgresOutput struct {
	Network       *sdkYandex.VpcNetwork
	Subnets       []*sdkYandex.VpcSubnet
	SecurityGroup *sdkYandex.VpcSecurityGroup
	Cluster       *sdkYandex.MdbPostgresqlCluster
	User          *sdkYandex.MdbPostgresqlUser
	Database      *sdkYandex.MdbPostgresqlDatabase
}

// Outputs lists what to await.
func (o *PostgresOutput) Outputs() []sdk.Output {
	outs := []sdk.Output{o.SecurityGroup.ID(), o.Cluster.ID(), o.User.ID(), o.Database.ID()}
	if o.Network != nil {
		outs = append(outs, o.Network.ID())
	}
	for _, s := range o.Subnets {
		outs = append(outs, s.ID())
	}
	return outs
}

// Postgres provisions a `yc-postgres` resource in the parent stack: network (unless
// one is given), a security group opening the pooler port to AllowedCidrs, the
// cluster, its user and its database. Connection details are exported for the
// compute processor to hand to consumers.
func Postgres(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	cfg, ok := input.Descriptor.Config.Config.(*yandex.PostgresConfig)
	if !ok {
		return nil, errors.Errorf("failed to cast config to *yandex.PostgresConfig for %q", input.Descriptor.Type)
	}
	if params.Provider == nil {
		return nil, errors.Errorf("provider must not be nil for postgres %q", input.Descriptor.Name)
	}
	if err := api.ConvertAuth(cfg, &cfg.AccountConfig); err != nil {
		return nil, errors.Wrapf(err, "failed to convert auth config for postgres %q", input.Descriptor.Name)
	}
	if cfg.FolderID == "" {
		return nil, errors.Errorf("folderId must be set for postgres %q in stack %q", input.Descriptor.Name, stack.Name)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	name := postgresClusterName(input, cfg)
	opts := []sdk.ResourceOption{sdk.Provider(params.Provider)}
	out := &PostgresOutput{}
	zones := cfg.EffectiveZones()

	var networkID sdk.StringInput = sdk.String(cfg.NetworkID)
	subnetIDs := map[string]sdk.StringInput{}
	for z, id := range cfg.SubnetIDs {
		subnetIDs[z] = sdk.String(id)
	}
	if cfg.NetworkID == "" {
		network, err := sdkYandex.NewVpcNetwork(ctx, name+"-net", &sdkYandex.VpcNetworkArgs{
			Name:        sdk.String(name + "-net"),
			FolderId:    sdk.String(cfg.FolderID),
			Description: sdk.String(fmt.Sprintf("network of postgres %s (stack %s)", name, stack.Name)),
		}, opts...)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create network for postgres %q", name)
		}
		out.Network = network
		networkID = network.ID().ToStringOutput()
		for i, z := range zones {
			subnet, err := sdkYandex.NewVpcSubnet(ctx, fmt.Sprintf("%s-%s", name, z), &sdkYandex.VpcSubnetArgs{
				Name:         sdk.String(fmt.Sprintf("%s-%s", name, z)),
				FolderId:     sdk.String(cfg.FolderID),
				NetworkId:    network.ID().ToStringOutput(),
				Zone:         sdk.String(z),
				V4CidrBlocks: sdk.StringArray{sdk.String(postgresSubnetCidr(i))},
			}, opts...)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to create subnet in %q for postgres %q", z, name)
			}
			out.Subnets = append(out.Subnets, subnet)
			subnetIDs[z] = subnet.ID().ToStringOutput()
		}
	}

	allowed := sdk.StringArray{}
	for _, c := range cfg.EffectiveAllowedCidrs() {
		allowed = append(allowed, sdk.String(c))
	}
	sg, err := sdkYandex.NewVpcSecurityGroup(ctx, name+"-sg", &sdkYandex.VpcSecurityGroupArgs{
		Name:        sdk.String(name + "-sg"),
		FolderId:    sdk.String(cfg.FolderID),
		NetworkId:   networkID,
		Description: sdk.String(fmt.Sprintf("postgres %s: pooler port from allowedCidrs only", name)),
		Ingresses: sdkYandex.VpcSecurityGroupIngressArray{&sdkYandex.VpcSecurityGroupIngressArgs{
			Protocol:     sdk.String("TCP"),
			Port:         sdk.Int(yandex.PostgresPort),
			V4CidrBlocks: allowed,
			Description:  sdk.String("postgres via the connection pooler"),
		}},
		Egresses: sdkYandex.VpcSecurityGroupEgressArray{&sdkYandex.VpcSecurityGroupEgressArgs{
			Protocol:     sdk.String("ANY"),
			FromPort:     sdk.Int(0),
			ToPort:       sdk.Int(65535),
			V4CidrBlocks: sdk.StringArray{sdk.String("0.0.0.0/0")},
		}},
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create security group for postgres %q", name)
	}
	out.SecurityGroup = sg

	hosts := sdkYandex.MdbPostgresqlClusterHostArray{}
	for _, z := range zones {
		hosts = append(hosts, &sdkYandex.MdbPostgresqlClusterHostArgs{
			Zone:           sdk.String(z),
			SubnetId:       subnetIDs[z],
			AssignPublicIp: sdk.Bool(cfg.EffectivePublicAccess()),
		})
	}
	params.Log.Info(ctx.Context(), "configure yandex managed postgres %q (v%s, %s, %d GB, %d host(s))",
		name, cfg.EffectiveVersion(), cfg.EffectivePreset(), cfg.EffectiveDiskSize(), len(zones))
	cluster, err := sdkYandex.NewMdbPostgresqlCluster(ctx, name, &sdkYandex.MdbPostgresqlClusterArgs{
		Name:               sdk.String(name),
		FolderId:           sdk.String(cfg.FolderID),
		Environment:        sdk.String(cfg.EffectiveEnvironment()),
		NetworkId:          networkID,
		SecurityGroupIds:   sdk.StringArray{sg.ID().ToStringOutput()},
		DeletionProtection: sdk.Bool(cfg.DeletionProtection),
		Config: &sdkYandex.MdbPostgresqlClusterConfigArgs{
			Version: sdk.String(cfg.EffectiveVersion()),
			Resources: &sdkYandex.MdbPostgresqlClusterConfigResourcesArgs{
				ResourcePresetId: sdk.String(cfg.EffectivePreset()),
				DiskSize:         sdk.Int(cfg.EffectiveDiskSize()),
				DiskTypeId:       sdk.String(cfg.EffectiveDiskType()),
			},
			Access: &sdkYandex.MdbPostgresqlClusterConfigAccessArgs{
				Serverless: sdk.Bool(cfg.EffectiveServerlessAccess()),
			},
		},
		Hosts: hosts,
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create postgres cluster %q", name)
	}
	out.Cluster = cluster

	password := sdk.ToSecret(sdk.String(cfg.Password)).(sdk.StringOutput)
	user, err := sdkYandex.NewMdbPostgresqlUser(ctx, name+"-user", &sdkYandex.MdbPostgresqlUserArgs{
		ClusterId: cluster.ID().ToStringOutput(),
		Name:      sdk.String(cfg.Username),
		Password:  password,
	}, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create postgres user %q", cfg.Username)
	}
	out.User = user

	db, err := sdkYandex.NewMdbPostgresqlDatabase(ctx, name+"-db", &sdkYandex.MdbPostgresqlDatabaseArgs{
		ClusterId: cluster.ID().ToStringOutput(),
		Name:      sdk.String(cfg.Database),
		Owner:     user.Name,
	}, append(opts, sdk.DependsOn([]sdk.Resource{user}))...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create postgres database %q", cfg.Database)
	}
	out.Database = db

	exportName := input.Descriptor.Name
	ctx.Export(toPostgresHostExport(exportName), cluster.ID().ToStringOutput().ApplyT(yandex.PostgresMasterHost).(sdk.StringOutput))
	ctx.Export(toPostgresPortExport(exportName), sdk.String(strconv.Itoa(yandex.PostgresPort)))
	ctx.Export(toPostgresDatabaseExport(exportName), sdk.String(cfg.Database))
	ctx.Export(toPostgresUserExport(exportName), sdk.String(cfg.Username))
	ctx.Export(toPostgresPasswordExport(exportName), password)

	return &api.ResourceOutput{Ref: out}, nil
}

func postgresClusterName(input api.ResourceInput, cfg *yandex.PostgresConfig) string {
	return yandexName(input.ToResName(lo.If(cfg.Name == "", input.Descriptor.Name).Else(cfg.Name)), "pg-")
}

// postgresSubnetCidr gives each created subnet its own /24, so a multi-zone
// cluster's subnets do not overlap.
func postgresSubnetCidr(i int) string {
	return fmt.Sprintf("10.%d.0.0/24", 130+i)
}

func toPostgresHostExport(resName string) string     { return resName + "-yc-pg-host" }
func toPostgresPortExport(resName string) string     { return resName + "-yc-pg-port" }
func toPostgresDatabaseExport(resName string) string { return resName + "-yc-pg-database" }
func toPostgresUserExport(resName string) string     { return resName + "-yc-pg-user" }
func toPostgresPasswordExport(resName string) string { return resName + "-yc-pg-password" }

// PostgresConnection is what a consumer of a yc-postgres gets.
type PostgresConnection struct {
	Host, Port, Database, User, Password string
}

// ConsumerEnv returns the env vars a `uses:` consumer receives, and which of them
// are secret. The names follow the aws-rds-postgres compute processor: the bare
// PG* set plus a `_<RESOURCE>` suffixed copy, so a service using two databases can
// tell them apart. PGSSLMODE=require: YC refuses unencrypted connections to a
// public host, and verify-full would need YC's CA on the client
// (yandex.PostgresCACertURL) — set PGSSLMODE yourself to opt into it.
func (c PostgresConnection) ConsumerEnv(resName string) (env map[string]string, secret map[string]bool) {
	u := url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword(c.User, c.Password),
		Host:     c.Host + ":" + c.Port,
		Path:     "/" + c.Database,
		RawQuery: "sslmode=require",
	}
	base := map[string]string{
		"PGHOST":       c.Host,
		"PGPORT":       c.Port,
		"PGDATABASE":   c.Database,
		"PGUSER":       c.User,
		"PGPASSWORD":   c.Password,
		"PGSSLMODE":    "require",
		"DATABASE_URL": u.String(),
	}
	env = map[string]string{}
	secret = map[string]bool{}
	for k, v := range base {
		for _, name := range []string{k, fmt.Sprintf("%s_%s", k, resName)} {
			name = util.ToEnvVariableName(name)
			env[name] = v
			if k == "PGPASSWORD" || k == "DATABASE_URL" {
				secret[name] = true
			}
		}
	}
	return env, secret
}

// Placeholders are the `${resource:<name>.<key>}` values a consumer can template.
func (c PostgresConnection) Placeholders() map[string]string {
	return map[string]string{
		"host":     c.Host,
		"port":     c.Port,
		"database": c.Database,
		"user":     c.User,
		"password": c.Password,
		"url":      c.Host + ":" + c.Port,
	}
}

// ReadPostgresConnection reads a yc-postgres's connection details out of its parent
// stack, the read half of what Postgres exports.
func ReadPostgresConnection(ctx *sdk.Context, stack api.Stack, resName string, parent *pApi.ParentInfo) (*PostgresConnection, *sdk.StackReference, error) {
	if parent == nil || parent.FullReference == "" {
		return nil, nil, errors.Errorf("parent stack must be known to consume postgres %q in %q", resName, stack.Name)
	}
	suffix := lo.If(parent.DependsOnResource != nil, "--"+lo.FromPtr(parent.DependsOnResource).Name).Else("")
	ref, err := sdk.NewStackReference(ctx, fmt.Sprintf("%s--%s--%s%s--yc-pg-ref", stack.Name, parent.StackName, resName, suffix), &sdk.StackReferenceArgs{
		Name: sdk.String(parent.FullReference).ToStringOutput(),
	})
	if err != nil {
		return nil, nil, err
	}
	read := func(export string, secret bool) (string, error) {
		v, err := pApi.GetParentOutput(ref, export, parent.FullReference, secret)
		if err != nil {
			return "", errors.Wrapf(err, "failed to read %q from parent stack %q", export, parent.FullReference)
		}
		if v == "" {
			return "", errors.Errorf("%q is empty in parent stack %q — has the parent been provisioned since yc-postgres %q was added?", export, parent.FullReference, resName)
		}
		return v, nil
	}
	conn := &PostgresConnection{}
	for _, f := range []struct {
		export string
		secret bool
		dst    *string
	}{
		{toPostgresHostExport(resName), false, &conn.Host},
		{toPostgresPortExport(resName), false, &conn.Port},
		{toPostgresDatabaseExport(resName), false, &conn.Database},
		{toPostgresUserExport(resName), false, &conn.User},
		{toPostgresPasswordExport(resName), true, &conn.Password},
	} {
		if *f.dst, err = read(f.export, f.secret); err != nil {
			return nil, nil, err
		}
	}
	return conn, ref, nil
}
