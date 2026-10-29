// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"testing"

	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// The keys are a contract with SC core's parentInfoToDependencyOutputs.
func TestParentInfoFromDependencyOutputs(t *testing.T) {
	RegisterTestingT(t)

	Expect(parentInfoFromDependencyOutputs(nil)).To(BeNil(), "an older core sends no parent: refuse rather than guess")

	p := parentInfoFromDependencyOutputs(map[string]string{
		"sc.parentStack.stackName":         "clients",
		"sc.parentStack.parentEnv":         "prod",
		"sc.parentStack.fullReference":     "org/clients/prod",
		"sc.parentStack.usesResource":      "true",
		"sc.parentStack.dependsOnResource": "main-db",
	})
	Expect(p.StackName).To(Equal("clients"))
	Expect(p.FullReference).To(Equal("org/clients/prod"))
	Expect(p.UsesResource).To(BeTrue())
	Expect(p.DependsOnResource.Name).To(Equal("main-db"))
}

func TestDecodeComputeEnvEntries(t *testing.T) {
	RegisterTestingT(t)

	str := func(s string) *scpluginv1.InputValue {
		return &scpluginv1.InputValue{Value: &scpluginv1.InputValue_StringValue{StringValue: s}}
	}
	item := &scpluginv1.InputValue{Value: &scpluginv1.InputValue_MapValue{MapValue: &scpluginv1.InputMap{Fields: map[string]*scpluginv1.InputValue{
		"name":   str("PGPASSWORD"),
		"value":  str("s3cret"),
		"secret": {Value: &scpluginv1.InputValue_BoolValue{BoolValue: true}},
	}}}}
	inputs := map[string]*scpluginv1.InputValue{
		computeEnvInputKey: {Value: &scpluginv1.InputValue_ListValue{ListValue: &scpluginv1.InputList{Items: []*scpluginv1.InputValue{item}}}},
	}
	var entries []computeEnvEntry
	b, err := yaml.Marshal(decodePlain(inputs[computeEnvInputKey]))
	Expect(err).ToNot(HaveOccurred())
	Expect(yaml.Unmarshal(b, &entries)).To(Succeed())
	Expect(entries).To(Equal([]computeEnvEntry{{Name: "PGPASSWORD", Value: "s3cret", Secret: true}}))
}
