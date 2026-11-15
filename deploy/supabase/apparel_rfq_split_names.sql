-- Additive RFQ expansion: legacy name stays populated for existing readers.
-- Apply before deploying the split-name form/function; no changes to row access.
alter table public.apparel_rfq
    add column if not exists first_name text check (char_length(first_name) between 1 and 50),
    add column if not exists last_name text check (char_length(last_name) between 1 and 50);
