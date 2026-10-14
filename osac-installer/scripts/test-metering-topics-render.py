#!/usr/bin/env python3
"""Verify M360 topic values render through the OSAC umbrella chart."""

from __future__ import annotations

import pathlib
import subprocess
from typing import Any

import yaml

INSTALLER = pathlib.Path(__file__).resolve().parents[1]
CHART = INSTALLER / "charts/osac"
DEFAULT_TOPICS = {
    "M360_KAFKA_TOPIC_BMAAS": "m360.metering.bmaas",
    "M360_KAFKA_TOPIC_EXTERNALIP": "m360.metering.externalip",
    "M360_KAFKA_TOPIC_NATGATEWAY": "m360.metering.natgateway",
    "M360_KAFKA_TOPIC_VOLUME": "m360.metering.volume",
}
CUSTOM_TOPICS = {
    "M360_KAFKA_TOPIC_BMAAS": "custom.bmaas",
    "M360_KAFKA_TOPIC_EXTERNALIP": "custom.externalip",
    "M360_KAFKA_TOPIC_NATGATEWAY": "custom.natgateway",
    "M360_KAFKA_TOPIC_VOLUME": "custom.volume",
}
TOPIC_VALUES = {
    "M360_KAFKA_TOPIC_BMAAS": "bmaas",
    "M360_KAFKA_TOPIC_EXTERNALIP": "externalip",
    "M360_KAFKA_TOPIC_NATGATEWAY": "natgateway",
    "M360_KAFKA_TOPIC_VOLUME": "volume",
}


def render(topics: dict[str, str] | None = None) -> list[dict[str, Any]]:
    command = [
        "helm",
        "template",
        "m360-topics-test",
        str(CHART),
        "--values",
        str(CHART / "ci/default-values.yaml"),
        "--set",
        "global.osacDeploymentId=ci/osac",
        "--set",
        "metering.enabled=true",
        "--set",
        "metering.m360Adapter.enabled=true",
        "--set",
        "metering.m360Adapter.output.protocol=kafka",
        "--set",
        "metering.m360Adapter.output.kafka.brokers=m360.example:9092",
    ]
    for env_name, topic in (topics or {}).items():
        value_name = TOPIC_VALUES[env_name]
        command.extend(
            (
                "--set-string",
                f"metering.m360Adapter.output.kafka.topics.{value_name}={topic}",
            )
        )

    result = subprocess.run(command, check=False, capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(f"Helm render failed:\n{result.stderr}")
    return [doc for doc in yaml.safe_load_all(result.stdout) if isinstance(doc, dict)]


def m360_adapter(docs: list[dict[str, Any]]) -> dict[str, Any]:
    matches = [
        doc
        for doc in docs
        if doc.get("kind") == "Deployment"
        and doc.get("metadata", {}).get("name", "").endswith("m360-adapter")
    ]
    if len(matches) != 1:
        raise AssertionError(f"expected one M360 adapter Deployment, found {len(matches)}")
    return matches[0]


def assert_topics(docs: list[dict[str, Any]], expected: dict[str, str]) -> None:
    deployment = m360_adapter(docs)
    containers = deployment["spec"]["template"]["spec"]["containers"]
    container = next(item for item in containers if item["name"] == "m360-adapter")
    actual = {item["name"]: item.get("value") for item in container["env"]}
    for name, topic in expected.items():
        assert actual.get(name) == topic, f"{name}: expected {topic!r}, got {actual.get(name)!r}"


def main() -> None:
    assert_topics(render(), DEFAULT_TOPICS)
    assert_topics(render(CUSTOM_TOPICS), CUSTOM_TOPICS)
    print("M360 Kafka topic values render through the OSAC umbrella chart")


if __name__ == "__main__":
    main()
