#!/bin/sh
# SPDX-License-Identifier: MIT
# Copyright The Muster Authors

# muster dev from Muster's image, with PostgreSQL, for the acceptance tests. Needs Linux (Docker's host network),
# Docker, curl and jq.
#
#   eval "$(test/muster-dev.sh up)"   starts both, creates the Service accounts terraform-acc (Admin) and
#                                     terraform-responder (Responder) with a token each, and prints MUSTER_ENDPOINT,
#                                     MUSTER_TOKEN and MUSTER_RESPONDER_TOKEN as export lines
#   test/muster-dev.sh down           removes both containers
#
# MUSTER_IMAGE replaces the image of the pin: ghcr.io/muster-io/muster:v<api/MUSTER_VERSION>, or
# ghcr.io/muster-io/muster:sha-<first 7 hex digits of api/MUSTER_COMMIT>. Everything but the export line goes to stderr.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
postgres=terraform-provider-muster-postgres
app=terraform-provider-muster-dev
app_url=http://127.0.0.1:8080
internal_url=http://127.0.0.1:8082
admin_login=admin@example.org
admin_password=muster-dev-password

log() {
	printf '%s\n' "$*" >&2
}

default_image() {
	if [ -f "$root/api/MUSTER_VERSION" ]; then
		echo "ghcr.io/muster-io/muster:v$(tr -d '[:space:]' <"$root/api/MUSTER_VERSION")"
	elif [ -f "$root/api/MUSTER_COMMIT" ]; then
		echo "ghcr.io/muster-io/muster:sha-$(tr -d '[:space:]' <"$root/api/MUSTER_COMMIT" | cut -c1-7)"
	else
		log "neither api/MUSTER_VERSION nor api/MUSTER_COMMIT exists"
		exit 1
	fi
}

down() {
	docker rm -f "$app" "$postgres" >/dev/null 2>&1 || :
}

# wait_for <what> <seconds> <command…>: run the command once a second until it succeeds.
wait_for() {
	what=$1
	seconds=$2
	shift 2
	n=0
	until "$@" >/dev/null 2>&1; do
		n=$((n + 1))
		if [ "$n" -ge "$seconds" ]; then
			log "$what is not ready after ${seconds}s"
			return 1
		fi
		sleep 1
	done
}

# request <method> <path> [body]: a request to the API with the session, whose cookie and CSRF token curl reads from
# the file $session_headers, never from its command line, which ps shows; prints the answer's body, fails outside 2xx.
request() {
	method=$1
	path=$2
	shift 2
	out=$(curl -sS -X "$method" "$app_url/api/v1$path" \
		-H "@$session_headers" -H 'Content-Type: application/json' \
		${1+--data-binary} ${1+"$1"} -w '\n%{http_code}') || return 1
	code=$(printf '%s\n' "$out" | tail -n 1)
	body=$(printf '%s\n' "$out" | sed '$d')
	case $code in
	2??) printf '%s\n' "$body" ;;
	*)
		log "$method $path answered $code: $body"
		return 1
		;;
	esac
}

# service_account <name> <role>: creates the Service account with a token and prints the token.
service_account() {
	id=$(request POST /service-accounts "$(jq -cn --arg name "$1" --arg role "$2" '{name: $name, role: $role}')" |
		jq -er .id) || return 1
	request POST "/service-accounts/$id/tokens" '{"name":"acceptance tests"}' | jq -er .value
}

up() {
	image=${MUSTER_IMAGE:-$(default_image)}
	down
	log "muster image: $image"
	docker pull -q "$image" >&2
	docker run -d --name "$postgres" -p 127.0.0.1:55432:5432 \
		-e POSTGRES_USER=muster -e POSTGRES_PASSWORD=muster -e POSTGRES_DB=muster postgres:17 >/dev/null
	wait_for PostgreSQL 60 docker exec "$postgres" pg_isready -h 127.0.0.1 -U muster -d muster
	docker run -d --name "$app" --network host "$image" dev >/dev/null
	if ! wait_for "muster dev" 120 curl -fsS "$internal_url/health/ready"; then
		docker logs --tail 50 "$app" >&2
		return 1
	fi

	# The answer's headers and the session's headers hold the session: files only this user reads, removed on exit.
	umask 077
	headers=$(mktemp)
	session_headers=$(mktemp)
	trap 'rm -f "$headers" "$session_headers"' EXIT
	csrf=$(curl -sS -f -D "$headers" -X POST "$app_url/api/v1/sessions" -H 'Content-Type: application/json' \
		-d "$(jq -cn --arg login "$admin_login" --arg password "$admin_password" '{login: $login, password: $password}')" |
		jq -er .csrf_token)
	session=$(tr -d '\r' <"$headers" | sed -n 's/^[Ss]et-[Cc]ookie: muster_session=\([^;]*\).*/\1/p' | head -n 1)
	if [ -z "$session" ]; then
		log "signing in as $admin_login set no muster_session cookie"
		return 1
	fi
	printf 'Cookie: muster_session=%s\nX-CSRF-Token: %s\n' "$session" "$csrf" >"$session_headers"

	version=$(request GET /system-status | jq -r .version)
	log "muster dev $version is ready on $app_url"
	admin_token=$(service_account terraform-acc admin)
	responder_token=$(service_account terraform-responder responder)
	printf "export MUSTER_ENDPOINT='%s' MUSTER_TOKEN='%s' MUSTER_RESPONDER_TOKEN='%s'\n" \
		"$app_url" "$admin_token" "$responder_token"
}

case ${1:-} in
up) up ;;
down) down ;;
*)
	log "usage: $0 up|down"
	exit 2
	;;
esac
