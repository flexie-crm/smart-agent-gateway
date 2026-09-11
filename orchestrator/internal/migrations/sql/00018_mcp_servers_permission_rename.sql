-- The outbound connections were briefly permissioned as integrations:*.
-- The product calls them MCP servers, and the keys follow the product.

-- +goose Up
UPDATE role_permissions SET permission = 'mcp-servers:view' WHERE permission = 'integrations:view';
UPDATE role_permissions SET permission = 'mcp-servers:create' WHERE permission = 'integrations:create';
UPDATE role_permissions SET permission = 'mcp-servers:edit' WHERE permission = 'integrations:edit';
UPDATE role_permissions SET permission = 'mcp-servers:delete' WHERE permission = 'integrations:delete';

-- +goose Down
UPDATE role_permissions SET permission = 'integrations:view' WHERE permission = 'mcp-servers:view';
UPDATE role_permissions SET permission = 'integrations:create' WHERE permission = 'mcp-servers:create';
UPDATE role_permissions SET permission = 'integrations:edit' WHERE permission = 'mcp-servers:edit';
UPDATE role_permissions SET permission = 'integrations:delete' WHERE permission = 'mcp-servers:delete';
