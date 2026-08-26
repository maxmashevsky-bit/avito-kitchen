-- +goose Up
CREATE TABLE items (
    external_item_id text PRIMARY KEY,
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    unit_price_minor bigint NOT NULL,
    currency text NOT NULL DEFAULT 'RUB',
    available boolean NOT NULL DEFAULT true,
    available_quantity integer NOT NULL DEFAULT 0,
    reserved_quantity integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT items_external_id_not_blank CHECK (btrim(external_item_id) <> ''),
    CONSTRAINT items_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT items_price_positive CHECK (unit_price_minor > 0),
    CONSTRAINT items_currency_rub CHECK (currency = 'RUB'),
    CONSTRAINT items_available_quantity_nonnegative CHECK (available_quantity >= 0),
    CONSTRAINT items_reserved_quantity_nonnegative CHECK (reserved_quantity >= 0)
);

CREATE TABLE incoming_orders (
    platform_order_id uuid PRIMARY KEY,
    platform_restaurant_id uuid NOT NULL,
    request_hash bytea NOT NULL,
    status text NOT NULL,
    total_price_minor bigint NOT NULL,
    currency text NOT NULL DEFAULT 'RUB',
    rejection_code text,
    rejection_message text,
    rejection_external_item_ids text[],
    received_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT incoming_orders_request_hash_length CHECK (octet_length(request_hash) = 32),
    CONSTRAINT incoming_orders_status_valid CHECK (status IN ('accepted', 'rejected')),
    CONSTRAINT incoming_orders_total_positive CHECK (total_price_minor > 0),
    CONSTRAINT incoming_orders_currency_rub CHECK (currency = 'RUB'),
    CONSTRAINT incoming_orders_rejection_consistent CHECK (
        (status = 'accepted' AND rejection_code IS NULL AND rejection_message IS NULL AND rejection_external_item_ids IS NULL)
        OR (status = 'rejected' AND rejection_code IS NOT NULL AND rejection_message IS NOT NULL AND cardinality(rejection_external_item_ids) > 0)
    )
);

CREATE INDEX incoming_orders_received_idx ON incoming_orders (received_at DESC, platform_order_id DESC);

CREATE TABLE incoming_order_items (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    platform_order_id uuid NOT NULL REFERENCES incoming_orders(platform_order_id) ON DELETE CASCADE,
    platform_menu_item_id uuid NOT NULL,
    external_item_id text NOT NULL,
    name_snapshot text NOT NULL,
    unit_price_minor bigint NOT NULL,
    quantity integer NOT NULL,
    line_total_minor bigint NOT NULL,
    currency text NOT NULL DEFAULT 'RUB',
    CONSTRAINT incoming_order_items_external_id_not_blank CHECK (btrim(external_item_id) <> ''),
    CONSTRAINT incoming_order_items_name_not_blank CHECK (btrim(name_snapshot) <> ''),
    CONSTRAINT incoming_order_items_unit_price_positive CHECK (unit_price_minor > 0),
    CONSTRAINT incoming_order_items_quantity_positive CHECK (quantity > 0),
    CONSTRAINT incoming_order_items_line_total_valid CHECK (line_total_minor = unit_price_minor * quantity),
    CONSTRAINT incoming_order_items_currency_rub CHECK (currency = 'RUB'),
    CONSTRAINT incoming_order_items_platform_item_unique UNIQUE (platform_order_id, platform_menu_item_id)
);

CREATE INDEX incoming_order_items_order_idx ON incoming_order_items (platform_order_id, id);

-- +goose Down
DROP TABLE IF EXISTS incoming_order_items;
DROP TABLE IF EXISTS incoming_orders;
DROP TABLE IF EXISTS items;
