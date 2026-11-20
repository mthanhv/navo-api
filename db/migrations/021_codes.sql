-- Product codes (owner decision 2026-11-20): typed by the user or numbered SP-00001, SP-00002…
-- Any insert without a code (forms, imports, AI drafts) is numbered by the default.
CREATE SEQUENCE product_code_seq;
CREATE FUNCTION next_product_code() RETURNS text LANGUAGE plpgsql AS $$
DECLARE c text;
BEGIN
    LOOP
        c := 'SP-' || lpad(nextval('product_code_seq')::text, 5, '0');
        EXIT WHEN NOT EXISTS (SELECT 1 FROM products WHERE code = c);
    END LOOP;
    RETURN c;
END $$;
ALTER TABLE products ADD COLUMN code text;
WITH o AS (SELECT id, row_number() OVER (ORDER BY created_at, id) AS n FROM products)
UPDATE products p SET code = 'SP-' || lpad(o.n::text, 5, '0') FROM o WHERE p.id = o.id;
SELECT setval('product_code_seq', GREATEST((SELECT count(*) FROM products), 1), (SELECT count(*) FROM products) > 0);
ALTER TABLE products ALTER COLUMN code SET DEFAULT next_product_code(),
    ALTER COLUMN code SET NOT NULL,
    ADD CONSTRAINT products_code_format CHECK (code ~ '^[A-Z0-9][A-Z0-9._-]{1,29}$');
CREATE UNIQUE INDEX products_code_key ON products (code);
