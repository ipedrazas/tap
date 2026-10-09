---
name: hn-eval-agent
runner: runner-python
reference: hn-agent
egress: hacker-news.firebaseio.com:443
secrets:
effects: read
---
An agent that tells you what's on the Hacker News front page right now and can fetch the top comments of a story, using the official Hacker News API (Firebase, no key). Write the tools in Python. One tool lists the current top stories with title, URL, score, author and comment count; the other returns a story's top-level comments as plain text. Keep the number of requests per call bounded.
