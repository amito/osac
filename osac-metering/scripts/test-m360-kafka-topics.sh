#!/usr/bin/env bash
# Copyright (c) 2026 Red Hat Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
# in compliance with the License. You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0

set -euo pipefail

rendered_file=$(mktemp)
trap 'rm -f "$rendered_file"' EXIT

assert_topic() {
	local env_name="$1"
	local topic="$2"

	if ! awk -v env="$env_name" -v expected="\"$topic\"" '
		$1 == "-" && $2 == "name:" && $3 == env {
			if (getline > 0 && $1 == "value:" && $2 == expected) {
				found = 1
			}
		}
		END { exit found ? 0 : 1 }
	' "$rendered_file"; then
		printf 'rendered M360 deployment missing %s=%s\n' "$env_name" "$topic" >&2
		return 1
	fi
}

helm template m360-test charts/osac-metering \
	--set m360Adapter.enabled=true \
	--set m360Adapter.output.protocol=kafka \
	--set m360Adapter.output.kafka.brokers=m360.example:9092 >"$rendered_file"

assert_topic M360_KAFKA_TOPIC_BMAAS m360.metering.bmaas
assert_topic M360_KAFKA_TOPIC_EXTERNALIP m360.metering.externalip
assert_topic M360_KAFKA_TOPIC_NATGATEWAY m360.metering.natgateway
assert_topic M360_KAFKA_TOPIC_VOLUME m360.metering.volume

helm template m360-test charts/osac-metering \
	--set m360Adapter.enabled=true \
	--set m360Adapter.output.protocol=kafka \
	--set m360Adapter.output.kafka.brokers=m360.example:9092 \
	--set-string m360Adapter.output.kafka.topics.bmaas=custom.bmaas \
	--set-string m360Adapter.output.kafka.topics.externalip=custom.externalip \
	--set-string m360Adapter.output.kafka.topics.natgateway=custom.natgateway \
	--set-string m360Adapter.output.kafka.topics.volume=custom.volume >"$rendered_file"

assert_topic M360_KAFKA_TOPIC_BMAAS custom.bmaas
assert_topic M360_KAFKA_TOPIC_EXTERNALIP custom.externalip
assert_topic M360_KAFKA_TOPIC_NATGATEWAY custom.natgateway
assert_topic M360_KAFKA_TOPIC_VOLUME custom.volume
