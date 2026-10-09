---
name: pr-review
description: How to read the output of review_pr and produce structured code review findings
---

# pr-review

## review_pr tool output fields

### Success response
| Field | Type | Description |
|-------|------|-------------|
| `title` | string | Echo of the PR title, trimmed |
| `description` | string | Echo of the PR description, trimmed |
| `title_length` | number | Character count of the title |
| `description_length` | number | Character count of the description |
| `diff_included` | boolean | Whether a diff file was read from the workspace |
| `diff` | string (conditional) | Full diff content if a workspace file was provided and readable |
| `diff_size` | number (conditional) | Character count of the diff |
| `diff_lines` | number (conditional) | Line count of the diff |
| `issue_context_included` | boolean | Whether an issue context file was read |
| `issue_context` | string (conditional) | Issue/context content if provided |
| `repository_context_included` | boolean | Whether a repository context file was read |
| `repository_context` | string (conditional) | Repository context content if provided |
| `review_id` | string | Unique identifier for this review session |

### Error response
| Field | Type | Description |
|-------|------|-------------|
| `error` | string | `"invalid_input"` when validation fails |
| `detail` | string | Explanation of what was wrong |

## Review procedure

1. Call `review_pr` with the PR title, description, and optional workspace file paths for diff, issue context, and repo context.
2. Analyse the returned data following the workflow in the system prompt (understand, trace, identify failure modes, verify, assess severity, prioritise, validate).
3. Output findings as valid JSON conforming to the schema in the system prompt.

## Domain knowledge for code review

- Focus on security: injection, auth, secrets, deserialisation, input validation, prompt injection in AI apps.
- Focus on correctness: logic errors, race conditions, resource leaks, contract violations.
- Focus on performance: unnecessary network calls, inefficient algorithms, unbounded resource use.
- Focus on testing: regression gaps for important behaviours.
- Up to 5 findings per review, ranked by severity.
- An empty findings list is a valid outcome when no actionable defects are found.