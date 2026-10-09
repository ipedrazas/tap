// review_pr: argv --title <string> --description <string>
//            [--diff-path <path>] [--issue-path <path>] [--repo-path <path>]
// Validates PR review data, reads workspace files if provided,
// and returns structured PR info for model analysis.
import { parseArgs } from "node:util";
import { readFile } from "node:fs/promises";
import { access } from "node:fs/promises";

const { values } = parseArgs({
  options: {
    title: { type: "string" },
    description: { type: "string" },
    "diff-path": { type: "string", default: "" },
    "issue-path": { type: "string", default: "" },
    "repo-path": { type: "string", default: "" },
  },
  strict: true,
});

const title = values.title!.trim();
const description = values.description!.trim();

if (!title) {
  process.stdout.write(JSON.stringify({ error: "invalid_input", detail: "title must not be empty" }));
  process.exit(0);
}
if (!description) {
  process.stdout.write(JSON.stringify({ error: "invalid_input", detail: "description must not be empty" }));
  process.exit(0);
}

const result: Record<string, unknown> = {
  title,
  description,
  title_length: title.length,
  description_length: description.length,
  diff_included: false,
  issue_context_included: false,
  repository_context_included: false,
};

// Try to read workspace files
const diffPath = values["diff-path"];
if (diffPath) {
  try {
    await access(diffPath);
    const diffContent = await readFile(diffPath, "utf-8");
    result.diff = diffContent;
    result.diff_included = true;
    result.diff_size = diffContent.length;
    result.diff_lines = diffContent.split("\n").length;
  } catch {
    result.diff_path = diffPath;
    result.diff_error = "file not found or inaccessible";
  }
}

const issuePath = values["issue-path"];
if (issuePath) {
  try {
    await access(issuePath);
    const issueContent = await readFile(issuePath, "utf-8");
    result.issue_context = issueContent;
    result.issue_context_included = true;
    result.issue_context_size = issueContent.length;
  } catch {
    result.issue_context_path = issuePath;
    result.issue_context_error = "file not found or inaccessible";
  }
}

const repoPath = values["repo-path"];
if (repoPath) {
  try {
    await access(repoPath);
    const repoContent = await readFile(repoPath, "utf-8");
    result.repository_context = repoContent;
    result.repository_context_included = true;
    result.repository_context_size = repoContent.length;
  } catch {
    result.repository_context_path = repoPath;
    result.repository_context_error = "file not found or inaccessible";
  }
}

result.review_id = `pr-${Date.now()}`;
process.stdout.write(JSON.stringify(result));