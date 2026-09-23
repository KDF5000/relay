ALTER TABLE relay_nodes
    ADD COLUMN IF NOT EXISTS desired_capacity INTEGER;

UPDATE relay_nodes
SET desired_capacity = capacity
WHERE desired_capacity IS NULL;

ALTER TABLE relay_nodes
    ALTER COLUMN desired_capacity SET NOT NULL;

ALTER TABLE relay_nodes
    ADD CONSTRAINT relay_nodes_desired_capacity_check
    CHECK (desired_capacity > 0 AND desired_capacity <= 32);
