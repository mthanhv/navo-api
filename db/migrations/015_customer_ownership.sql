-- Customer contacts are read by everyone but changed and emailed only by the customer's
-- owner, owners of its open contracts, and people who see the whole company (owner decision
-- 2026-11-11). Give existing customers the owner of their most relevant contract.
UPDATE organizations o SET owner_id = (
    SELECT c.owner_id FROM contracts c WHERE c.customer_id = o.id
    ORDER BY (c.status IN ('draft', 'active')) DESC, c.created_at DESC, c.id LIMIT 1)
WHERE o.is_customer AND o.owner_id IS NULL AND EXISTS (SELECT 1 FROM contracts c WHERE c.customer_id = o.id);
CREATE INDEX organizations_owner_idx ON organizations (owner_id) WHERE owner_id IS NOT NULL;
