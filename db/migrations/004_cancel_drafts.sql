-- A draft purchase order, goods receipt or delivery order entered by mistake is cancelled,
-- never deleted, so its number stays used and the history explains the gap. An open
-- Admin request for that document is withdrawn with it. Draft contracts (DRAFT-nnnn,
-- no official number yet) are deleted instead.
ALTER TABLE goods_receipts DROP CONSTRAINT goods_receipts_status_check,
    ADD CONSTRAINT goods_receipts_status_check CHECK (status IN ('draft', 'confirmed', 'cancelled'));
ALTER TABLE delivery_orders DROP CONSTRAINT delivery_orders_status_check,
    ADD CONSTRAINT delivery_orders_status_check CHECK (status IN ('draft', 'confirmed', 'cancelled'));
ALTER TABLE quantity_override_requests DROP CONSTRAINT quantity_override_requests_status_check,
    ADD CONSTRAINT quantity_override_requests_status_check CHECK (status IN ('pending', 'approved', 'rejected', 'withdrawn'));
