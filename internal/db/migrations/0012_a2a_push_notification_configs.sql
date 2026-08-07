-- 0012_a2a_push_notification_configs — A2A PushNotificationConfig storage.
-- A caller-registered webhook that switchboard POSTs task updates to over authenticated HTTP, so an
-- external A2A client does not have to hold an open connection or poll GetTask. This migration lands
-- only the storage; the CRUD handlers, the SSRF-guarded delivery path, and the sequence/retry
-- machinery arrive in the follow-up push stories. task_id references the todo (todos.id is text) so a
-- config is deleted when its task is, and no config can outlive the work it notifies for.
-- Governing: ADR-0021 (A2A task-delegation transport),
-- SPEC-0019 REQ "PushNotificationConfig CRUD",
-- SPEC-0019 REQ "Webhook Target Validation (SSRF Guard)".
CREATE TABLE push_notification_configs (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),      -- server-assigned config id
    task_id        text NOT NULL REFERENCES todos(id) ON DELETE CASCADE, -- the todo this notifies for
    url            text NOT NULL,                                   -- caller-supplied webhook target (SSRF-guarded)
    token          text,                                            -- optional caller token echoed on delivery for correlation
    authentication jsonb,                                           -- optional auth descriptor switchboard applies when calling the webhook
    created_at     timestamptz NOT NULL DEFAULT now()
);
-- Hot path: list/dispatch a task's active push configs on every committed transition.
CREATE INDEX idx_push_notification_configs_task ON push_notification_configs (task_id, created_at DESC);
