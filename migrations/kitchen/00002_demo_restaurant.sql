-- +goose Up
INSERT INTO restaurants (id, external_id, name, description, address, is_accepting_orders)
VALUES (
    '10000000-0000-4000-8000-000000000001',
    'demo-restaurant',
    'Авито.Кухня Demo',
    'Демо-заведение для локального E2E-сценария',
    'Москва, ул. Тестовая, 1',
    true
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO restaurant_integrations (restaurant_id, partner_api_key_hash, order_endpoint_url)
VALUES (
    '10000000-0000-4000-8000-000000000001',
    decode('fbf417589e5811819fb0e3bab01c241ec0499699e914a12c237eb29349a29b6d', 'hex'),
    'http://restaurant-demo:8081/integration/v1/orders'
)
ON CONFLICT (restaurant_id) DO NOTHING;

-- +goose Down
DELETE FROM restaurant_integrations WHERE restaurant_id = '10000000-0000-4000-8000-000000000001';
DELETE FROM restaurants WHERE id = '10000000-0000-4000-8000-000000000001';
