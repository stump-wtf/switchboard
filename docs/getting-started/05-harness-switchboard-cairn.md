---
title: Harness, Switchboard, and Cairn
---

# How Harness, Switchboard, and Cairn fit together

Harness runs your agents, Switchboard hands them work, and Cairn holds what they make.
Each is useful alone; together they close a loop from an event to a result a person can read.
The full walkthrough lives on the Harness docs, so this page only says what each piece is and is not.

| | Is | Is not |
|---|---|---|
| **Switchboard todo** | A **dispatch lease**: work from a verified event that one agent claims under a lease and carries to `complete` or `fail` | A second tracker: the issue or pull request stays the record of the work, and the todo points an agent at it |
| **Cairn artifact** | **Evidence**: the review, diff, log or trace an agent made, at a stable link you cite from your tracker or a todo's `result` | Where work is tracked or where the next step is decided |
| **Harness** | A **supervisor**: it starts, restarts, schedules and attaches to agent processes, and keeps their logs and run history | A task manager: it does not decide where work comes from or where results go |

> **Read the canonical page:**
> [How Harness, Switchboard and Cairn fit together](https://stump-wtf.github.io/harness/guides/harness-switchboard-cairn)
> on the Harness docs walks one loop end to end and links the setup guide for each step.

Setting up the Switchboard side starts at [Concepts](/getting-started/concepts), then
[your first endpoint](/getting-started/first-endpoint).
