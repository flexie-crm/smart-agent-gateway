-- The certificate of a machine that issued its own.
--
-- A machine normally gets its certificate from us: it joins, sends a request
-- over a key it keeps, and we sign it, so every machine in the fleet chains to
-- one authority and there is nothing to store per machine.
--
-- That needs the machine to be able to call us, which is false for the whole
-- personal edition and for any gateway inside a network the machine is outside
-- of. There the exchange happens by hand and in both directions: we hand over
-- our authority so the machine will accept only us, and the machine generates
-- its own key and a certificate over it, which is pasted back here. Its key
-- never leaves it, and we trust that one certificate and nothing else, so it is
-- kept rather than derived.
--
-- Empty for every machine that joined, which is how the two are told apart when
-- dialling: a pinned certificate is used INSTEAD of the authority, never as
-- well, because a machine that signed its own chains to nothing.

-- +goose Up
ALTER TABLE `inference_nodes`
  ADD COLUMN `pinned_cert` text DEFAULT NULL AFTER `cert_expires_at`;

-- +goose Down
ALTER TABLE `inference_nodes` DROP COLUMN `pinned_cert`;
