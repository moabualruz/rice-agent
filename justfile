# Repository checks; CI runs the same gates.
check: dashes secrets

dashes:
    bash scripts/check-no-dashes.sh --tree

secrets:
    gitleaks git --no-banner --redact
