#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
config_file=${1:-"$script_dir/config.yaml"}

request_timeout_seconds=${REQUEST_TIMEOUT_SECONDS:-15}
retry_delay_seconds=${RETRY_DELAY_SECONDS:-5}
max_attempts=${MAX_ATTEMPTS:-5}

usage() {
	printf 'Usage: %s [config.yaml]\n' "$0" >&2
	printf '\nEnvironment overrides:\n' >&2
	printf '  REQUEST_TIMEOUT_SECONDS  Request timeout (default: 15)\n' >&2
	printf '  RETRY_DELAY_SECONDS      Delay between 429 retries (default: 5)\n' >&2
	printf '  MAX_ATTEMPTS             Total attempts per provider/value (default: 5)\n' >&2
}

is_positive_integer() {
	case "$1" in
		''|*[!0-9]*|0)
			return 1
		;;
		*)
			return 0
		;;
	esac
}

is_nonnegative_integer() {
	case "$1" in
		''|*[!0-9]*)
			return 1
		;;
		*)
			return 0
		;;
	esac
}

if [ ! -f "$config_file" ]; then
	printf 'Config file not found: %s\n' "$config_file" >&2
	usage
	exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
	printf 'curl is required but was not found in PATH\n' >&2
	exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
	printf 'jq is required but was not found in PATH\n' >&2
	exit 1
fi

if ! is_positive_integer "$request_timeout_seconds"; then
	printf 'REQUEST_TIMEOUT_SECONDS must be a positive integer\n' >&2
	exit 1
fi

if ! is_nonnegative_integer "$retry_delay_seconds"; then
	printf 'RETRY_DELAY_SECONDS must be a non-negative integer\n' >&2
	exit 1
fi

if ! is_positive_integer "$max_attempts"; then
	printf 'MAX_ATTEMPTS must be a positive integer\n' >&2
	exit 1
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/gonka-reasoning-effort.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

providers_file=$work_dir/providers.tsv
results_file=$work_dir/results.tsv
response_body=$work_dir/response-body
curl_error=$work_dir/curl-error

# The project config has a deliberately small, flat provider schema. Parse only
# provider blocks so the script remains usable without requiring a YAML package.
awk '
function clean(value) {
	sub(/^[^:]*:[[:space:]]*/, "", value)
	sub(/[[:space:]]+#.*$/, "", value)
	gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
	if (value ~ /^".*"$/) {
		sub(/^"/, "", value)
		sub(/"$/, "", value)
	}
	return value
}
function emit() {
	if (name != "") {
		print name "\t" base_url "\t" api_key "\t" model_alias
	}
}
/^providers:[[:space:]]*$/ {
	in_providers = 1
	next
}
/^[^[:space:]#]/ {
	if ($0 !~ /^providers:[[:space:]]*$/) {
		in_providers = 0
	}
}
in_providers && /^  - name:/ {
	emit()
	name = clean($0)
	base_url = ""
	api_key = ""
	model_alias = ""
	next
}
in_providers && /^    base_url:/ { base_url = clean($0); next }
in_providers && /^    api_key:/ { api_key = clean($0); next }
in_providers && /^    model_alias:/ { model_alias = clean($0); next }
END { emit() }
' "$config_file" > "$providers_file"

if [ ! -s "$providers_file" ]; then
	printf 'No providers found in %s\n' "$config_file" >&2
	exit 1
fi

printf '\n== detected providers ===\n'
provider_number=0
while IFS="$(printf '\t')" read -r provider_name base_url api_key model_alias; do
	[ -n "$provider_name" ] || continue
	provider_number=$((provider_number + 1))
	printf '%s. %s - %s - %s\n' "$provider_number" "$provider_name" "$base_url" "$model_alias"
done < "$providers_file"

shorten_message() {
	printf '%s' "$1" |
		sed -E 's/[|[:space:]]+/ /g; s/sk-[A-Za-z0-9_-]{8,}/<redacted>/g' |
		cut -c1-80
}

response_message() {
	jq -r '.error.message // .message // .error.code // .error.type // empty' \
		"$response_body" 2>/dev/null |
		head -n 1
}

summarize_http_error() {
	status_code=$1
	message=$2
	case "$message" in
		*unsupported*|*Unsupported*)
			printf '%s unsupported' "$status_code"
		;;
		*rate*limit*|*Rate*limit*|*too*many*|*Too*many*)
			printf '%s rate limited' "$status_code"
		;;
		*)
			printf '%s error' "$status_code"
		;;
	esac
}

record_result() {
	provider_name=$1
	effort=$2
	result=$3
	printf '%s\t%s\t%s\n' "$provider_name" "$effort" "$result" >> "$results_file"
}

log_result() {
	request_number=$((request_number + 1))
	printf '%s. %s - %s - %s\n' "$request_number" "$1" "$2" "$3"
}

efforts='low medium high xhigh max'
: > "$results_file"
printf '\n== sending requests ===\n'
request_number=0

while IFS="$(printf '\t')" read -r provider_name base_url api_key model_alias; do
	[ -n "$provider_name" ] || continue
	[ -n "$base_url" ] && [ -n "$api_key" ] && [ -n "$model_alias" ] || {
		for effort in $efforts; do
			record_result "$provider_name" "$effort" 'config error'
			log_result "$provider_name" "$effort" 'config error'
		done
		continue
	}

	endpoint=${base_url%/}/chat/completions

	for effort in $efforts; do
		payload=$(jq -cn \
			--arg model "$model_alias" \
			--arg effort "$effort" \
			'{model:$model,messages:[{role:"user",content:"Reply with OK."}],reasoning_effort:$effort,max_tokens:8,stream:false}')

		attempt=1
		result='network error'
		while :; do
			: > "$curl_error"
			if http_code=$(curl --silent --show-error --location \
				--connect-timeout "$request_timeout_seconds" \
				--max-time "$request_timeout_seconds" \
				--header "Authorization: Bearer $api_key" \
				--header 'Content-Type: application/json' \
				--header 'Accept: application/json' \
				--data-binary "$payload" \
				--output "$response_body" \
				--write-out '%{http_code}' \
				"$endpoint" 2>"$curl_error"); then
				curl_status=0
			else
				curl_status=$?
			fi

			if [ "$curl_status" -ne 0 ]; then
				curl_message=$(shorten_message "$(tr '\n' ' ' < "$curl_error")")
				case "$curl_message" in
					*timed[[:space:]]out*|*Timeout*)
						result='timeout'
					;;
					*)
						result='network error'
					;;
				esac
				break
			fi

			if [ "$http_code" = '429' ]; then
				if [ "$attempt" -lt "$max_attempts" ]; then
					printf 'Retrying %s/%s after HTTP 429 (attempt %s/%s); cooldown %ss\n' \
						"$provider_name" "$effort" "$attempt" "$max_attempts" "$retry_delay_seconds" >&2
					sleep "$retry_delay_seconds"
					attempt=$((attempt + 1))
					continue
				fi
				result='429 rate limited'
				break
			fi

			message=$(response_message)
			case "$http_code" in
				2??)
					if jq -e 'type == "object" and (.error? != null)' "$response_body" >/dev/null 2>&1; then
						result="${http_code} error"
					else
						result="${http_code} OK"
					fi
				;;
				'')
					result='network error'
				;;
				*)
					result=$(summarize_http_error "$http_code" "$message")
				;;
			esac
			break
		done

		record_result "$provider_name" "$effort" "$result"
		log_result "$provider_name" "$effort" "$result"
	done
done < "$providers_file"

widths=$(awk -F '\t' '
BEGIN {
	provider_width = length("Provider")
	low_width = length("low")
	medium_width = length("medium")
	high_width = length("high")
	xhigh_width = length("xhigh")
	max_width = length("max")
}
FILENAME == ARGV[1] {
	if (length($1) > provider_width) provider_width = length($1)
	next
}
{
	if ($2 == "low" && length($3) > low_width) low_width = length($3)
	if ($2 == "medium" && length($3) > medium_width) medium_width = length($3)
	if ($2 == "high" && length($3) > high_width) high_width = length($3)
	if ($2 == "xhigh" && length($3) > xhigh_width) xhigh_width = length($3)
	if ($2 == "max" && length($3) > max_width) max_width = length($3)
}
END {
	printf "%d %d %d %d %d %d\n", provider_width + 2, low_width + 2, medium_width + 2, high_width + 2, xhigh_width + 2, max_width + 2
}
' "$providers_file" "$results_file")

read -r provider_width low_width medium_width high_width xhigh_width max_width <<EOF
$widths
EOF

repeat_char() {
	char=$1
	count=$2
	output=''
	while [ "$count" -gt 0 ]; do
		output=$output$char
		count=$((count - 1))
	done
	printf '%s' "$output"
}

print_separator() {
	char=$1
	printf '  %s' "$(repeat_char "$char" "$provider_width")"
	printf '  %s' "$(repeat_char "$char" "$low_width")"
	printf '  %s' "$(repeat_char "$char" "$medium_width")"
	printf '  %s' "$(repeat_char "$char" "$high_width")"
	printf '  %s' "$(repeat_char "$char" "$xhigh_width")"
	printf '  %s\n' "$(repeat_char "$char" "$max_width")"
}

result_for() {
	awk -F '\t' -v provider="$1" -v effort="$2" \
		'$1 == provider && $2 == effort { print $3; exit }' "$results_file"
}

printf '\n== DONE ===\n'
printf '•  %-*s' "$provider_width" 'Provider'
printf '  %-*s' "$low_width" 'low'
printf '  %-*s' "$medium_width" 'medium'
printf '  %-*s' "$high_width" 'high'
printf '  %-*s' "$xhigh_width" 'xhigh'
printf '  %-*s\n' "$max_width" 'max'
print_separator '━'

first_row=1
while IFS="$(printf '\t')" read -r provider_name base_url api_key model_alias; do
	[ -n "$provider_name" ] || continue
	if [ "$first_row" -ne 1 ]; then
		print_separator '─'
	fi
	first_row=0
	printf '   %-*s' "$provider_width" "$provider_name"
	printf '  %-*s' "$low_width" "$(result_for "$provider_name" low)"
	printf '  %-*s' "$medium_width" "$(result_for "$provider_name" medium)"
	printf '  %-*s' "$high_width" "$(result_for "$provider_name" high)"
	printf '  %-*s' "$xhigh_width" "$(result_for "$provider_name" xhigh)"
	printf '  %-*s\n' "$max_width" "$(result_for "$provider_name" max)"
done < "$providers_file"
