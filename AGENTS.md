# Working rules

It is the contract, not a suggestion.

If `doc/agent-notes.md` exists, read it before starting: it holds what earlier
agents learned that neither these rules nor the docs state. When a task teaches
you something the next agent would otherwise rediscover, add it there in the
same PR, creating the file with the first note.

## Comments

- A comment block is **at most 5 lines**. If the explanation needs more, the
  code is wrong or the explanation belongs in a doc.
- One line is the default. Comment *why*, never *what*: the code says what.
- No narrative, no history, no restating the diff, no essays on design
  philosophy. Git log and the docs hold those.
- No dates, commit or PR references, or plan labels in comments.
- No comment on a self-evident function.
- Package doc blocks: 5 lines. Files do not get their own prologue.

## Files

- Do not create a file for one or a few helpers. Put them next to what uses
  them, or, if shared, in the package's main or type file.
- A new file needs a new concept, not a new function.
- Prefer editing an existing file over adding one.
- When you edit a file, bring its comments up to these rules.

## Scope

- Implement what was asked. Do not add tables, indirection, telemetry, tests
  or abstraction that nothing asked for. A task that explicitly invites
  additions gets the obvious ones, each named in the PR body; ask before any
  that is not obvious.
- One mechanism per problem. Two mechanisms doing one job is a bug or a
  refactor opportunity.
- Delete before adding. If a change is net-positive lines for a fix, justify
  it.
- Reverting an existing API to "improve" it is not a fix. Leave working code
  alone.
- Per-kind data lives in one table indexed by its kind; a new kind adds a row,
  never a switch or a parallel field.
- The project is at v0: a change may break a flag, a configuration key or the
  HTTP contract, and takes the next minor version; never hold or narrow a fix
  for backward compatibility.

## Tests

- One test per rule, named for the rule. No test that restates another.
- Test comments follow the 5-line limit.
- Do not pin lists that a human has to hand-maintain unless the pin prevents a
  real regression.
- Collapse tests: when a broader test covers a narrower one, delete the
  narrower one or do not add it. No test for simple behavior that is unlikely
  to fail or is already exercised elsewhere.

## Docs

- Condense a doc when you touch it; append only for a new concept or scope.
- Docs are read in terminals, where a Markdown table renders as its source.
  Lay tabular content out as a nested list; a matrix of figures goes in a
  fenced code block with its columns aligned by spaces.
- The repository describes the architecture, never a machine: docs, scripts,
  examples and defaults use placeholders for addresses, names, endpoints and
  credentials. What a deployment needs reaches it when it deploys, by template
  arguments, rc.conf or the environment, never by a commit.

## Commits and PRs

- A commit message is its subject alone, at most 72 characters: no body, no
  trailer. A PR's title is a subject too.
- Commits are versions: a subject starts with the version it makes
  (`v0.1.1 gate: ...`), the next minor for a break or a new feature and the
  next patch otherwise, and the maintainer tags it. PRs are squash-merged, so a
  PR's title and every commit's subject start with the version.
- A change to documents alone makes no version and no tag: its subject starts
  `docs:` and its PR body with `doc only`.
- A PR body is a summary, never documentation: what changed, half a line an
  item at most, with the why only where the diff does not show it.
- Verification is the two words `gates passed`, never the commands or their
  output. A figure a rule asks for is given as the figure alone, before and
  after.
- A commit message and a PR's title, body and comments carry no URL and
  nothing of where or by whom the work was done: no session or account id, no
  person's name or email, no agent, model or tool name, no "generated with"
  line, no attribution trailer. A tool may add such a line itself: read a PR
  back after opening or editing it and edit the line out.
- Open the PR at the first push, its title final and its body as far as it
  is, saying what is still owed.
- A change made for performance reports its measurement, before and after, in
  the unit the user feels; a projection is not a result.
- A change that needs a dependency's new version names it in `go.mod` in the
  same PR, as if tagged; until the tag exists, the gates run against the
  dependency's checkout through a `go.work` outside the tree, never a
  `replace` in `go.mod`.
- One task, one branch, one PR. Divide the work into commits on that branch;
  do not split it across stacked PRs or multiple branches.
- Work that continues after its PR merged starts again from the latest main;
  never stack new commits on merged history.
- Finish and push one commit before starting the next.
- Once a PR is merged or closed unmerged, delete its branch on GitHub and
  locally if the session can; one that cannot says so once and leaves it.

## Agents

- Spin off few agents, and only for work one context cannot hold.
- Agents run in parallel only on scopes that cannot conflict: each brief names
  the files its agent owns, disjoint from every other agent's. Shared docs stay
  with the parent, which commits each agent's work in turn on the one branch.
- An agent writes what it finds to a file as it goes, one section per step, so
  an interrupt costs a step and not the task; the parent reads those files and
  resumes from them.

## Gates

`go build ./...`, `gofmt -l` on changed files, and `go test` on the packages
the change touches.

- The production target is FreeBSD: a change also cross-builds it,
  `GOOS=freebsd GOARCH=amd64 go build ./...`.
- A mechanical edit across files is checked by a count before and after, with
  a search that reads its pattern as the file holds it; a search that matches
  nothing proves nothing.
- Do not run `-race` tests unless investigating a known or suspected race.
