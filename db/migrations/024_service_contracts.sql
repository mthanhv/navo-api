-- Maintenance & repair contracts (REQUIREMENTS 5.13, owner approval 2026-11-20). The same
-- contract, with a kind; service lines are fulfilled by service visits (SV-…) instead of
-- purchasing and delivery. A finished visit counts as delivered, so progress, payment terms,
-- acceptance and closing reuse the existing rules.
ALTER TABLE contracts
    ADD COLUMN kind text NOT NULL DEFAULT 'supply' CHECK (kind IN ('supply', 'service')),
    ADD COLUMN service_from date,
    ADD COLUMN service_to date,
    ADD COLUMN sv_seq int NOT NULL DEFAULT 0,
    ADD CONSTRAINT contracts_service_period CHECK (service_from IS NULL OR service_to IS NULL OR service_to >= service_from);

ALTER TABLE contract_lines ADD COLUMN line_type text NOT NULL DEFAULT 'goods' CHECK (line_type IN ('goods', 'service'));
ALTER TABLE quotation_lines ADD COLUMN line_type text NOT NULL DEFAULT 'goods' CHECK (line_type IN ('goods', 'service'));

-- The customer's equipment this contract maintains (a short list, not an asset register; N-14 stays open).
CREATE TABLE contract_equipment (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id uuid NOT NULL REFERENCES contracts (id) ON DELETE CASCADE,
    position    int NOT NULL DEFAULT 0,
    name        text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    model       text CHECK (model IS NULL OR length(model) <= 120),
    serial      text CHECK (serial IS NULL OR length(serial) <= 120),
    location    text CHECK (location IS NULL OR length(location) <= 200),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX contract_equipment_contract_idx ON contract_equipment (contract_id, position);

CREATE TABLE service_visits (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number         text NOT NULL UNIQUE,
    contract_id    uuid NOT NULL REFERENCES contracts (id),
    -- scheduled: periodic maintenance; repair: the customer reported a problem.
    kind           text NOT NULL CHECK (kind IN ('scheduled', 'repair')),
    status         text NOT NULL DEFAULT 'planned' CHECK (status IN ('planned', 'done', 'cancelled', 'reversed')),
    planned_on     date,
    assignee_id    uuid REFERENCES users (id),
    equipment_id   uuid REFERENCES contract_equipment (id) ON DELETE SET NULL,
    description    text CHECK (description IS NULL OR length(description) <= 2000),
    urgency        text NOT NULL DEFAULT 'normal' CHECK (urgency IN ('normal', 'urgent')),
    done_on        date,
    work_done      text CHECK (work_done IS NULL OR length(work_done) <= 4000),
    findings       text CHECK (findings IS NULL OR length(findings) <= 4000),
    done_by        uuid REFERENCES users (id),
    done_at        timestamptz,
    cancel_reason  text CHECK (cancel_reason IS NULL OR length(cancel_reason) <= 500),
    created_by     uuid NOT NULL REFERENCES users (id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    version        bigint NOT NULL DEFAULT 1,
    CHECK (status NOT IN ('done', 'reversed') OR (done_on IS NOT NULL AND work_done IS NOT NULL)),
    CHECK (kind = 'repair' OR planned_on IS NOT NULL OR status <> 'planned')
);
CREATE INDEX service_visits_contract_idx ON service_visits (contract_id, status);
CREATE INDEX service_visits_planned_idx ON service_visits (planned_on) WHERE status = 'planned';

-- Which service lines a visit fulfils (usually 1 visit of one line). A repair outside the
-- contracted visits has no lines and counts toward nothing.
CREATE TABLE service_visit_lines (
    visit_id         uuid NOT NULL REFERENCES service_visits (id) ON DELETE CASCADE,
    contract_line_id uuid NOT NULL REFERENCES contract_lines (id),
    quantity         numeric(20, 6) NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (visit_id, contract_line_id)
);
CREATE INDEX service_visit_lines_line_idx ON service_visit_lines (contract_line_id);

-- Net delivered now also counts finished service visits (goods lines are unchanged: they have none).
CREATE OR REPLACE FUNCTION line_delivered(cl uuid) RETURNS numeric LANGUAGE sql STABLE AS $$
    SELECT coalesce((SELECT sum(dol.quantity) FROM delivery_order_lines dol JOIN delivery_orders d ON d.id = dol.delivery_order_id
                     WHERE dol.contract_line_id = cl AND d.status = 'confirmed'), 0)
         - coalesce((SELECT sum(crl.quantity) FROM customer_return_lines crl JOIN customer_returns r ON r.id = crl.customer_return_id
                     WHERE crl.contract_line_id = cl AND r.status = 'confirmed'), 0)
         + coalesce((SELECT sum(vl.quantity) FROM service_visit_lines vl JOIN service_visits v ON v.id = vl.visit_id
                     WHERE vl.contract_line_id = cl AND v.status = 'done'), 0)
$$;

-- Allow 'service_visit' in two type lists. The lists are extended as they stand (not
-- rewritten), so values added by other migrations are kept.
DO $$
DECLARE def text;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO def FROM pg_constraint WHERE conname = 'document_reversals_document_type_check';
    EXECUTE 'ALTER TABLE document_reversals DROP CONSTRAINT document_reversals_document_type_check, ADD CONSTRAINT document_reversals_document_type_check '
        || replace(def, 'ARRAY[', 'ARRAY[''service_visit''::text, ');
    -- Files on a visit: the signed service report and photos.
    SELECT pg_get_constraintdef(oid) INTO def FROM pg_constraint WHERE conname = 'attachment_links_owner_type_check';
    EXECUTE 'ALTER TABLE attachment_links DROP CONSTRAINT attachment_links_owner_type_check, ADD CONSTRAINT attachment_links_owner_type_check '
        || replace(def, 'ARRAY[', 'ARRAY[''service_visit''::text, ');
END $$;
