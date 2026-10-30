-- Apparel preview RFQ storage; Supabase PostgREST is the only application writer.
-- DB owns identity/time; anonymous clients have no row policies and cannot read inquiries.
create table if not exists public.apparel_rfq (
    id uuid primary key default gen_random_uuid(),
    created_at timestamptz not null default now(),
    name text not null check (char_length(name) between 1 and 120),
    company text check (char_length(company) <= 160),
    email text not null check (char_length(email) between 3 and 254),
    country text check (char_length(country) <= 80),
    product_slug text check (char_length(product_slug) <= 120),
    quantity_note text check (char_length(quantity_note) <= 200),
    message text check (char_length(message) <= 4000),
    source_page text check (char_length(source_page) <= 300),
    user_agent text check (char_length(user_agent) <= 300)
);
alter table public.apparel_rfq enable row level security;
-- Intentionally no policies. Only the private server-side secret may bypass RLS.
