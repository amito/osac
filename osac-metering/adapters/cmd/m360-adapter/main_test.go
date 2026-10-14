/*
Copyright (c) 2026 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
in compliance with the License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"reflect"
	"testing"
)

func TestM360KafkaTopicsFromEnvUsesDefaultsForAllRoutes(t *testing.T) {
	for name := range map[string]string{
		"M360_KAFKA_TOPIC_VMAAS":      "",
		"M360_KAFKA_TOPIC_CAAS":       "",
		"M360_KAFKA_TOPIC_MAAS":       "",
		"M360_KAFKA_TOPIC_BMAAS":      "",
		"M360_KAFKA_TOPIC_EXTERNALIP": "",
		"M360_KAFKA_TOPIC_NATGATEWAY": "",
		"M360_KAFKA_TOPIC_VOLUME":     "",
	} {
		t.Setenv(name, "")
	}

	got := m360KafkaTopicsFromEnv()
	want := map[string]string{
		"vmaas":      "m360.metering.vmaas",
		"caas":       "m360.metering.caas",
		"maas":       "m360.metering.maas",
		"bmaas":      "m360.metering.bmaas",
		"externalip": "m360.metering.externalip",
		"natgateway": "m360.metering.natgateway",
		"volume":     "m360.metering.volume",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("m360KafkaTopicsFromEnv() = %#v, want %#v", got, want)
	}
}

func TestM360KafkaTopicsFromEnvUsesResourceTopicOverrides(t *testing.T) {
	t.Setenv("M360_KAFKA_TOPIC_BMAAS", "billing.baremetal")
	t.Setenv("M360_KAFKA_TOPIC_EXTERNALIP", "billing.external-address")
	t.Setenv("M360_KAFKA_TOPIC_NATGATEWAY", "billing.egress")
	t.Setenv("M360_KAFKA_TOPIC_VOLUME", "billing.storage")

	got := m360KafkaTopicsFromEnv()
	want := map[string]string{
		"bmaas":      "billing.baremetal",
		"externalip": "billing.external-address",
		"natgateway": "billing.egress",
		"volume":     "billing.storage",
	}
	for route, topic := range want {
		if got[route] != topic {
			t.Errorf("topic for %q = %q, want %q", route, got[route], topic)
		}
	}
}
