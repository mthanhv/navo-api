-- Quotations (owner decision 2026-11-20): a price offer to a customer that, once won,
-- becomes a draft contract. QT-{yy}-{nnnn}; changing a sent quotation raises its revision.
CREATE TABLE quotations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number      text NOT NULL UNIQUE,
    customer_id uuid NOT NULL REFERENCES organizations (id),
    contact_id  uuid REFERENCES contacts (id) ON DELETE SET NULL,
    title       text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 300),
    currency    text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    valid_until date NOT NULL,
    terms       text CHECK (length(terms) <= 4000),
    status      text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'sent', 'won', 'lost')),
    revision    int  NOT NULL DEFAULT 1 CHECK (revision >= 1),
    sent_on     date,
    lost_reason text CHECK (length(lost_reason) <= 500),
    contract_id uuid UNIQUE REFERENCES contracts (id) ON DELETE SET NULL,
    owner_id    uuid NOT NULL REFERENCES users (id),
    created_by  uuid NOT NULL REFERENCES users (id),
    version     bigint NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'lost') = (lost_reason IS NOT NULL))
);
CREATE INDEX quotations_owner_idx ON quotations (owner_id, status);
CREATE INDEX quotations_customer_idx ON quotations (customer_id);

CREATE TABLE quotation_lines (
    quotation_id uuid NOT NULL REFERENCES quotations (id) ON DELETE CASCADE,
    line_no      int  NOT NULL CHECK (line_no >= 1),
    product_id   uuid NOT NULL REFERENCES products (id),
    name         text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 500),
    unit         text NOT NULL CHECK (length(btrim(unit)) BETWEEN 1 AND 40),
    quantity     numeric(20, 6) NOT NULL CHECK (quantity > 0),
    unit_price   numeric(24, 6) NOT NULL CHECK (unit_price >= 0),
    tax_rate     numeric(9, 6) CHECK (tax_rate IS NULL OR (tax_rate >= 0 AND tax_rate <= 1)),
    lead_time    text CHECK (length(lead_time) <= 100),
    PRIMARY KEY (quotation_id, line_no)
);

ALTER TABLE document_series DROP CONSTRAINT document_series_doc_type_check;
ALTER TABLE document_series ADD CONSTRAINT document_series_doc_type_check CHECK (doc_type IN ('SC', 'PI', 'RV', 'PV', 'QT', 'DRAFT'));
