-- The personal edition's seeded owner gets a legible address.
--
-- It was `owner@personal.invalid`, chosen because RFC 2606 guarantees .invalid
-- never resolves. Nothing is ever sent to it and nothing looks it up, so what
-- the guarantee bought was a row that reads like a mistake to the one person who
-- will ever see it. `owner@localhost.fx` says whose it is and still satisfies
-- the address validation the ordinary user endpoint applies (which is why it
-- cannot simply be `owner@localhost`).
--
-- Why this is a migration and not a line in the seeding code. The seed looks the
-- owner up BY address: an installation seeded with the old one would not be
-- found under the new one, and the next start would quietly create a SECOND
-- owner beside the first. Renaming the row is the only thing that makes the
-- constant a change rather than a fork.
--
-- Scoped by the address alone, deliberately. A server deployment has no such row
-- and this updates none of its own: `owner@personal.invalid` is written by the
-- personal seed and by nothing else, and nobody signs up at a .invalid address
-- because no message could ever reach them there. It is NOT additionally scoped
-- by the name, which would be the tempting second guard and would break the one
-- case this exists for: the first thing setup asks is what to call you, so by
-- the time anybody upgrades, the name is theirs and no longer 'Owner'.

-- +goose Up
UPDATE `users`
   SET `email` = 'owner@localhost.fx'
 WHERE `email` = 'owner@personal.invalid';

-- +goose Down
UPDATE `users`
   SET `email` = 'owner@personal.invalid'
 WHERE `email` = 'owner@localhost.fx';
