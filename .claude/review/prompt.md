# Prompt

Review the text agents read as instructions in this repository: `AGENTS.md`
(`CLAUDE.md` is a symlink to it), the review criteria under
`.claude/review/*.md` and `.claude/review/review.yaml`, whose `verify_notes`
every verifier reads. The service itself sends no prompts to a model, so Go
code, `README.md`, test fixtures and code comments are out of scope.

## Two kinds of finding

1. Drift: the text names a path, identifier, command, limit or rule the
   repository does not have, or contradicts the code. Evidence is the
   `git ls-files`, `git grep` or command output that shows the mismatch; no
   web source is needed.
2. Prompting quality: every other finding carries a verbatim quote from a
   primary source fetched in this run, with URL and retrieval date, and
   one sentence on why it applies. Provider documentation (Anthropic,
   OpenAI) is tier one; peer-reviewed papers tier two; practitioner
   writing tier three, supporting only. A finding resting on tier three
   alone is a nit at most. If a fetch fails, search the exact title; if
   still unreachable, drop the finding.

## What must agree

- Commands under AGENTS.md "Verification" and "Development" run as written
  inside the flake dev shell it names (`nix develop`), and match the `tools`
  in `.claude/review/review.yaml`.
- Every critical path in AGENTS.md, `review.yaml` included, falls under a
  tag the `security` dimension in `review.yaml` selects on (`runtime` for
  `cmd/llama-swap-voice`, `build` for `review.yaml`); the prompt files
  AGENTS.md lists match the `llm` tag.
- The rules in AGENTS.md "Rules" match `.golangci.yaml` (gocritic all
  checks, gosec G115, govet shadow, errcheck blank and type assertions,
  misspell UK).
- Every file and identifier a criteria file names exists, and each
  criteria file stays near eighty lines.
- The confirmation list and the routine list in AGENTS.md "What needs
  confirmation" do not overlap or contradict `verify_notes`.

## Priorities

1. Text that produces wrong agent behaviour: a command that fails, a path
   that does not exist, two rules that conflict, a confirmation-gated
   action described as routine.
2. Text that prevents self-correction: a rule with no reason where the
   reason decides edge cases; a verification step with no stated success
   condition.
3. Text that degrades reasoning: a prohibition where a positive rule
   exists; "never" on something that is not an invariant; generic advice
   the reviewer or agent prompt already gives.
4. Structure: the most important rules not first; one subject split
   across sections; a label used without saying what it refers to.

Falsify beyond the standard checks: is the choice deliberate (Pantheon log
`llama-swap-voice-first-agents-md-from-the-pantheon-instruct` records why
AGENTS.md is shaped as it is, the "Working" blocks included); does a more authoritative source disagree; would the change add
bloat without changing behaviour.

## Check for what is absent

- A new rule states its reason.
- A new command has been run and its success output is known.
- A new critical path appears in both AGENTS.md and `review.yaml`.

## Finding fields

- Kind: Drift / Quality
- Current text: verbatim
- Recommended text: a drop-in rewrite
- Evidence: the command output (Drift), or the source quote, URL and
  retrieval date with one sentence on why it applies (Quality)
