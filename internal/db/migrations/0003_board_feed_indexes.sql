-- Board live-feed read models (SPEC-0013 "Board View — Live Incoming Lines"):
--   * RecentEvents orders all events newest-first and laterally joins each event's todo by
--     event_id — index both sides of that read.
--   * EventBuckets / BoardStats count events by received_at windows.
CREATE INDEX idx_events_received ON events (received_at DESC);
CREATE INDEX idx_todos_event ON todos (event_id) WHERE event_id IS NOT NULL;
