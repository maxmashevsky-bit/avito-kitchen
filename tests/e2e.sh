#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root_dir"

e2e_tmp=$(mktemp -d "${TMPDIR:-/tmp}/avito-kitchen-e2e.XXXXXX")
cleanup() {
  if [ "${E2E_KEEP_STACK:-0}" != "1" ]; then
    docker compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -rf "$e2e_tmp"
}
trap cleanup EXIT INT TERM

docker compose down --volumes --remove-orphans >/dev/null 2>&1 || true
docker compose up --build --wait

kitchen_url=${KITCHEN_E2E_URL:-http://localhost:8080}
restaurant_id=10000000-0000-4000-8000-000000000001
partner_key=${DEMO_PARTNER_API_KEY:-demo-partner-key-change-me}
user_id=30000000-0000-4000-8000-000000000001

menu_file="$e2e_tmp/menu.json"
attempt=0
until curl -fsS "$kitchen_url/api/v1/restaurants/$restaurant_id/menu" -o "$menu_file"; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    echo "demo menu was not published" >&2
    exit 1
  fi
  sleep 1
done

menu_item_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["items"][0]["id"])' "$menu_file")
unit_price=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["items"][0]["unit_price_minor"])' "$menu_file")

create_payload=$(printf '{"restaurant_id":"%s","items":[{"menu_item_id":"%s","quantity":1,"seen_unit_price_minor":%s}]}' "$restaurant_id" "$menu_item_id" "$unit_price")
order_file="$e2e_tmp/order.json"
http_status=$(curl -sS -o "$order_file" -w '%{http_code}' \
  -X POST "$kitchen_url/api/v1/orders" \
  -H 'Content-Type: application/json' \
  -H "X-User-ID: $user_id" \
  -H 'Idempotency-Key: e2e-accepted-order' \
  --data "$create_payload")
[ "$http_status" = "201" ] || { cat "$order_file" >&2; exit 1; }
order_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$order_file")

attempt=0
while :; do
  curl -fsS "$kitchen_url/api/v1/orders/$order_id" -H "X-User-ID: $user_id" -o "$order_file"
  order_status=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "$order_file")
  [ "$order_status" = "accepted" ] && break
  attempt=$((attempt + 1))
  [ "$attempt" -lt 30 ] || { echo "order did not become accepted: $order_status" >&2; exit 1; }
  sleep 1
done

replay_file="$e2e_tmp/replay.json"
http_status=$(curl -sS -o "$replay_file" -w '%{http_code}' \
  -X POST "$kitchen_url/api/v1/orders" \
  -H 'Content-Type: application/json' \
  -H "X-User-ID: $user_id" \
  -H 'Idempotency-Key: e2e-accepted-order' \
  --data "$create_payload")
[ "$http_status" = "200" ] || { cat "$replay_file" >&2; exit 1; }
replay_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$replay_file")
[ "$replay_id" = "$order_id" ] || { echo "idempotent replay returned another order" >&2; exit 1; }

for next_status in preparing ready delivering delivered; do
  response_file="$e2e_tmp/status-$next_status.json"
  http_status=$(curl -sS -o "$response_file" -w '%{http_code}' \
    -X PATCH "$kitchen_url/partner/v1/orders/$order_id/status" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $partner_key" \
    --data "{\"status\":\"$next_status\"}")
  [ "$http_status" = "200" ] || { cat "$response_file" >&2; exit 1; }
done

shortage_payload=$(printf '{"restaurant_id":"%s","items":[{"menu_item_id":"%s","quantity":100,"seen_unit_price_minor":%s}]}' "$restaurant_id" "$menu_item_id" "$unit_price")
shortage_file="$e2e_tmp/shortage.json"
http_status=$(curl -sS -o "$shortage_file" -w '%{http_code}' \
  -X POST "$kitchen_url/api/v1/orders" \
  -H 'Content-Type: application/json' \
  -H "X-User-ID: $user_id" \
  -H 'Idempotency-Key: e2e-rejected-order' \
  --data "$shortage_payload")
[ "$http_status" = "201" ] || { cat "$shortage_file" >&2; exit 1; }
shortage_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$shortage_file")

attempt=0
while :; do
  curl -fsS "$kitchen_url/api/v1/orders/$shortage_id" -H "X-User-ID: $user_id" -o "$shortage_file"
  shortage_status=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "$shortage_file")
  [ "$shortage_status" = "rejected" ] && break
  attempt=$((attempt + 1))
  [ "$attempt" -lt 30 ] || { echo "shortage order did not become rejected: $shortage_status" >&2; exit 1; }
  sleep 1
done

echo "E2E passed: accepted/delivered=$order_id rejected=$shortage_id replay=$replay_id"
