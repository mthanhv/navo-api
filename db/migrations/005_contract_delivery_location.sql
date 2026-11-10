-- Where the customer receives the goods (shown on the contract page, printed on deliveries).
ALTER TABLE contracts ADD COLUMN delivery_location text CHECK (delivery_location IS NULL OR length(delivery_location) <= 300);
