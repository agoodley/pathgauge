# Contributing to PathGauge

Thanks for your interest. The project is pre-alpha. Read [docs/PLAN.md](docs/PLAN.md) first: it describes the architecture, the test catalogue and the roadmap that contributions should fit.

## Setup

- Go: the version in `go.mod`. Newer toolchains download it automatically.
- Docker: for the package install tests and the lab smoke test.
- [GoReleaser](https://goreleaser.com) v2: for `make snapshot`.
- [golangci-lint](https://golangci-lint.run) v2: optional, because `make lint` falls back to its Docker image.

Run `make help` to list the tasks.

## Expectations for changes

- **Measurement changes need evidence.** Show results from the netns lab (`lab/netns.sh`) with a known impairment, and say how they compare with an established tool (iperf3, irtt, flent, mtr) where one exists.
- **Packaging changes** must pass `make snapshot && make install-test`.
- **Tests:** add unit tests for parsing, maths and rules. Prefer injecting inputs (see `hostinfo.Sources`) over reading the real system.
- **Safety:** nothing may leave shaping applied, accept unauthenticated test traffic, or reflect more bytes than it receives (docs/PLAN.md §15).
- **Style:** `gofmt`, plus the linters in `.golangci.yml`. User-facing text uses UK spelling.

## Commits

- Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:` …). The changelog is generated from them.
- Sign off every commit (`git commit -s`), certifying the [Developer Certificate of Origin](https://developercertificate.org/). No CLA is needed.
- Contributions are accepted under the project's [Apache License 2.0](LICENSE). New dependencies must use a compatible licence: Apache-2.0, MIT, BSD or ISC. `make licences` checks this.

## Reporting bugs

Use the issue templates, and include `sudo pathgauge doctor` output from **both** ends. Report security problems privately (see [SECURITY.md](SECURITY.md)).
