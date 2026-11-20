-- Owner decision 2026-11-20: the leader role is removed and a delivery role is added.
-- People who were leaders become admins, so they keep seeing the whole company.
INSERT INTO user_roles (user_id, role)
SELECT user_id, 'admin' FROM user_roles WHERE role = 'leader'
ON CONFLICT DO NOTHING;
-- Their sessions pick up the new permissions on the next request.
UPDATE users SET version = version + 1, updated_at = now()
WHERE id IN (SELECT user_id FROM user_roles WHERE role = 'leader');
DELETE FROM user_roles WHERE role = 'leader';

ALTER TABLE user_roles DROP CONSTRAINT user_roles_role_check;
ALTER TABLE user_roles ADD CONSTRAINT user_roles_role_check CHECK (role IN ('sales', 'delivery', 'accounting', 'admin'));
