-- Files on quotations (owner decision 2026-11-20): the customer's request and our offer.
ALTER TABLE attachment_links DROP CONSTRAINT attachment_links_owner_type_check,
    ADD CONSTRAINT attachment_links_owner_type_check CHECK (owner_type IN ('customer', 'contract', 'purchase_order', 'goods_receipt',
        'delivery_order', 'sales_invoice', 'purchase_invoice', 'receipt', 'payment', 'note', 'quotation'));
