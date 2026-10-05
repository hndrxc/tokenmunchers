-- Backstop: private.rate_limits is only touched by the security-definer ingest
-- function (as its owner), so RLS with no policies changes nothing for it.
alter table private.rate_limits enable row level security;
