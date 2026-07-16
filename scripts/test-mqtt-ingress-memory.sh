#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
workspace="${GITHUB_WORKSPACE:-$(cd -- "$script_dir/.." && pwd)}"
cd "$workspace"

proof_tmp="$workspace/.tmp-mqtt-memory"
mkdir -p "$proof_tmp"
go_cache="$(go env GOCACHE)"
go_mod_cache="$(go env GOMODCACHE)"
mkdir -p "$go_cache" "$go_mod_cache"

run_id="${GITHUB_RUN_ID:-local-$$}"
run_attempt="${GITHUB_RUN_ATTEMPT:-1}"
broker_name="gobridge-mqtt-memory-${run_id}-${run_attempt}"
publisher_name="${broker_name}-publisher"
network_name="${broker_name}-net"
cleanup() {
  docker rm -f "$broker_name" >/dev/null 2>&1 || true
  docker rm -f "$publisher_name" >/dev/null 2>&1 || true
  docker network rm "$network_name" >/dev/null 2>&1 || true
  rm -f "$proof_tmp/mqtt-memory.test"
  rmdir "$proof_tmp" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# The measured binary uses the production allocator. Race instrumentation has
# a separate shadow heap and would not measure the configured production
# ingress bound.
docker run --rm \
  --memory 512m \
  --memory-swap 512m \
  -v "$workspace:$workspace" \
  -v "$go_cache:/root/.cache/go-build" \
  -v "$go_mod_cache:/go/pkg/mod" \
  -w "$workspace" \
  golang:1.25 \
  go test -p=1 -c -tags=longrunning \
    -o "$proof_tmp/mqtt-memory.test" ./tests/longrunning

docker network create "$network_name"
docker run -d \
  --name "$broker_name" \
  --network "$network_name" \
  --network-alias mqtt-memory-broker \
  -v "$workspace/tests/longrunning/testdata/mosquitto-memory.conf:/mosquitto/config/mosquitto.conf:ro" \
  eclipse-mosquitto:latest

docker run -d \
  --name "$publisher_name" \
  --network "$network_name" \
  --network-alias mqtt-memory-publisher \
  -e GOBRIDGE_MQTT_MEMORY_PUBLISHER_HELPER=1 \
  -e GOBRIDGE_MQTT_MEMORY_PUBLISHER_LISTEN=:1884 \
  -e GOBRIDGE_MQTT_MEMORY_BROKER_URL=tcp://mqtt-memory-broker:1883 \
  -e GOBRIDGE_MQTT_MEMORY_TOPIC=longrunning/ingress-memory \
  -e GOBRIDGE_MQTT_MEMORY_PAYLOAD_BYTES=262144 \
  -v "$workspace:$workspace" \
  -w "$workspace" \
  golang:1.25 \
  "$proof_tmp/mqtt-memory.test" \
    -test.count=1 \
    -test.run '^TestMQTTIngressMemoryPublisherProcess$' \
    -test.timeout=180s

docker run --rm \
  --memory 512m \
  --memory-swap 512m \
  --network "$network_name" \
  -e GOBRIDGE_REQUIRE_MEMORY_LIMIT=1 \
  -e MQTT_MEMORY_BROKER_URL=tcp://mqtt-memory-broker:1883 \
  -e MQTT_MEMORY_PUBLISHER_CONTROL=mqtt-memory-publisher:1884 \
  -e TMPDIR="$proof_tmp" \
  -v "$workspace:$workspace" \
  -w "$workspace" \
  golang:1.25 \
  /bin/sh -ceu '
    memory_max="$(cat /sys/fs/cgroup/memory.max)"
    [ "$memory_max" = "536870912" ] || {
      echo "expected memory.max=536870912, got $memory_max" >&2
      exit 1
    }
    swap_max="$(cat /sys/fs/cgroup/memory.swap.max)"
    [ "$swap_max" = "0" ] || {
      echo "expected memory.swap.max=0, got $swap_max" >&2
      exit 1
    }
    exec "$@"
  ' sh "$proof_tmp/mqtt-memory.test" \
    -test.count=1 \
    -test.run '^TestMQTTIngressMemory$' \
    -test.timeout=180s \
    -test.v
