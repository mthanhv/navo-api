-- Money (REQUIREMENTS N-07, 5.11; mockups Công nợ and Trao đổi MISA). Amounts are recorded
-- in Navo first and exported to MISA SME (decision 1.4). Assumptions until O-04/O-05 close:
--  * Taxes and rounding are entered as printed on the e-invoice, never recalculated.
--  * A payment milestone has a fixed amount (usually % × contract value, editable). It becomes
--    collectable when its trigger happens; due = trigger date + days (or a fixed date).
--  * Currencies are never added together (no exchange rate yet).

ALTER TABLE contracts ADD COLUMN si_seq int NOT NULL DEFAULT 0;
ALTER TABLE organizations ADD COLUMN misa_code text CHECK (misa_code IS NULL OR length(misa_code) BETWEEN 1 AND 50);
ALTER TABLE products ADD COLUMN misa_code text CHECK (misa_code IS NULL OR length(misa_code) BETWEEN 1 AND 50);
CREATE UNIQUE INDEX organizations_misa_code_key ON organizations (misa_code) WHERE misa_code IS NOT NULL;
CREATE UNIQUE INDEX products_misa_code_key ON products (misa_code) WHERE misa_code IS NOT NULL;

CREATE TABLE payment_milestones (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id  uuid NOT NULL REFERENCES contracts (id) ON DELETE CASCADE,
    seq          int  NOT NULL,
    name         text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    percent      numeric(7, 4) CHECK (percent IS NULL OR (percent > 0 AND percent <= 100)),
    amount       numeric(24, 2) NOT NULL CHECK (amount > 0),
    trigger      text NOT NULL CHECK (trigger IN ('signing', 'delivery_complete', 'acceptance', 'date', 'manual')),
    days_after   int  NOT NULL DEFAULT 0 CHECK (days_after BETWEEN 0 AND 3650),
    fixed_due_on date CHECK ((trigger = 'date') = (fixed_due_on IS NOT NULL)),
    -- When the trigger happened (signing and full delivery are filled by Navo; acceptance/manual by a person).
    triggered_on date,
    trigger_note text,
    version      bigint NOT NULL DEFAULT 1,
    created_by   uuid NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contract_id, seq)
);

CREATE TABLE sales_invoices (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id   uuid NOT NULL REFERENCES contracts (id),
    number        text NOT NULL UNIQUE CHECK (number ~ '^SI-[0-9]{2}-[0-9]{4}-[0-9]{2,}$'),
    legal_number  text NOT NULL CHECK (length(btrim(legal_number)) BETWEEN 1 AND 50),
    issued_on     date NOT NULL,
    description   text NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 300),
    milestone_id  uuid REFERENCES payment_milestones (id),
    currency      char(3) NOT NULL,
    amount_net    numeric(24, 2) NOT NULL CHECK (amount_net >= 0),
    vat_amount    numeric(24, 2) NOT NULL CHECK (vat_amount >= 0),
    amount_total  numeric(24, 2) NOT NULL CHECK (amount_total = amount_net + vat_amount),
    status        text NOT NULL DEFAULT 'issued' CHECK (status IN ('issued', 'cancelled')),
    misa_export_id uuid,
    created_by    uuid NOT NULL REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX sales_invoices_legal_key ON sales_invoices (legal_number) WHERE status = 'issued';

CREATE TABLE receipts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number      text NOT NULL UNIQUE CHECK (number ~ '^RV-[0-9]{2}-[0-9]{4}$'),
    customer_id uuid NOT NULL REFERENCES organizations (id),
    received_on date NOT NULL,
    amount      numeric(24, 2) NOT NULL CHECK (amount > 0),
    currency    char(3) NOT NULL,
    method      text NOT NULL CHECK (method IN ('bank', 'cash')),
    reference   text CHECK (reference IS NULL OR length(reference) <= 200),
    note        text CHECK (note IS NULL OR length(note) <= 500),
    status      text NOT NULL DEFAULT 'posted' CHECK (status IN ('posted', 'cancelled')),
    misa_export_id uuid,
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE receipt_allocations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    receipt_id    uuid NOT NULL REFERENCES receipts (id),
    milestone_id  uuid NOT NULL REFERENCES payment_milestones (id),
    amount        numeric(24, 2) NOT NULL CHECK (amount > 0),
    created_by    uuid NOT NULL REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- A wrong allocation is reversed with a reason, never edited or deleted.
    reversed_at   timestamptz,
    reversed_by   uuid REFERENCES users (id),
    reverse_reason text,
    CHECK ((reversed_at IS NULL) = (reversed_by IS NULL) AND (reversed_at IS NULL) = (reverse_reason IS NULL))
);
CREATE INDEX receipt_allocations_milestone_idx ON receipt_allocations (milestone_id) WHERE reversed_at IS NULL;

CREATE TABLE purchase_invoices (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number        text NOT NULL UNIQUE CHECK (number ~ '^PI-[0-9]{2}-[0-9]{4}$'),
    supplier_id   uuid NOT NULL REFERENCES organizations (id),
    legal_number  text NOT NULL CHECK (length(btrim(legal_number)) BETWEEN 1 AND 50),
    issued_on     date NOT NULL,
    due_on        date,
    currency      char(3) NOT NULL,
    amount_net    numeric(24, 2) NOT NULL CHECK (amount_net >= 0),
    vat_amount    numeric(24, 2) NOT NULL CHECK (vat_amount >= 0),
    amount_total  numeric(24, 2) NOT NULL CHECK (amount_total = amount_net + vat_amount),
    status        text NOT NULL DEFAULT 'recorded' CHECK (status IN ('recorded', 'cancelled')),
    misa_export_id uuid,
    created_by    uuid NOT NULL REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (supplier_id, legal_number)
);
-- One supplier invoice may cover purchase orders of several contracts.
CREATE TABLE purchase_invoice_orders (
    purchase_invoice_id uuid NOT NULL REFERENCES purchase_invoices (id) ON DELETE CASCADE,
    purchase_order_id   uuid NOT NULL REFERENCES purchase_orders (id),
    PRIMARY KEY (purchase_invoice_id, purchase_order_id)
);

CREATE TABLE payments (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number        text NOT NULL UNIQUE CHECK (number ~ '^PV-[0-9]{2}-[0-9]{4}$'),
    supplier_id   uuid NOT NULL REFERENCES organizations (id),
    paid_on       date NOT NULL,
    amount        numeric(24, 2) NOT NULL CHECK (amount > 0),
    currency      char(3) NOT NULL,
    method        text NOT NULL CHECK (method IN ('bank', 'cash')),
    reference     text CHECK (reference IS NULL OR length(reference) <= 200),
    purchase_invoice_id uuid REFERENCES purchase_invoices (id),
    status        text NOT NULL DEFAULT 'posted' CHECK (status IN ('posted', 'cancelled')),
    misa_export_id uuid,
    created_by    uuid NOT NULL REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Each export to MISA: which documents went in which file (so nothing is exported twice).
CREATE TABLE misa_exports (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    period      char(7) NOT NULL CHECK (period ~ '^[0-9]{4}-[0-9]{2}$'),
    batch       int NOT NULL,
    file_name   text NOT NULL,
    counts      jsonb NOT NULL,
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (period, batch)
);
