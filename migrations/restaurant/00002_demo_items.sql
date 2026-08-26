-- +goose Up
INSERT INTO items (
    external_item_id, name, description, unit_price_minor, currency, available, available_quantity
)
VALUES
    ('margherita', 'Пицца Маргарита', 'Томаты, моцарелла и базилик', 59000, 'RUB', true, 20),
    ('pepperoni', 'Пицца Пепперони', 'Пепперони, моцарелла и томатный соус', 69000, 'RUB', true, 15),
    ('lemonade', 'Домашний лимонад', 'Лимон, мята и газированная вода', 19000, 'RUB', true, 30)
ON CONFLICT (external_item_id) DO NOTHING;

-- +goose Down
DELETE FROM items WHERE external_item_id IN ('margherita', 'pepperoni', 'lemonade');
