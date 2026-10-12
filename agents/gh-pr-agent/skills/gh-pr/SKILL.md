---
name: gh-pr
description: How to use gh-pr-agent's five GitHub API tools and read their output
---

# gh-pr

All five tools talk only to `api.github.com`, use the optional read-only `GITHUB_TOKEN` when it is set, and never modify anything. Expected failures (not found, no access, rate limited) come back as JSON with an `error` field, not as crashes.

## Common inputs

- `owner`, `repo`: GitHub owner and repository name. For fork PRs, reads of **new** code may need the fork's owner/repo from `get_pull_request`'s `head_repo_full_name`.
- `number`: PR number.
- `path`: repository-relative filename (no leading `/` or `-`, no `.`/`..` segments; dotfiles are accepted, e.g. `.github/workflows/ci.yml`).
- `ref`: a 40-hex commit sha or a branch name.

## get_pull_request

One request. Fields: `title`, `body` (capped at 4000 chars with `body_truncated`), `author`, `state` (`open`/`closed`), `draft`, `merged`, `base_ref`/`base_sha` (what the PR merges into), `head_ref`/`head_sha` (what the PR introduces), `head_repo_full_name` (null when the head repo is gone; otherwise the fork to read head-sha content from), `additions`, `deletions`, `changed_files`, `labels`. Call it first.

## get_pull_request_diff / get_pull_request_diff_page

The PR's changed files, 100 per page. `get_pull_request_diff` fetches pages 1-3 and `files_total` (from PR metadata); `get_pull_request_diff_page(page)` continues at `page` with up to three more pages. Both return:

- `files[]`: each entry is `{filename, status (modified|added|removed|renamed), previous_filename (renames only), additions, deletions, first_line, last_line, patch_truncated, skipped, reason, lines}`.
- `lines[]`: the patch annotated line by line. `n` is the line number **in the new file**; `marker` is `""` (added), `" "` (context), `"-"` (removed, and then `n` is the **old**-file line number), `"@@"` (hunk header, `n: null`). Hunk-header arithmetic is done by the tool: cite `lines[].n` directly, do not recompute.
- `skipped: true` with a `reason` (`generated`, `vendored`, `binary_or_too_large`) means the file is listed but has no reviewable patch.
- `first_line`/`last_line`: the min/max new-file line numbers covered (0 when the file only removes lines).
- `patch_truncated: true`: the patch was cut at a hunk boundary by the budget (~8000 chars per file, ~60000 per call); use `get_pull_request_file_patch` for the full patch.
- `files_returned` (entries listed), `files_total` (changed files across the whole PR; null if metadata failed — diff tool only), `next_page` (page to continue from, or `null`), `omitted` (files dropped by the call budget, reason `over_budget`).

## get_pull_request_file_patch

The full hunk-annotated patch of exactly one file (pages the list until the filename is found, up to 15 requests). Output is `{"file": {...}}` with the same per-file shape as above; the file gets the whole ~60000-char budget, so a file truncated by the diff tools usually comes back whole. Errors: `not_found` (not a changed file), `skipped` (no patch to show).

## get_file

A numbered window of one file at a ref: `{"lines": [{"n", "text"}, ...], "total_lines": N, "window_start", "window_end"}`. At most 400 lines are returned; requesting `start_line`/`end_line` beyond the file clamps to the file. `ref` = head sha for new code, base sha for old. Errors: `directory`, `binary`, `not_found`. A `not_found` for a fork PR's head sha means the sha lives on the head repository — call again with the fork's owner/repo from `get_pull_request.head_repo_full_name`.

## Errors

`not_found` (404: wrong PR, missing file/ref, or no access to a private repo), `forbidden` (403 without rate-limit header), `rate_limited` (403 with quota exhausted; `reset` is the Unix time to retry after, from `x-ratelimit-reset`), `unauthorized` (401), `upstream` (other status), `upstream_unreachable` (network). Anonymous callers get ~60 requests/hour per IP, so keep tool calls low.