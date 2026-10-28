# Repository checks; CI runs the same gates.
check: dashes secrets

dashes:
    bash tools/check-no-dashes/check-no-dashes --tree

secrets:
    gitleaks git --no-banner --redact

# Every Go invocation unsets GOFLAGS so a caller's setting cannot change the build.
build:
    env -u GOFLAGS go build -o bin/rice-agent -ldflags "-X github.com/moabualruz/rice-agent/internal/commands.Version=$(git describe --tags --always --dirty)" ./cmd/rice-agent
    env -u GOFLAGS go build -o bin/agent-dry -ldflags "-X github.com/moabualruz/rice-agent/internal/commands.Version=$(git describe --tags --always --dirty)" ./cmd/agent-dry
    bin/rice-agent --help >/dev/null
    bin/agent-dry --help >/dev/null

vet:
    env -u GOFLAGS go vet ./...

worktree-doctor:
    bash scripts/worktree-doctor.sh

test:
    env -u GOFLAGS go test ./...
    bash scripts/deploy.test.sh
    bash scripts/worktree-doctor.test.sh

# Release TAG to this machine and every hosts.<name>.install host (see scripts/deploy.sh --help).
deploy TAG *ARGS:
    bash scripts/deploy.sh {{TAG}} {{ARGS}}

# Admission gates G1 to G6 on every tool under tools/.
tools: build
    bin/rice-agent check --all

# Everything CI runs.
ci: worktree-doctor vet test build tools dashes secrets
