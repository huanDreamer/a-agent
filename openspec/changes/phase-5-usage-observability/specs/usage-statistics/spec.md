# Capability: usage-statistics

## Purpose

Attribute LLM consumption to the end user and the model that produced it, and
expose aggregates that answer "who used what, when, and what did it cost".

## Scope

- `internal/store` — `user_id` attribution and the aggregation queries.
- `internal/pricing` — turning token counts into money.
- The admin API under `/api/usage/*`.

## Requirements (MUST)

1. **Attribution** — every recorded LLM call MUST carry a `user_id` when the
   caller knows the end user, and an empty `user_id` MUST mean "unattributed"
   rather than failing the write.
2. **Backfill safety** — adding attribution MUST be a non-destructive migration;
   rows written before it MUST remain readable with an empty `user_id`.
3. **Totals** — the store MUST expose call count, prompt/completion/total
   tokens, total duration, distinct users and distinct sessions for a window.
4. **Distinct users** — `Users` MUST NOT count the empty `user_id`, so an
   unattributed row cannot inflate the user count.
5. **Grouping** — the store MUST support grouping by user, by model, by provider
   and by provider+model, each ordered by total tokens descending with a stable
   tiebreak, and each capped at a sane limit.
6. **Trend** — the store MUST bucket usage by day for a caller-supplied number
   of days, returning days that have data in ascending order. Bucketing MUST
   work for rows written through the normal Go write path, whose timestamps are
   not in a format SQLite's `date()` parses natively.
7. **Window filtering** — every query and every aggregation MUST apply the same
   `since`/`until`/`user_id`/`provider`/`model` filtering, so a listing and an
   aggregate over the same window cannot disagree.
8. **Cost** — cost MUST be computed from a configurable price table keyed by
   `provider/model`, `model` or `provider` (most specific match first), and the
   computation MUST tolerate missing, negative and non-finite rates by yielding
   zero rather than NaN.
9. **Honest totals** — a cost total MUST be summed over the distinct
   (provider, model) pairs in the window, and MUST report whether anything
   matched a price entry, so "no price configured" is never presented as a
   confident zero.
10. **Empty results** — a query with no matching rows MUST return an empty
    result, not an error.

## Non-goals

- Budget enforcement or alerting on spend.
- Per-request billing/rating beyond token counts.
- Historic price changes (a price table edit re-prices the past).

## Key interfaces

- `store.UsageWindow`, `store.UsageTotals`, `store.UsageGroupRow`,
  `store.UsageDayRow`, `store.ProviderModelGroup`
- `store.QueryUsageTotals`, `QueryUsageByUser`, `QueryUsageByModel`,
  `QueryUsageByProvider`, `QueryUsageByProviderModel`, `QueryUsageByDay`,
  `QueryUsageRecent`
- `pricing.Table`, `pricing.Rate`, `pricing.Cost`
- `GET /api/usage/summary|by-model|by-provider|by-user|by-day|recent`
