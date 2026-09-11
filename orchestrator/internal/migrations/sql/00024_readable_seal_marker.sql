-- Make a stored secret's seal marker readable UTF-8.
--
-- A sealed secret in a custom tool's config was prefixed with a NUL byte, which
-- JSON stores as the ten-character escape sequence backslash-u-0000 then "enc:".
-- Rewrite that prefix to a printable "enc:". The ciphertext after it is
-- untouched, so nothing is re-encrypted; only the marker's spelling changes.
--
-- The seal marker carries no security by itself (sealing works from plaintext
-- and never trusts the marker), so this is purely a presentation change. base64
-- (the ciphertext encoding) never contains a colon, so the sequence appears only
-- as the marker and REPLACE cannot touch anything else.

-- +goose Up
UPDATE tools
SET config = REPLACE(config, '\\u0000enc:', 'enc:')
WHERE kind = 'custom' AND INSTR(config, '\\u0000enc:') > 0;

-- +goose Down
UPDATE tools
SET config = REPLACE(config, 'enc:', '\\u0000enc:')
WHERE kind = 'custom' AND INSTR(config, 'enc:') > 0;
