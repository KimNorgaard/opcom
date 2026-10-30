# List the recipes.
default:
    @just --list

# Run everything CI runs.
check: fmt-check lint sec vuln test

# Run the tests with the race detector.
test:
    go test -race ./...

# Run golangci-lint with .golangci.yaml.
lint:
    golangci-lint run ./...

# Run gosec on its own, which also reports what golangci-lint's gosec skips.
sec:
    gosec -quiet ./...

# Check dependencies for known vulnerabilities that the code reaches.
vuln:
    govulncheck ./...

# Format the code with gofumpt and goimports.
fmt:
    golangci-lint fmt ./...

# Fail when the code isn't formatted.
fmt-check:
    golangci-lint fmt --diff ./...

# Modernize the code with go fix.
fix:
    go fix ./...

# Print total test coverage, and write per-line coverage to coverage.html.
cover:
    go test -coverprofile=coverage.out ./...
    go tool cover -html=coverage.out -o coverage.html
    go tool cover -func=coverage.out | tail -n 1

# Build ./opcom for this machine.
build:
    go build -o opcom .

# Build and run, passing args to opcom.
run *args: build
    ./opcom {{ args }}

# Tidy go.mod and go.sum.
tidy:
    go mod tidy

# Remove build output.
clean:
    rm -f opcom coverage.out coverage.html
