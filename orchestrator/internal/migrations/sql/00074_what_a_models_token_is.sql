-- How big one of a model's tokens is, measured from the model's own counts.
--
-- The window a model is given is in tokens, and nothing here can count tokens:
-- every vendor splits text its own way, and a model's successor can split the
-- same text into a third more of them. What can be counted is the characters of
-- a request, so a token budget has to be turned into characters, and a single
-- fixed rate for every model was wrong by 15 to 50 percent on the ones measured.
--
-- But every vendor says how many tokens a request was, in the answer to it. So
-- each call, the characters sent and the tokens the vendor reported are added
-- to these two totals, and a model's characters per token is one divided by the
-- other. The totals decay a little with every call, so what the model says now
-- counts for more than what it said a hundred calls ago, and a large request
-- counts for more than a small one, because it is the large ones that come near
-- the window. Zero is a model that has not reported yet.
--
-- And `model_calls.input_chars` is the characters sent on the call, beside the
-- tokens the vendor reported for it, so how well the estimate holds can be read
-- back afterwards rather than taken on trust.

-- +goose Up
ALTER TABLE `ai_models`
  ADD COLUMN `measured_chars` double NOT NULL DEFAULT 0,
  ADD COLUMN `measured_tokens` double NOT NULL DEFAULT 0;

ALTER TABLE `model_calls`
  ADD COLUMN `input_chars` bigint(20) NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE `model_calls`
  DROP COLUMN `input_chars`;

ALTER TABLE `ai_models`
  DROP COLUMN `measured_tokens`,
  DROP COLUMN `measured_chars`;
