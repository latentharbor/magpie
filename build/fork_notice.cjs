// Workflow notices contain only public run links, never logs or local details.
module.exports = async function report(github, context, kind, failed) {
  const titles = {
    upstream: "Upstream synchronization needs attention",
    release: "Distribution release needs attention",
  };
  if (!Object.hasOwn(titles, kind)) throw new Error("Unknown notice kind");
  const { owner, repo } = context.repo;
  const marker = `<!-- distribution-notice:${kind} -->`;
  const issues = await github.paginate(github.rest.issues.listForRepo, {
    owner, repo, state: "all", creator: "github-actions[bot]", per_page: 100,
  });
  const issue = issues.find((item) => !item.pull_request && item.body?.includes(marker));
  if (!failed && (!issue || issue.state === "closed")) return;
  const runId = context.payload.workflow_run?.id || context.runId;
  const link = `${context.serverUrl}/${owner}/${repo}/actions/runs/${runId}`;
  const body = `${marker}\n\n${failed
    ? "The automated maintenance run did not complete. Review the linked run before retrying. A failed run does not authorize publishing an incomplete update."
    : "The automated maintenance run completed successfully."}\n\nRun: ${link}\n`;
  const fields = { owner, repo, title: titles[kind], body, state: failed ? "open" : "closed" };
  if (issue) {
    await github.rest.issues.update({ ...fields, issue_number: issue.number });
  } else {
    await github.rest.issues.create(fields);
  }
};
