/*
Copyright (c) 2026 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
in compliance with the License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import "testing"

func TestEnableAllIfNoneSetLeavesBMaaSOptIn(t *testing.T) {
	cfg := &config{}

	cfg.enableAllIfNoneSet()

	if !cfg.enableCaaS || !cfg.enableVMaaS || !cfg.enableMaaS {
		t.Fatalf("expected CaaS, VMaaS, and MaaS to be enabled by default: %+v", cfg)
	}
	if cfg.enableBMaaS {
		t.Fatalf("expected BMaaS to remain opt-in: %+v", cfg)
	}
}
