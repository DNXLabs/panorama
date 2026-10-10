---
title: The hot path
description: >-
  What happens on a request, what may never happen on it, and the consequence
  that is stated rather than hidden.
sidebar:
  order: 3
---

**Streaming is never buffered.** Nothing on the request path may buffer a
stream, block, or add meaningful latency. The target is sub-10ms added latency to
time-to-first-token.

Inline work on the request path is **regex and structural matching only**.
Everything else runs asynchronously against already-extracted metadata.

## Nothing on the policy path touches the database

Routes, budgets, team membership and spend all live in an in-memory snapshot,
reloaded on an interval and incremented as requests complete.

A `SELECT` between a keystroke and the first token is exactly what this gateway
promises not to do.

:::note[The consequence is stated rather than hidden]
A request is priced only after it finishes, so enforcement is always against
spend as of the last completed request.
:::

## One snapshot, not two

Routes load into the policy engine's **existing** snapshot. Two snapshots
refreshed independently would spend part of every cycle disagreeing about which
models exist, and a request could be admitted by one and have no target in the
other.

There is no resolver beside it: `Evaluate` returns the provider-native model to
invoke, so the data plane has nothing left to look up.

A provider that is off or failed to build is in the snapshot as a state with an
error, which is how a route on it becomes a 502 naming the deployment rather
than a nil dereference.

## Authentication is one indexed lookup

The credential prefix is what makes the five kinds tellable apart before any
query runs, which is what keeps authentication on the request path to one
indexed lookup in one table. It is also what lets a credential be refused at a
door it has no business at, rather than looked up and rejected later.

## A stream ends at its end marker

`[DONE]`, `message_stop`, or the terminal `response.*` event. **Not** at the
connection closing.

A body that ends before its end marker is an upstream error, never a clean
finish with a smaller bill.
