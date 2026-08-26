-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE restaurants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id text NOT NULL UNIQUE,
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    address text NOT NULL,
    is_accepting_orders boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT restaurants_external_id_not_blank CHECK (btrim(external_id) <> ''),
    CONSTRAINT restaurants_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT restaurants_address_not_blank CHECK (btrim(address) <> '')
);

CREATE TABLE restaurant_integrations (
    restaurant_id uuid PRIMARY KEY REFERENCES restaurants(id) ON DELETE CASCADE,
    partner_api_key_hash bytea NOT NULL UNIQUE,
    order_endpoint_url text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT restaurant_integrations_hash_length CHECK (octet_length(partner_api_key_hash) = 32),
    CONSTRAINT restaurant_integrations_url_not_blank CHECK (btrim(order_endpoint_url) <> '')
);

CREATE TABLE menus (
    restaurant_id uuid PRIMARY KEY REFERENCES restaurants(id) ON DELETE CASCADE,
    version bigint NOT NULL,
    payload_hash bytea NOT NULL,
    currency text NOT NULL DEFAULT 'RUB',
    published_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT menus_version_positive CHECK (version > 0),
    CONSTRAINT menus_hash_length CHECK (octet_length(payload_hash) = 32),
    CONSTRAINT menus_currency_rub CHECK (currency = 'RUB')
);

CREATE TABLE menu_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id uuid NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,
    external_item_id text NOT NULL,
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    unit_price_minor bigint NOT NULL,
    currency text NOT NULL DEFAULT 'RUB',
    available boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT menu_items_external_id_not_blank CHECK (btrim(external_item_id) <> ''),
    CONSTRAINT menu_items_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT menu_items_price_positive CHECK (unit_price_minor > 0),
    CONSTRAINT menu_items_currency_rub CHECK (currency = 'RUB'),
    CONSTRAINT menu_items_restaurant_external_unique UNIQUE (restaurant_id, external_item_id)
);

CREATE INDEX menu_items_restaurant_available_idx
    ON menu_items (restaurant_id, available, name, id);

CREATE TABLE orders (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL,
    restaurant_id uuid NOT NULL REFERENCES restaurants(id) ON DELETE RESTRICT,
    restaurant_name_snapshot text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    status text NOT NULL,
    total_price_minor bigint NOT NULL,
    currency text NOT NULL DEFAULT 'RUB',
    dispatch_started_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT orders_idempotency_key_not_blank CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT orders_request_hash_length CHECK (octet_length(request_hash) = 32),
    CONSTRAINT orders_restaurant_name_not_blank CHECK (btrim(restaurant_name_snapshot) <> ''),
    CONSTRAINT orders_status_valid CHECK (
        status IN ('pending_confirmation', 'accepted', 'preparing', 'ready', 'delivering', 'delivered', 'rejected', 'cancelled')
    ),
    CONSTRAINT orders_total_positive CHECK (total_price_minor > 0),
    CONSTRAINT orders_currency_rub CHECK (currency = 'RUB'),
    CONSTRAINT orders_user_idempotency_unique UNIQUE (user_id, idempotency_key)
);

CREATE INDEX orders_user_history_idx ON orders (user_id, created_at DESC, id DESC);
CREATE INDEX orders_restaurant_status_idx ON orders (restaurant_id, status, created_at DESC, id DESC);

CREATE TABLE order_items (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    menu_item_id uuid NOT NULL,
    external_item_id text NOT NULL,
    name_snapshot text NOT NULL,
    unit_price_minor bigint NOT NULL,
    quantity integer NOT NULL,
    line_total_minor bigint NOT NULL,
    currency text NOT NULL DEFAULT 'RUB',
    CONSTRAINT order_items_external_id_not_blank CHECK (btrim(external_item_id) <> ''),
    CONSTRAINT order_items_name_not_blank CHECK (btrim(name_snapshot) <> ''),
    CONSTRAINT order_items_unit_price_positive CHECK (unit_price_minor > 0),
    CONSTRAINT order_items_quantity_positive CHECK (quantity > 0),
    CONSTRAINT order_items_line_total_valid CHECK (line_total_minor = unit_price_minor * quantity),
    CONSTRAINT order_items_currency_rub CHECK (currency = 'RUB'),
    CONSTRAINT order_items_order_menu_unique UNIQUE (order_id, menu_item_id)
);

CREATE INDEX order_items_order_idx ON order_items (order_id, id);

CREATE TABLE order_status_history (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    status text NOT NULL,
    actor text NOT NULL,
    reason text,
    changed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT order_status_history_status_valid CHECK (
        status IN ('pending_confirmation', 'accepted', 'preparing', 'ready', 'delivering', 'delivered', 'rejected', 'cancelled')
    ),
    CONSTRAINT order_status_history_actor_valid CHECK (actor IN ('user', 'platform', 'restaurant', 'worker'))
);

CREATE INDEX order_status_history_order_idx ON order_status_history (order_id, changed_at, id);

CREATE TABLE outbox_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type text NOT NULL,
    aggregate_id uuid NOT NULL,
    event_type text NOT NULL,
    payload jsonb NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_at timestamptz,
    processed_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_events_aggregate_type_valid CHECK (aggregate_type = 'order'),
    CONSTRAINT outbox_events_event_type_valid CHECK (event_type = 'order.created'),
    CONSTRAINT outbox_events_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_events_status_valid CHECK (status IN ('pending', 'processing', 'processed', 'exhausted', 'cancelled')),
    CONSTRAINT outbox_events_attempts_nonnegative CHECK (attempts >= 0),
    CONSTRAINT outbox_events_aggregate_event_unique UNIQUE (aggregate_id, event_type)
);

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at, created_at, id)
    WHERE status = 'pending';

CREATE INDEX outbox_events_processing_idx
    ON outbox_events (locked_at, id)
    WHERE status = 'processing';

-- +goose Down
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS order_status_history;
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS menu_items;
DROP TABLE IF EXISTS menus;
DROP TABLE IF EXISTS restaurant_integrations;
DROP TABLE IF EXISTS restaurants;
DROP EXTENSION IF EXISTS pgcrypto;
