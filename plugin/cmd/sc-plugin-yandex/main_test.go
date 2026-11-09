// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/samber/lo"
	"github.com/simple-container-com/api/pkg/api"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
	"github.com/simple-container-com/api/pkg/plugin/runtime"

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

// The yandex plugin's Provision wires ProvisionRequest.compute_env_vars
// into pApi.ProvisionParams.ComputeContext via runtime.NewProvisionCollector.
// The pool-separation invariant (incoming env does not bleed into outgoing
// ToResponse) lives in api/pkg/plugin/runtime TestNewProvisionCollector_SeedsIncomingPools;
// these tests pin that the yandex plugin uses the shared helper rather than
// the in-tree pApi.NewComputeContextCollector it previously wrapped (which
// routed incoming vars through outgoing mutator methods — trap 14 class).
func TestProvisionCollectorFromRequest(t *testing.T) {
	RegisterTestingT(t)

	req := &scpluginv1.ProvisionRequest{
		Header: &scpluginv1.RequestHeader{StackName: "backend", Environment: "staging-ru"},
		ComputeEnvVars: []*scpluginv1.ComputeEnvVariable{
			{Name: "MONGO_DB", Value: "app", ResourceName: "mongodb", ResourceType: "mongodb-atlas", StackName: "backend"},
			{Name: "MONGO_URI", Value: "mongodb+srv://u:p@h", Secret: true, ResourceName: "mongodb", ResourceType: "mongodb-atlas", StackName: "backend"},
			{Name: "", Value: "dropped"},
		},
	}
	cc := runtime.NewProvisionCollector(req)
	Expect(cc).ToNot(BeNil())
	Expect(cc.EnvVariables()).To(HaveLen(1))
	Expect(cc.EnvVariables()[0].Name).To(Equal("MONGO_DB"))
	Expect(cc.EnvVariables()[0].Value).To(Equal("app"))
	Expect(cc.SecretEnvVariables()).To(HaveLen(1))
	Expect(cc.SecretEnvVariables()[0].Name).To(Equal("MONGO_URI"))
	Expect(cc.SecretEnvVariables()[0].ResourceType).To(Equal("mongodb-atlas"))
}

// An older SC core sends no compute env. The context must still be non-nil so
// the template runs with an empty context instead of panicking.
func TestProvisionCollectorFromRequest_OlderCoreSendsNothing(t *testing.T) {
	RegisterTestingT(t)

	cc := runtime.NewProvisionCollector(&scpluginv1.ProvisionRequest{})
	Expect(cc).ToNot(BeNil())
	Expect(cc.EnvVariables()).To(BeEmpty())
	Expect(cc.SecretEnvVariables()).To(BeEmpty())
	Expect(cc.Dependencies()).To(BeEmpty())
}

// Every template type this plugin advertises on its Schema must decode
// the template_config into the concrete Go type its ToXxxConfig
// converter asserts on. The handler used to hoist the decode above the
// switch and land every arm with `*TemplateConfig`; `yc-static-website`
// failed with `template config is not of type
// *yandex.StaticWebsiteTemplateConfig: *yandex.TemplateConfig` on every
// landing-ru-apex deploy (2026-11-09), and `yc-serverless-container`
// worked by accident because its converter happens to accept the
// generic. This table test pins the right concrete type gets to each
// converter — add a case here when a new template type lands.
func TestConvertStackConfig_EveryTemplateDecodesSpecificType(t *testing.T) {
	RegisterTestingT(t)

	acct := map[string]any{
		"cloudId":           "b1g-cloud",
		"folderId":          "b1g-folder",
		"serviceAccountKey": `{"id":"k"}`,
	}

	cases := []struct {
		name         string
		templateType string
		// templateConfig values that WOULD be dropped on silent fall-
		// through to the generic *TemplateConfig — a positive signal
		// the specific type was used.
		templateConfig map[string]any
		stackConfig    map[string]any
	}{
		{
			name:         "yc-serverless-container → TemplateConfig",
			templateType: ycApi.TemplateTypeYandexServerlessContainer,
			// AccountConfig fields are shared, so a minimal case is enough
			// here; the discriminating case is yc-static-website below.
			templateConfig: acct,
			stackConfig: map[string]any{
				"image": map[string]any{"name": "cr.yandex/crp/app:v1"},
			},
		},
		{
			name:         "yc-static-website → StaticWebsiteTemplateConfig",
			templateType: ycApi.TemplateTypeStaticWebsite,
			// edgeCacheSettings + gzipOn + redirectHttpToHttps only exist
			// on StaticWebsiteTemplateConfig. If the handler decodes into
			// the generic TemplateConfig, they're silently dropped and
			// ToStaticWebsiteConfig's type assertion fails.
			templateConfig: map[string]any{
				"cloudId":             "b1g-cloud",
				"folderId":            "b1g-folder",
				"serviceAccountKey":   `{"id":"k"}`,
				"edgeCacheSettings":   7200,
				"gzipOn":              true,
				"redirectHttpToHttps": true,
			},
			stackConfig: map[string]any{
				// StackConfigStatic.Site is `json:",inline" yaml:",inline"`
				// but SC-core's sender (PlainToInputs → json.Marshal)
				// emits it NESTED as `"Site":{...}` because encoding/json
				// doesn't honour `,inline` on NAMED fields. Earlier
				// revisions of this test put Site's fields FLAT at the
				// top (what yaml.v3's inline decoder expects) — which
				// passed against the yaml-roundtrip but did not model
				// the real wire. Live caught on landing-ru-apex run
				// 42603017630 as "yc-static-website is missing a
				// domain". The nested shape forces the ConvertStack
				// Config decode onto the `jsonRoundTrip` path.
				"bundleDir": "./bundle",
				"location":  "ru-central1",
				"Site": map[string]any{
					"domain":        "site.simple-container.com",
					"baseDnsZone":   "simple-container.com",
					"indexDocument": "index.html",
					"errorDocument": "404.html",
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			RegisterTestingT(t)
			p := newTestPlugin()
			req := &scpluginv1.ConvertStackConfigRequest{
				Header:         &scpluginv1.RequestHeader{StackName: "test", Environment: "staging-ru"},
				TemplateType:   tc.templateType,
				StackName:      "test",
				StackDir:       "/tmp",
				TemplateConfig: toInputs(t, tc.templateConfig),
				StackConfig:    toInputs(t, tc.stackConfig),
			}
			resp, err := p.ConvertStackConfig(nil, req)
			Expect(err).ToNot(HaveOccurred(), "ConvertStackConfig for %q must not fail on type-assert", tc.templateType)
			Expect(resp).ToNot(BeNil())
			Expect(resp.Input).ToNot(BeEmpty(), "response Input must carry the typed converter's output")
		})
	}
}
