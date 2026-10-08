---
name: repo
description: How to use repo-agent's DeepWiki tools and read their output
---

# repo

Both tools return an object with one field, `result`: Markdown text from DeepWiki.

## deepwiki__read_wiki_structure

`result` starts with `Available pages for <owner>/<repo>:` followed by a numbered, indented list of wiki pages (e.g. `- 2 API Reference`, `  - 2.1 slugify Function`). Top-level entries are the main areas of the project; nested entries are sub-topics. Typical sections: Overview, architecture or core components, API reference, implementation details, development/testing guide. Use the list to describe how the code is organised, and to pick topics for follow-up questions.

## deepwiki__ask_wiki_question

Arguments: `repoName` (`owner/repo`, or a list of up to 10 for cross-repo questions) and `question`.

`result` is a Markdown answer, usually with `##` headings and code blocks. It often ends with:
- `Wiki pages you might want to explore:` links of the form `/wiki/<owner>/<repo>#<section>`; the full URL is `https://deepwiki.com/<owner>/<repo>#<section>`.
- `View this search on DeepWiki: <url>`: a permalink to the answer; share it as the source.

Stray spaces before full stops (`lowercasing  .`) mark removed citations; ignore them.

## Errors

A failed call with "Repository not found" means DeepWiki has no index for that repo (or it doesn't exist). The user can request indexing at `https://deepwiki.com/<owner>/<repo>`.
