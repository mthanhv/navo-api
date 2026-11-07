-- BR-05 (REQUIREMENTS 1.2): ordering or delivering beyond the contract needs Admin approval.
-- The requester asks with a reason; Admin approves (which confirms the document) or rejects.
CREATE TABLE quantity_override_requests (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind          text NOT NULL CHECK (kind IN ('purchase_order', 'delivery_order')),
    document_id   uuid NOT NULL,
    contract_id   uuid NOT NULL REFERENCES contracts (id),
    reason        text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 1000),
    delivered_on  date, -- delivery orders: the date the delivery is confirmed with
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    requested_by  uuid NOT NULL REFERENCES users (id),
    requested_at  timestamptz NOT NULL DEFAULT now(),
    decided_by    uuid REFERENCES users (id),
    decided_at    timestamptz,
    decision_note text CHECK (decision_note IS NULL OR length(decision_note) <= 1000),
    CHECK (kind <> 'delivery_order' OR delivered_on IS NOT NULL),
    CHECK ((status = 'pending') = (decided_at IS NULL))
);
-- One open request per document.
CREATE UNIQUE INDEX quantity_override_requests_open_key ON quantity_override_requests (kind, document_id) WHERE status = 'pending';
CREATE INDEX quantity_override_requests_status_idx ON quantity_override_requests (status, requested_at);
