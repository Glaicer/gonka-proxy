#!/bin/sh
set -eu

project_dir=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
compose_file="$project_dir/compose.smoke.yaml"

cleanup() {
	docker compose -f "$compose_file" down --volumes --remove-orphans
}
trap cleanup EXIT INT TERM

docker compose -f "$compose_file" up --build --detach

attempt=0
while [ "$attempt" -lt 30 ]; do
	if response=$(curl --fail --silent --show-error \
		--connect-timeout 1 --max-time 5 \
		--header 'Content-Type: application/json' \
		--data '{"model":"smoke-virtual-model","messages":[]}' \
		http://127.0.0.1:18080/v1/chat/completions); then
		case "$response" in
			*'"authorized":true'*'"received_model":"smoke-model"'*)
				echo "container smoke test passed"
				exit 0
				;;
		esac
	fi
	attempt=$((attempt + 1))
	sleep 1
done

echo "container smoke test failed" >&2
exit 1
