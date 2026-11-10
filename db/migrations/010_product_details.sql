-- Richer catalogue entries: what the product is, where it comes from, and (optionally)
-- who we usually buy it from. Purchase prices stay on purchase orders, never here.
ALTER TABLE products
    ADD COLUMN description        text CHECK (description IS NULL OR length(description) <= 2000),
    ADD COLUMN origin             text CHECK (origin IS NULL OR length(origin) <= 80),
    ADD COLUMN supplier_id        uuid REFERENCES organizations (id),
    ADD COLUMN supplier_item_code text CHECK (supplier_item_code IS NULL OR length(supplier_item_code) <= 80);
CREATE INDEX products_supplier_idx ON products (supplier_id) WHERE supplier_id IS NOT NULL;
