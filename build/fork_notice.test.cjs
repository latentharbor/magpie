const assert = require("node:assert/strict");
const test = require("node:test");
const report = require("./fork_notice.cjs");

const context = { repo: { owner: "latentharbor", repo: "magpie" }, serverUrl: "https://github.com", runId: 42, payload: {} };
function fixture(issues = []) {
  const writes = [];
  return {
    writes,
    github: {
      paginate: async () => issues,
      rest: { issues: { listForRepo() {}, create: async (v) => writes.push(v), update: async (v) => writes.push(v) } },
    },
  };
}
test("failure opens a generic maintenance notice and recovery closes it", async () => {
  const f = fixture();
  await report(f.github, context, "upstream", true);
  assert.equal(f.writes.length, 1);
  assert.equal(f.writes[0].state, "open");
  assert.match(f.writes[0].body, /actions\/runs\/42/);
  const recovered = fixture([{ number: 7, state: "open", body: f.writes[0].body }]);
  await report(recovered.github, context, "upstream", false);
  assert.equal(recovered.writes.length, 1, "recovery must close the existing maintenance issue");
  assert.equal(recovered.writes[0].issue_number, 7);
  assert.equal(recovered.writes[0].state, "closed");
});
test("repeated failure updates one notice and reopens it after recurrence", async () => {
  for (const state of ["open", "closed"]) {
    const f = fixture([{ number: 7, state, body: "<!-- distribution-notice:upstream -->" }]);
    await report(f.github, context, "upstream", true);
    assert.equal(f.writes.length, 1);
    assert.equal(f.writes[0].issue_number, 7);
    assert.equal(f.writes[0].state, "open");
  }
});
test("successful runs do not create notices or touch already closed ones", async () => {
  for (const issues of [[], [{ number: 7, state: "closed", body: "<!-- distribution-notice:upstream -->" }]]) {
    const f = fixture(issues);
    await report(f.github, context, "upstream", false);
    assert.equal(f.writes.length, 0);
  }
});
test("a release notice links the completed run and never edits a pull request", async () => {
  const f = fixture([{ number: 1, pull_request: {}, body: "<!-- distribution-notice:release -->" }]);
  await report(f.github, { ...context, payload: { workflow_run: { id: 99 } } }, "release", true);
  assert.equal(f.writes.length, 1);
  assert.equal(f.writes[0].issue_number, undefined);
  assert.match(f.writes[0].body, /actions\/runs\/99/);
});
test("failed main checks have their own notice and recover independently", async () => {
  const f = fixture([{ number: 4, state: "open", body: "<!-- distribution-notice:release -->" }]);
  const completed = { ...context, payload: { workflow_run: { id: 101 } } };
  await report(f.github, completed, "test", true);
  assert.equal(f.writes.length, 1);
  assert.equal(f.writes[0].issue_number, undefined, "a failed Test never overwrites the release notice");
  assert.equal(f.writes[0].title, "Distribution checks need attention");
  assert.match(f.writes[0].body, /actions\/runs\/101/);
  const recovered = fixture([{ number: 8, state: "open", body: f.writes[0].body }]);
  await report(recovered.github, completed, "test", false);
  assert.equal(recovered.writes[0].issue_number, 8);
  assert.equal(recovered.writes[0].state, "closed");
});
