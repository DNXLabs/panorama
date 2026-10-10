---
title: Reading request bodies
description: >-
  How the attribution extractors read a request in flight, and four findings
  worth knowing before changing one.
sidebar:
  order: 4
---

Attribution is read out of the request body as it passes, using regular
expressions and structural matching only. The body is then discarded. The
extraction rules are open source so that the claim "bodies are never stored"
can be checked.

They are also the part of the product most likely to rot. A coding tool changes
how it spells its environment block, and an extractor pinned to the old
spelling does not fail loudly: it stops attributing. Both known spellings of
Claude Code's block are supported for that reason.

This page is for somebody changing an extractor. For what attribution does, see
[How attribution works](/docs/start/attribution/).

## Four things an extractor has to get right

Each of these made an extractor that looked correct find nothing.

1. **The body is JSON, so the text inside it is escaped.** A newline is the two
   characters `\` and `n`. A pattern written against the text a person sees
   matches nothing. Captures end at the next quote or backslash instead, which
   reads one field out of escaped JSON without unescaping the document.
2. **A cheap literal anchor is usually in the middle of what the pattern
   matches**, such as `/pull/` inside a pull request URL. Starting the match
   window at the anchor puts the start of the pattern outside it, so windows
   begin a fixed distance *before* the anchor.
3. **`"text"` matches inside `"type":"text"`.** A content block is
   `{"type":"text","text":"…"}`. Searching for the characters `"text"` finds the
   value of `type` and returns the word `text`, identically for every request
   from every conversation, which merges the whole deployment into one session.
   Keys are matched as keys: name, colon, then a string.
4. **The opening message has to be cut at its own end.** Taking a fixed number
   of bytes spills into the messages after it. Those grow every turn, so the
   session fingerprint changes on each request and every turn becomes its own
   session.

## One extractor per API shape

The gateway serves three request shapes, the Anthropic Messages API, OpenAI
Chat Completions and OpenAI Responses, and each has its own extractor. A new
API surface needs an extractor to match, or its traffic lands in the
unattributed bucket, which is the one number this product promises to be honest
about.

## Stamped values are untrusted input

Everything extracted, including a client's own stamp headers, is
attacker-influenced. Values are trimmed, rejected outright if longer than 200
bytes or if they contain anything outside printable ASCII, and never truncated
into something that looks valid. No `x-panorama-` header is forwarded to a
model provider.
