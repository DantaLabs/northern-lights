# T6 attempt 1: acceptance waiver (owner-approved 2026-10-20)

The only failing acceptance command in checks/T6-a1.log is `go test ./... -race`, which hit Go's 10-minute default timeout in `internal/assurance`.

- The package is frozen (ADR 0002) and untouched by this run.
- On the unchanged integration branch (81d08da) the package alone takes about 620 s under `-race` on this machine.
- `go test ./... -race -count=1 -timeout 40m` on d3e819a passed every package (10m05s wall time).
- GitHub CI runs the same command and passed on every recent PR (#14, #15, #16).
- Both reviewers found T6's own code meets the spec; the spec reviewer's revise was only for this log line.

Tom approved continuing ("Go for the run"). No test, timeout or check was changed.
