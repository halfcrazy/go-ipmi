#!/usr/bin/env bash
# client_test.sh — E2E test for go-ipmi as an IPMI client.
#
# Connects to an IPMI peer (default 127.0.0.1:623).  If no peer is reachable
# and Docker is available, the script automatically starts an ipmi-simulator
# container and removes it on exit.
#
# Environment variables:
#   GOIPMI_HOST       – IPMI peer hostname      (default: 127.0.0.1)
#   GOIPMI_PORT       – IPMI peer port          (default: 623)
#   GOIPMI_USER       – IPMI username           (default: ADMIN)
#   GOIPMI_PASS       – IPMI password           (default: ADMIN)
#   IPMI_SIM_IMAGE    – simulator Docker image  (default: vaporio/ipmi-simulator:master)
#   GOIPMI_NO_DOCKER  – if set to 1, never auto-start Docker
#
# Usage:
#   ./test/e2e/client_test.sh
#   GOIPMI_HOST=10.0.0.1 ./test/e2e/client_test.sh

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

ROOT_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
GOIPMI="${ROOT_DIR}/_output/goipmi"

GOIPMI_HOST="${GOIPMI_HOST:-127.0.0.1}"
GOIPMI_PORT="${GOIPMI_PORT:-623}"
GOIPMI_USER="${GOIPMI_USER:-ADMIN}"
GOIPMI_PASS="${GOIPMI_PASS:-ADMIN}"
IPMI_SIM_IMAGE="${IPMI_SIM_IMAGE:-vaporio/ipmi-simulator:master}"
SIM_CONTAINER="goipmi-e2e-sim"

DOCKER_STARTED=false

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
cleanup() {
	if [ "${DOCKER_STARTED}" = true ]; then
		echo "==> Stopping ipmi-simulator container ..."
		docker rm -f "${SIM_CONTAINER}" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

peer_reachable() {
	# Try a quick UDP probe to see if something is listening.
	if command -v ss &>/dev/null && [ "${GOIPMI_HOST}" = "127.0.0.1" -o "${GOIPMI_HOST}" = "localhost" ]; then
		ss -uln | grep -q ":${GOIPMI_PORT} " && return 0
	fi
	# Fallback: assume reachable for remote hosts (test will fail with a clear error).
	return 1
}

ensure_peer() {
	if peer_reachable; then
		return 0
	fi

	if [ "${GOIPMI_NO_DOCKER:-0}" = "1" ] || ! command -v docker &>/dev/null; then
		echo -e "${RED}ERROR: No IPMI peer on ${GOIPMI_HOST}:${GOIPMI_PORT} and Docker unavailable.${NC}" >&2
		echo "  Start an IPMI simulator manually or install Docker." >&2
		exit 1
	fi

	echo "==> Starting ipmi-simulator (${IPMI_SIM_IMAGE}) ..."
	docker run -d --name "${SIM_CONTAINER}" -p "${GOIPMI_PORT}:623/udp" "${IPMI_SIM_IMAGE}" >/dev/null
	DOCKER_STARTED=true

	# Wait for the simulator to be ready (up to 10 s).
	local i=0
	while [ $i -lt 20 ]; do
		if peer_reachable; then
			echo "==> Simulator is ready."
			return 0
		fi
		sleep 0.5
		i=$((i + 1))
	done
	echo -e "${RED}ERROR: Simulator did not become ready in time.${NC}" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
if [ ! -x "${GOIPMI}" ]; then
	echo "==> Building goipmi ..."
	(cd "${ROOT_DIR}" && make build)
fi

GOIPMI_ARGS=(
	"${GOIPMI}"
	-H "${GOIPMI_HOST}" -p "${GOIPMI_PORT}"
	-U "${GOIPMI_USER}" -P "${GOIPMI_PASS}"
	-I lanplus
)

echo "========================================"
echo " Client E2E: go-ipmi → ${GOIPMI_HOST}:${GOIPMI_PORT}"
echo "========================================"

ensure_peer

run_test() {
	local name="$1"; shift
	echo ""
	echo "--- ${name} ---"
	if "${GOIPMI_ARGS[@]}" "$@"; then
		echo -e "${GREEN}✓ ${name}${NC}"
	else
		echo -e "${RED}✗ ${name}${NC}" >&2
		return 1
	fi
}

failures=0
run_test "chassis status"  chassis status  || ((failures++))
run_test "chassis power on" chassis power on || ((failures++))
run_test "chassis power off" chassis power off || ((failures++))

echo ""
if [ "${failures}" -eq 0 ]; then
	echo -e "${GREEN}========================================"
	echo " Client E2E: PASSED"
	echo -e "========================================${NC}"
else
	echo -e "${RED}========================================"
	echo " Client E2E: ${failures} test(s) FAILED"
	echo -e "========================================${NC}"
	exit 1
fi
