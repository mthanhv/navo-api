-- Corrections retain the original document and a linked, immutable reason record.
ALTER TABLE goods_receipts DROP CONSTRAINT goods_receipts_status_check;
ALTER TABLE goods_receipts ADD CONSTRAINT goods_receipts_status_check CHECK (status IN ('draft','confirmed','cancelled','reversed'));
ALTER TABLE delivery_orders DROP CONSTRAINT delivery_orders_status_check;
ALTER TABLE delivery_orders ADD CONSTRAINT delivery_orders_status_check CHECK (status IN ('draft','confirmed','cancelled','reversed'));
ALTER TABLE receipts DROP CONSTRAINT receipts_status_check;
ALTER TABLE receipts ADD CONSTRAINT receipts_status_check CHECK (status IN ('posted','cancelled','reversed'));
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check CHECK (status IN ('posted','cancelled','reversed'));

CREATE TABLE document_reversals (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 document_type text NOT NULL CHECK (document_type IN ('goods_receipt','delivery_order','receipt','payment')),
 document_id uuid NOT NULL,
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 created_by uuid NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(document_type, document_id)
);
CREATE FUNCTION forbid_history_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'navo:history_immutable'; END $$;
CREATE TRIGGER document_reversals_immutable BEFORE UPDATE OR DELETE ON document_reversals FOR EACH ROW EXECUTE FUNCTION forbid_history_change();

CREATE TABLE payment_allocations (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 payment_id uuid NOT NULL REFERENCES payments(id),
 purchase_invoice_id uuid NOT NULL REFERENCES purchase_invoices(id),
 amount numeric(24,2) NOT NULL CHECK (amount > 0),
 created_by uuid NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 reversed_at timestamptz,
 reversed_by uuid REFERENCES users(id),
 reverse_reason text,
 CHECK ((reversed_at IS NULL) = (reversed_by IS NULL) AND (reversed_at IS NULL) = (reverse_reason IS NULL))
);
INSERT INTO payment_allocations(payment_id,purchase_invoice_id,amount,created_by,created_at)
 SELECT id,purchase_invoice_id,amount,created_by,created_at FROM payments WHERE purchase_invoice_id IS NOT NULL AND status = 'posted';
CREATE INDEX payment_allocations_invoice_idx ON payment_allocations(purchase_invoice_id) WHERE reversed_at IS NULL;
CREATE INDEX payment_allocations_payment_idx ON payment_allocations(payment_id) WHERE reversed_at IS NULL;

-- Terms and trigger events have a recoverable, versioned history.
CREATE TABLE payment_term_history (
 id bigserial PRIMARY KEY, milestone_id uuid NOT NULL REFERENCES payment_milestones(id),
 version bigint NOT NULL, snapshot jsonb NOT NULL, changed_by uuid NOT NULL REFERENCES users(id),
 changed_at timestamptz NOT NULL DEFAULT now(), UNIQUE(milestone_id,version)
);
