---
name: hn
description: What hn-agent's front_page and story_comments outputs mean and how Hacker News items work
---

# hn

Data comes from the official Hacker News API (hacker-news.firebaseio.com/v0). Every story and comment is an "item" with a numeric ID.

## front_page
`{count, stories: [...]}`, in front-page order. Each story:
- `rank`: position on the front page (1 = top). Dead or deleted items are dropped, so ranks can skip a number.
- `id`: item ID; pass it to `story_comments`.
- `title`: story title. "Ask HN:", "Show HN:", "Tell HN:" prefixes mark text posts by HN users.
- `url`: the linked article. Text posts have no article, so `url` equals `hn_url`.
- `hn_url`: the discussion page on news.ycombinator.com.
- `points`: upvote score. `comments`: total comment count, including replies.
- `by`: submitter's username. `type`: usually `story`, sometimes `job` (YC job ads, no comments) or `poll`.
- `time`: submission time, Unix seconds (UTC).

## story_comments
`{story, top_comments: [...]}`.
- `story`: same fields as above plus `text` (body of Ask/Tell HN posts, plain text; empty for link posts).
- `top_comments`: top-level comments in HN's ranking order (best first), deleted and dead ones skipped. Each has `id`, `by`, `text` (HTML converted to plain text, links shown as their URL, cut at 1500 characters with "..."), `replies` (number of direct replies) and `time` (Unix seconds).
- Only top-level comments are returned, not reply threads.

## Errors
`{"error": ...}` with exit 0: `not_found` (no item with that ID), `not_a_story` (the ID is a comment; `type` says what it is), `upstream` (`status` is the HTTP code), `unreachable` (network failure).
