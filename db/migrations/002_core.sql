-- Core A2: partners, catalog, contracts and lines, purchase orders, goods receipts, delivery orders.
-- Composite foreign keys make cross-contract links impossible at the database level (BR-03):
-- a PO line, receipt line or delivery line can only point at a line of its own contract/PO.

CREATE TABLE organizations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code        text NOT NULL CHECK (code ~ '^[A-Z0-9]{2,12}$'),
    name        text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 300),
    is_customer boolean NOT NULL DEFAULT false,
    is_supplier boolean NOT NULL DEFAULT false,
    tax_code    text CHECK (tax_code IS NULL OR tax_code ~ '^[0-9]{10}(-[0-9]{3})?$'),
    owner_id    uuid REFERENCES users (id),
    version     bigint NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (is_customer OR is_supplier)
);
CREATE UNIQUE INDEX organizations_code_key ON organizations (code);

CREATE TABLE products (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name              text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 300),
    manufacturer      text CHECK (manufacturer IS NULL OR length(manufacturer) <= 120),
    manufacturer_code text CHECK (manufacturer_code IS NULL OR length(manufacturer_code) <= 80),
    unit              text NOT NULL CHECK (length(btrim(unit)) BETWEEN 1 AND 20),
    version           bigint NOT NULL DEFAULT 1,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX products_manufacturer_code_key
    ON products (lower(coalesce(manufacturer, '')), lower(manufacturer_code)) WHERE manufacturer_code IS NOT NULL;

CREATE TABLE contracts (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number             text UNIQUE CHECK (number IS NULL OR number ~ '^SC-[0-9]{2}-[0-9]{4}$'),
    draft_number       text NOT NULL UNIQUE,
    status             text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'closed', 'cancelled')),
    customer_id        uuid NOT NULL REFERENCES organizations (id),
    title              text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 300),
    customer_reference text CHECK (customer_reference IS NULL OR length(customer_reference) <= 100),
    currency           char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    signed_on          date,
    owner_id           uuid NOT NULL REFERENCES users (id),
    -- per-contract counters for child document numbers (PO-26-0142-01, ...)
    po_seq             int NOT NULL DEFAULT 0,
    gr_seq             int NOT NULL DEFAULT 0,
    do_seq             int NOT NULL DEFAULT 0,
    version            bigint NOT NULL DEFAULT 1,
    created_by         uuid NOT NULL REFERENCES users (id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (status = 'draft' OR (number IS NOT NULL AND signed_on IS NOT NULL))
);
CREATE INDEX contracts_owner_idx ON contracts (owner_id);
CREATE INDEX contracts_customer_idx ON contracts (customer_id);

CREATE TABLE contract_lines (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id             uuid NOT NULL REFERENCES contracts (id) ON DELETE CASCADE,
    line_no                 int NOT NULL CHECK (line_no > 0),
    product_id              uuid NOT NULL REFERENCES products (id),
    name_on_contract        text NOT NULL CHECK (length(btrim(name_on_contract)) BETWEEN 1 AND 500),
    unit                    text NOT NULL CHECK (length(btrim(unit)) BETWEEN 1 AND 20),
    quantity                numeric(20, 6) NOT NULL CHECK (quantity > 0),
    unit_price              numeric(24, 6) NOT NULL CHECK (unit_price >= 0),
    tax_rate                numeric(9, 6) CHECK (tax_rate IS NULL OR (tax_rate >= 0 AND tax_rate <= 1)),
    due_on                  date,
    last_business_change_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contract_id, line_no),
    UNIQUE (contract_id, id)
);

CREATE TABLE purchase_orders (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id  uuid NOT NULL REFERENCES contracts (id),
    number       text NOT NULL UNIQUE CHECK (number ~ '^PO-[0-9]{2}-[0-9]{4}-[0-9]{2,}$'),
    supplier_id  uuid NOT NULL REFERENCES organizations (id),
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'confirmed', 'cancelled')),
    currency     char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    confirmed_at timestamptz,
    confirmed_by uuid REFERENCES users (id),
    version      bigint NOT NULL DEFAULT 1,
    created_by   uuid NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, contract_id)
);
CREATE INDEX purchase_orders_contract_idx ON purchase_orders (contract_id);

CREATE TABLE purchase_order_lines (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    purchase_order_id  uuid NOT NULL,
    contract_id        uuid NOT NULL,
    contract_line_id   uuid NOT NULL,
    quantity           numeric(20, 6) NOT NULL CHECK (quantity > 0),
    supplier_item_name text CHECK (supplier_item_name IS NULL OR length(supplier_item_name) <= 500),
    unit_price         numeric(24, 6) NOT NULL CHECK (unit_price >= 0),
    expected_on        date,
    FOREIGN KEY (purchase_order_id, contract_id) REFERENCES purchase_orders (id, contract_id) ON DELETE CASCADE,
    FOREIGN KEY (contract_id, contract_line_id) REFERENCES contract_lines (contract_id, id),
    UNIQUE (purchase_order_id, id)
);
CREATE INDEX purchase_order_lines_contract_line_idx ON purchase_order_lines (contract_line_id);

CREATE TABLE goods_receipts (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id       uuid NOT NULL,
    purchase_order_id uuid NOT NULL,
    number            text NOT NULL UNIQUE CHECK (number ~ '^GR-[0-9]{2}-[0-9]{4}-[0-9]{2,}$'),
    status            text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'confirmed')),
    received_on       date NOT NULL,
    confirmed_at      timestamptz,
    confirmed_by      uuid REFERENCES users (id),
    version           bigint NOT NULL DEFAULT 1,
    created_by        uuid NOT NULL REFERENCES users (id),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (purchase_order_id, contract_id) REFERENCES purchase_orders (id, contract_id),
    UNIQUE (id, purchase_order_id)
);

CREATE TABLE goods_receipt_lines (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    goods_receipt_id       uuid NOT NULL,
    purchase_order_id      uuid NOT NULL,
    purchase_order_line_id uuid NOT NULL,
    accepted_quantity      numeric(20, 6) NOT NULL DEFAULT 0 CHECK (accepted_quantity >= 0),
    rejected_quantity      numeric(20, 6) NOT NULL DEFAULT 0 CHECK (rejected_quantity >= 0),
    CHECK (accepted_quantity + rejected_quantity > 0),
    FOREIGN KEY (goods_receipt_id, purchase_order_id) REFERENCES goods_receipts (id, purchase_order_id) ON DELETE CASCADE,
    FOREIGN KEY (purchase_order_id, purchase_order_line_id) REFERENCES purchase_order_lines (purchase_order_id, id),
    UNIQUE (goods_receipt_id, purchase_order_line_id)
);
CREATE INDEX goods_receipt_lines_po_line_idx ON goods_receipt_lines (purchase_order_line_id);

-- A delivery order is planned as a draft (tentative or firm date) and confirmed when delivered.
CREATE TABLE delivery_orders (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id  uuid NOT NULL REFERENCES contracts (id),
    number       text NOT NULL UNIQUE CHECK (number ~ '^DO-[0-9]{2}-[0-9]{4}-[0-9]{2,}$'),
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'confirmed')),
    planned_on   date,
    firmness     text NOT NULL DEFAULT 'tentative' CHECK (firmness IN ('tentative', 'firm')),
    route        text NOT NULL DEFAULT 'via_company' CHECK (route IN ('via_company', 'direct')),
    delivered_on date,
    confirmed_at timestamptz,
    confirmed_by uuid REFERENCES users (id),
    version      bigint NOT NULL DEFAULT 1,
    created_by   uuid NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, contract_id),
    CHECK (status <> 'confirmed' OR delivered_on IS NOT NULL)
);
CREATE INDEX delivery_orders_contract_idx ON delivery_orders (contract_id);

CREATE TABLE delivery_order_lines (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    delivery_order_id uuid NOT NULL,
    contract_id       uuid NOT NULL,
    contract_line_id  uuid NOT NULL,
    quantity          numeric(20, 6) NOT NULL CHECK (quantity > 0),
    FOREIGN KEY (delivery_order_id, contract_id) REFERENCES delivery_orders (id, contract_id) ON DELETE CASCADE,
    FOREIGN KEY (contract_id, contract_line_id) REFERENCES contract_lines (contract_id, id),
    UNIQUE (delivery_order_id, contract_line_id)
);
CREATE INDEX delivery_order_lines_contract_line_idx ON delivery_order_lines (contract_line_id);

-- Confirmed documents are history: never hard-deleted (AC-13). Corrections use reversal documents.
CREATE FUNCTION forbid_delete_unless_draft() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'draft' THEN
        RAISE EXCEPTION 'navo:document_not_draft';
    END IF;
    RETURN OLD;
END $$;

CREATE TRIGGER contracts_no_delete BEFORE DELETE ON contracts FOR EACH ROW EXECUTE FUNCTION forbid_delete_unless_draft();
CREATE TRIGGER purchase_orders_no_delete BEFORE DELETE ON purchase_orders FOR EACH ROW EXECUTE FUNCTION forbid_delete_unless_draft();
CREATE TRIGGER goods_receipts_no_delete BEFORE DELETE ON goods_receipts FOR EACH ROW EXECUTE FUNCTION forbid_delete_unless_draft();
CREATE TRIGGER delivery_orders_no_delete BEFORE DELETE ON delivery_orders FOR EACH ROW EXECUTE FUNCTION forbid_delete_unless_draft();
