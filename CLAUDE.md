@AGENTS.md

<!-- dev-pipeline:start -->
## Development pipeline

This repository uses the dev-pipeline skill. Commands are in .pipeline/config.json.
Read CONTEXT.md for vocabulary and CONSTRAINTS.md for the quality bar.
For a feature, multi-file bugfix, refactor or migration, suggest "pipeline spec <goal>" before starting.
Never push to the default branch, force push or merge pull requests. New cloud branches start with claude/; existing run branches retain their names.
Never weaken a test or check to get a green result.
<!-- dev-pipeline:end -->

## Claude Code lanes

The demo lane specs in `docs/specs/demo-*.md` are written in the pipeline spec format. Once Tom writes `Approved: yes` in a spec, run it with "ship docs/specs/demo-<lane>.md". Lane pull requests target `claude/demo-integration`, not `main`.
