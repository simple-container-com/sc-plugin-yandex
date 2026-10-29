// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/simple-container-com/api/pkg/api"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"

	ycApi "github.com/simple-container-com/sc-plugin-yandex/api"
)

// toInputs mirrors what SC core's serializeConfigToInputs puts on the bus: a JSON
// round-trip, nested objects as InputMap and lists as InputList.
func toInputs(t *testing.T, cfg any) map[string]*scpluginv1.InputValue {
	t.Helper()
	b, err := json.Marshal(cfg)
	Expect(err).ToNot(HaveOccurred())
	var raw map[string]any
	Expect(json.Unmarshal(b, &raw)).To(Succeed())
	out := map[string]*scpluginv1.InputValue{}
	for k, v := range raw {
		if iv := toInputValue(v); iv != nil {
			out[k] = iv
		}
	}
	return out
}

func toInputValue(v any) *scpluginv1.InputValue {
	switch x := v.(type) {
	case string:
		return &scpluginv1.InputValue{Value: &scpluginv1.InputValue_StringValue{StringValue: x}}
	case bool:
		return &scpluginv1.InputValue{Value: &scpluginv1.InputValue_BoolValue{BoolValue: x}}
	case float64:
		return &scpluginv1.InputValue{Value: &scpluginv1.InputValue_DoubleValue{DoubleValue: x}}
	case map[string]any:
		fields := map[string]*scpluginv1.InputValue{}
		for k, item := range x {
			if iv := toInputValue(item); iv != nil {
				fields[k] = iv
			}
		}
		return &scpluginv1.InputValue{Value: &scpluginv1.InputValue_MapValue{MapValue: &scpluginv1.InputMap{Fields: fields}}}
	case []any:
		items := make([]*scpluginv1.InputValue, 0, len(x))
		for _, item := range x {
			iv := toInputValue(item)
			if iv == nil {
				iv = &scpluginv1.InputValue{}
			}
			items = append(items, iv)
		}
		return &scpluginv1.InputValue{Value: &scpluginv1.InputValue_ListValue{ListValue: &scpluginv1.InputList{Items: items}}}
	}
	return nil
}

// The serverless-container Provision path used to decode into TemplateConfig with
// a scalar-only decoder, so the client's stackConfig never arrived and the
// driver's *ServerlessContainerInput assertion always failed.
func TestDecodeInputsAs_ServerlessContainerInputKeepsStackConfig(t *testing.T) {
	RegisterTestingT(t)

	var extras any = map[string]any{"serviceAccountId": "aje-sa"}
	in := ycApi.ServerlessContainerInput{
		AccountConfig:    ycApi.AccountConfig{CloudID: "b1g-cloud", FolderID: "b1g-folder", ServiceAccountKey: `{"id":"k"}`},
		RegistryID:       "crp-registry",
		ServiceAccountID: "aje-sa",
		StackConfig: api.StackConfigSingleImage{
			Image:       &api.ContainerImage{Dockerfile: "Dockerfile", Context: ".", Build: &api.ContainerImageBuild{Args: map[string]string{"A": "b"}}},
			Domain:      "app.example.ru",
			Env:         map[string]string{"LOG_LEVEL": "debug"},
			Secrets:     map[string]string{"API_KEY": "v"},
			Uses:        []string{"mongodb"},
			MaxMemory:   lo.ToPtr(512),
			Timeout:     lo.ToPtr(30),
			CloudExtras: &extras,
		},
	}

	var out ycApi.ServerlessContainerInput
	Expect(decodeInputsAs(toInputs(t, &in), &out)).To(Succeed())

	Expect(out.FolderID).To(Equal("b1g-folder"))
	Expect(out.CloudID).To(Equal("b1g-cloud"))
	Expect(out.ServiceAccountKey).To(Equal(`{"id":"k"}`))
	Expect(out.RegistryID).To(Equal("crp-registry"))
	Expect(out.ServiceAccountID).To(Equal("aje-sa"))
	Expect(out.StackConfig.Image).ToNot(BeNil())
	Expect(out.StackConfig.Image.Dockerfile).To(Equal("Dockerfile"))
	Expect(out.StackConfig.Image.Build.Args).To(HaveKeyWithValue("A", "b"))
	Expect(out.StackConfig.Domain).To(Equal("app.example.ru"))
	Expect(out.StackConfig.Env).To(HaveKeyWithValue("LOG_LEVEL", "debug"))
	Expect(out.StackConfig.Secrets).To(HaveKeyWithValue("API_KEY", "v"))
	Expect(out.StackConfig.Uses).To(Equal([]string{"mongodb"}))
	Expect(out.StackConfig.MaxMemory).To(Equal(lo.ToPtr(512)))
	Expect(out.StackConfig.Timeout).To(Equal(lo.ToPtr(30)))
	ce, err := out.CloudExtras()
	Expect(err).ToNot(HaveOccurred())
	Expect(ce).ToNot(BeNil())
}

type noopMocks struct{}

func (noopMocks) NewResource(args sdk.MockResourceArgs) (string, resource.PropertyMap, error) {
	return args.Name + "-id", args.Inputs, nil
}

func (noopMocks) Call(args sdk.MockCallArgs) (resource.PropertyMap, error) { return args.Args, nil }

func TestComputeContextFromRequest(t *testing.T) {
	RegisterTestingT(t)

	err := sdk.RunErr(func(pctx *sdk.Context) error {
		req := &scpluginv1.ProvisionRequest{
			Header: &scpluginv1.RequestHeader{StackName: "backend", Environment: "staging-ru"},
			ComputeEnvVars: []*scpluginv1.ComputeEnvVariable{
				{Name: "MONGO_DB", Value: "app", ResourceName: "mongodb", ResourceType: "mongodb-atlas", StackName: "backend"},
				{Name: "MONGO_URI", Value: "mongodb+srv://u:p@h", Secret: true, ResourceName: "mongodb", ResourceType: "mongodb-atlas", StackName: "backend"},
				{Name: "", Value: "dropped"},
			},
		}
		cc := computeContextFromRequest(pctx, req)
		Expect(cc).ToNot(BeNil())
		Expect(cc.EnvVariables()).To(HaveLen(1))
		Expect(cc.EnvVariables()[0].Name).To(Equal("MONGO_DB"))
		Expect(cc.EnvVariables()[0].Value).To(Equal("app"))
		Expect(cc.SecretEnvVariables()).To(HaveLen(1))
		Expect(cc.SecretEnvVariables()[0].Name).To(Equal("MONGO_URI"))
		Expect(cc.SecretEnvVariables()[0].ResourceType).To(Equal("mongodb-atlas"))
		return nil
	}, sdk.WithMocks("project", "stack", noopMocks{}))
	Expect(err).ToNot(HaveOccurred())
}

// An older SC core sends no compute env. The context must still be non-nil so
// the template runs with an empty context instead of panicking.
func TestComputeContextFromRequest_OlderCoreSendsNothing(t *testing.T) {
	RegisterTestingT(t)

	err := sdk.RunErr(func(pctx *sdk.Context) error {
		cc := computeContextFromRequest(pctx, &scpluginv1.ProvisionRequest{})
		Expect(cc).ToNot(BeNil())
		Expect(cc.EnvVariables()).To(BeEmpty())
		Expect(cc.SecretEnvVariables()).To(BeEmpty())
		Expect(cc.Dependencies()).To(BeEmpty())
		return nil
	}, sdk.WithMocks("project", "stack", noopMocks{}))
	Expect(err).ToNot(HaveOccurred())
}
