---
name: pr-review-eval-agent
runner: runner-node
egress: api.github.com:443
secrets: 1
effects: read
---
An agent that reviews a GitHub pull request. People give it a PR URL (https://github.com/<owner>/<repo>/pull/<number>) or owner, repo and number in the chat. The agent fetches the PR itself from the GitHub REST API: title, description, the list of changed files and the unified diff. It works unauthenticated on public repositories and uses an optional GitHub token (read-only) for private ones and higher rate limits. It reviews the change for security, correctness, reliability and material performance problems, and answers in the chat with at most five findings, each with the file and line, the concrete failure scenario and a suggested fix, or says it found nothing worth raising. It never posts to GitHub: read-only. Large diffs must be truncated sensibly rather than failing.
