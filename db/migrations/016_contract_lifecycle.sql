-- Contract lifecycle after activation (REQUIREMENTS N-04, N-06, BR-06, AC-05/06/07/10):
-- amendments, acceptance documents, customer and supplier returns, close/cancel.
-- Every document here is recorded already confirmed; a mistake is reversed with a reason,
-- never edited or deleted.

ALTER TABLE contracts
    ADD COLUMN ac_seq int NOT NULL DEFAULT 0,
    ADD COLUMN cr_seq int NOT NULL DEFAULT 0,
    ADD COLUMN sr_seq int NOT NULL DEFAULT 0,
    ADD COLUMN am_seq int NOT NULL DEFAULT 0;

CREATE FUNCTION forbid_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'navo:history_immutable'; END $$;

-- Close, cancel and reopen, each with who and why.
CREATE TABLE contract_status_changes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id uuid NOT NULL REFERENCES contracts (id),
    from_status text NOT NULL,
    to_status   text NOT NULL CHECK (to_status IN ('active', 'closed', 'cancelled')),
    reason      text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX contract_status_changes_contract_idx ON contract_status_changes (contract_id, created_at);
CREATE TRIGGER contract_status_changes_immutable BEFORE UPDATE OR DELETE ON contract_status_changes FOR EACH ROW EXECUTE FUNCTION forbid_history_change();

-- Amendments (phụ lục): signed changes to quantities, prices and due dates, and new lines.
CREATE TABLE contract_amendments (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id uuid NOT NULL REFERENCES contracts (id),
    number      text NOT NULL UNIQUE CHECK (number ~ '^AM-[0-9]{2}-[0-9]{4}-[0-9]{2,}$'),
    signed_on   date NOT NULL,
    reference   text CHECK (reference IS NULL OR length(reference) <= 100),
    reason      text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX contract_amendments_contract_idx ON contract_amendments (contract_id, created_at);
CREATE TABLE contract_amendment_lines (
    amendment_id     uuid NOT NULL REFERENCES contract_amendments (id),
    contract_line_id uuid NOT NULL REFERENCES contract_lines (id),
    change           text NOT NULL CHECK (change IN ('added', 'changed')),
    before           jsonb,
    after            jsonb NOT NULL,
    PRIMARY KEY (amendment_id, contract_line_id)
);
CREATE TRIGGER contract_amendments_immutable BEFORE UPDATE OR DELETE ON contract_amendments FOR EACH ROW EXECUTE FUNCTION forbid_history_change();
CREATE TRIGGER contract_amendment_lines_immutable BEFORE UPDATE OR DELETE ON contract_amendment_lines FOR EACH ROW EXECUTE FUNCTION forbid_history_change();

-- Acceptance (nghiệm thu): quantities the customer accepted, never more than delivered net.
CREATE TABLE acceptances (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id uuid NOT NULL REFERENCES contracts (id),
    number      text NOT NULL UNIQUE CHECK (number ~ '^AC-[0-9]{2}-[0-9]{4}-[0-9]{2,}$'),
    accepted_on date NOT NULL,
    evidence    text NOT NULL CHECK (length(btrim(evidence)) BETWEEN 1 AND 500),
    status      text NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'reversed')),
    version     bigint NOT NULL DEFAULT 1,
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contract_id, id)
);
CREATE TABLE acceptance_lines (
    acceptance_id    uuid NOT NULL,
    contract_id      uuid NOT NULL,
    contract_line_id uuid NOT NULL,
    quantity         numeric(20, 6) NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (acceptance_id, contract_line_id),
    FOREIGN KEY (contract_id, acceptance_id) REFERENCES acceptances (contract_id, id),
    FOREIGN KEY (contract_id, contract_line_id) REFERENCES contract_lines (contract_id, id)
);
CREATE INDEX acceptance_lines_line_idx ON acceptance_lines (contract_line_id);

-- Customer returns: goods the customer sent back; they reduce delivered and return to stock.
CREATE TABLE customer_returns (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id       uuid NOT NULL REFERENCES contracts (id),
    number            text NOT NULL UNIQUE CHECK (number ~ '^CR-[0-9]{2}-[0-9]{4}-[0-9]{2,}$'),
    returned_on       date NOT NULL,
    reason            text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
    status            text NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'reversed')),
    version           bigint NOT NULL DEFAULT 1,
    created_by        uuid NOT NULL REFERENCES users (id),
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contract_id, id)
);
CREATE TABLE customer_return_lines (
    customer_return_id uuid NOT NULL,
    contract_id        uuid NOT NULL,
    contract_line_id   uuid NOT NULL,
    quantity           numeric(20, 6) NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (customer_return_id, contract_line_id),
    FOREIGN KEY (contract_id, customer_return_id) REFERENCES customer_returns (contract_id, id),
    FOREIGN KEY (contract_id, contract_line_id) REFERENCES contract_lines (contract_id, id)
);
CREATE INDEX customer_return_lines_line_idx ON customer_return_lines (contract_line_id);

-- Supplier returns: accepted goods sent back to the supplier; they reduce received.
CREATE TABLE supplier_returns (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id       uuid NOT NULL REFERENCES contracts (id),
    purchase_order_id uuid NOT NULL REFERENCES purchase_orders (id),
    number            text NOT NULL UNIQUE CHECK (number ~ '^SR-[0-9]{2}-[0-9]{4}-[0-9]{2,}$'),
    returned_on       date NOT NULL,
    reason            text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
    status            text NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'reversed')),
    version           bigint NOT NULL DEFAULT 1,
    created_by        uuid NOT NULL REFERENCES users (id),
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE supplier_return_lines (
    supplier_return_id     uuid NOT NULL REFERENCES supplier_returns (id),
    purchase_order_line_id uuid NOT NULL REFERENCES purchase_order_lines (id),
    quantity               numeric(20, 6) NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (supplier_return_id, purchase_order_line_id)
);
CREATE INDEX supplier_return_lines_pol_idx ON supplier_return_lines (purchase_order_line_id);

CREATE TRIGGER acceptances_no_delete BEFORE DELETE ON acceptances FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER acceptance_lines_no_change BEFORE UPDATE OR DELETE ON acceptance_lines FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER customer_returns_no_delete BEFORE DELETE ON customer_returns FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER customer_return_lines_no_change BEFORE UPDATE OR DELETE ON customer_return_lines FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER supplier_returns_no_delete BEFORE DELETE ON supplier_returns FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER supplier_return_lines_no_change BEFORE UPDATE OR DELETE ON supplier_return_lines FOR EACH ROW EXECUTE FUNCTION forbid_delete();

ALTER TABLE document_reversals DROP CONSTRAINT document_reversals_document_type_check,
    ADD CONSTRAINT document_reversals_document_type_check CHECK (document_type IN
        ('goods_receipt', 'delivery_order', 'receipt', 'payment', 'acceptance', 'customer_return', 'supplier_return'));

-- Net quantities, defined once. Every check and report uses these.
CREATE FUNCTION po_line_received(pol uuid) RETURNS numeric LANGUAGE sql STABLE AS $$
    SELECT coalesce((SELECT sum(grl.accepted_quantity) FROM goods_receipt_lines grl JOIN goods_receipts gr ON gr.id = grl.goods_receipt_id
                     WHERE grl.purchase_order_line_id = pol AND gr.status = 'confirmed'), 0)
         - coalesce((SELECT sum(srl.quantity) FROM supplier_return_lines srl JOIN supplier_returns sr ON sr.id = srl.supplier_return_id
                     WHERE srl.purchase_order_line_id = pol AND sr.status = 'confirmed'), 0)
$$;
CREATE FUNCTION line_received(cl uuid) RETURNS numeric LANGUAGE sql STABLE AS $$
    SELECT coalesce((SELECT sum(grl.accepted_quantity) FROM goods_receipt_lines grl JOIN goods_receipts gr ON gr.id = grl.goods_receipt_id
                     JOIN purchase_order_lines pol ON pol.id = grl.purchase_order_line_id WHERE pol.contract_line_id = cl AND gr.status = 'confirmed'), 0)
         - coalesce((SELECT sum(srl.quantity) FROM supplier_return_lines srl JOIN supplier_returns sr ON sr.id = srl.supplier_return_id
                     JOIN purchase_order_lines pol ON pol.id = srl.purchase_order_line_id WHERE pol.contract_line_id = cl AND sr.status = 'confirmed'), 0)
$$;
CREATE FUNCTION line_delivered(cl uuid) RETURNS numeric LANGUAGE sql STABLE AS $$
    SELECT coalesce((SELECT sum(dol.quantity) FROM delivery_order_lines dol JOIN delivery_orders d ON d.id = dol.delivery_order_id
                     WHERE dol.contract_line_id = cl AND d.status = 'confirmed'), 0)
         - coalesce((SELECT sum(crl.quantity) FROM customer_return_lines crl JOIN customer_returns r ON r.id = crl.customer_return_id
                     WHERE crl.contract_line_id = cl AND r.status = 'confirmed'), 0)
$$;
CREATE FUNCTION line_accepted(cl uuid) RETURNS numeric LANGUAGE sql STABLE AS $$
    SELECT coalesce((SELECT sum(al.quantity) FROM acceptance_lines al JOIN acceptances a ON a.id = al.acceptance_id
                     WHERE al.contract_line_id = cl AND a.status = 'confirmed'), 0)
$$;

-- An acceptance-triggered payment term records which acceptance completed the contract;
-- NULL with a date means the date was entered by hand before acceptance documents existed.
ALTER TABLE payment_milestones ADD COLUMN acceptance_id uuid REFERENCES acceptances (id);
