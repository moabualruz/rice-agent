# Repository checks; CI runs the same gates.
check: dashes secrets

dashes:
    bash scripts/check-no-dashes.sh --tree

secrets:
    gitleaks git --no-banner --redact

# Every Go invocation unsets GOFLAGS so a caller's setting cannot change the build.
build:
    env -u GOFLAGS go build -o bin/agent-dry -ldflags "-X github.com/moabualruz/agent-dry/internal/commands.Version=$(git describe --tags --always --dirty)" ./cmd/agent-dry

vet:
    env -u GOFLAGS go vet ./...

test:
    env -u GOFLAGS go test ./...

# Admission gates G1 to G6 on every tool under tools/.
tools: build
    bin/agent-dry check --all

# Everything CI runs.
ci: vet test build tools dashes secrets
