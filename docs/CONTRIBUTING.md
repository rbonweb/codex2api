# Codex2API Contributing Guide

Thank you for your interest in the Codex2API project! This guide explains how to contribute.

## Table of contents

- [Development environment setup](#development-environment-setup)
- [Code style](#code-style)
- [Commit conventions](#commit-conventions)
- [Pull request process](#pull-request-process)
- [Testing requirements](#testing-requirements)
- [Documentation updates](#documentation-updates)

---

## Development environment setup

### Prerequisites

- Go 1.26.10 or later
- Node.js 22.12 or later
- Docker and Docker Compose
- Git

### Local development setup

1. **Fork and clone the repository**

```bash
git clone https://github.com/YOUR_USERNAME/codex2api.git
cd codex2api
git remote add upstream https://github.com/rbonweb/codex2api.git
```

2. **Install backend dependencies**

```bash
go mod download
go mod verify
```

3. **Install frontend dependencies**

```bash
cd frontend
npm ci
cd ..
```

4. **Configure the environment**

```bash
cp .env.example .env
# Edit .env to configure a local database (or use SQLite mode)
```

5. **Build the frontend**

```bash
cd frontend && npm run build && cd ..
```

6. **Start the service**

```bash
# Option 1: run locally
go run .

# Option 2: Docker (published image with bundled PostgreSQL and Redis)
docker compose -f docker-compose.full.yml up -d
```

---

## Code style

### Go code style

We follow standard Go conventions:

1. **Format code with gofmt**

```bash
gofmt -w .
```

2. **Lint with golint**

```bash
go install golang.org/x/lint/golint@latest
golint ./...
```

3. **Run static analysis with go vet**

```bash
go vet ./...
```

4. **Code style requirements**

- Use CamelCase naming
- Exported functions and types must have comments
- Handle errors first and do not ignore them
- Use meaningful variable names

```go
// Good example
// GetAccountByID gets account information by ID
func GetAccountByID(ctx context.Context, id int64) (*Account, error) {
    if id <= 0 {
        return nil, fmt.Errorf("invalid account id: %d", id)
    }
    // ...
}

// Bad example
func getaccount(id int64) *Account {
    // ignoring the error
    db.Query("SELECT * FROM accounts WHERE id = ?", id)
    // ...
}
```

### Frontend code style

1. **Run frontend validation**

```bash
cd frontend
npm test
npm run typecheck
npm run build
```

When changing the SVG export of the intelligence check, also run the real-browser regression (install Chromium on the first run):

```bash
npx playwright install chromium
npm run test:svg
```

You can additionally verify a local pelican sample with `QUALITY_TEST_SVG_SAMPLE=/absolute/path/pelican.html npm run test:svg`; the sample file is not written to the repository.

When changing the intelligence check page, the account check status bar or the history dialog, run `npm run test:intelligence`.
This browser regression uses a local mock API and does not call real models. Screenshots are saved to the system temp directory by default; set `INTELLIGENCE_TEST_ARTIFACT_DIR` to choose another output directory.

2. **Code style**

- Use 2-space indentation
- Use single quotes
- Trailing semicolons are optional
- Maximum line length is 100 characters

### Project structure

```
codex2api/
├── admin/          # Admin panel API handlers
├── auth/           # Account pool and scheduling logic
├── cache/          # Cache abstraction layer
├── config/         # Configuration loading
├── database/       # Database access layer
├── proxy/          # Proxy forwarding and translation
├── frontend/       # React frontend
│   ├── src/
│   │   ├── components/  # UI components
│   │   ├── pages/       # Page components
│   │   └── locales/     # Internationalization
├── docs/           # Documentation
└── main.go         # Entry point
```

---

## Commit conventions

We use the [Conventional Commits](https://www.conventionalcommits.org/) convention:

### Commit format

```
<type>(<scope>): <subject>

<body>

<footer>
```

### Types

| Type | Description | Example |
|------|------|------|
| `feat` | New feature | `feat(scheduler): add fast scheduler` |
| `fix` | Bug fix | `fix(auth): fix account cooldown calculation error` |
| `docs` | Documentation update | `docs(api): update API documentation` |
| `style` | Code formatting | `style: format code` |
| `refactor` | Refactoring | `refactor(proxy): optimize request executor` |
| `test` | Test-related | `test(auth): add scheduler unit tests` |
| `chore` | Build/tooling | `chore: update dependencies` |

### Example

```
feat(api): add batch account import

- Support TXT and JSON import formats
- Add SSE progress push
- Deduplicate and validate automatically

Fixes #123
```

---

## Pull request process

### 1. Create a branch

```bash
# Create a feature branch from main
git checkout main
git pull upstream main
git checkout -b feature/your-feature-name
```

Branch naming:
- `feature/` - new feature
- `fix/` - bug fix
- `docs/` - documentation update
- `refactor/` - code refactoring

### 2. Develop and commit

```bash
# Commit your code
git add .
git commit -m "feat: add some feature"

# Push to your fork
git push origin feature/your-feature-name
```

### 3. Create a PR

1. Go to your fork's page
2. Click "Compare & pull request"
3. Fill in the PR description:

```markdown
## Description
Briefly describe the purpose of this PR

## Change type
- [ ] Bug fix
- [ ] New feature
- [ ] Documentation update
- [ ] Code refactoring

## Testing
- [ ] Added unit tests
- [ ] Manual testing passed
- [ ] All existing tests pass

## Related issues
Fixes #123
```

### 4. Code review

- Maintainers will review the code
- Make changes based on the feedback
- Make sure the CI checks pass

### 5. Merge

- The PR is merged once it is approved
- You can choose squash merge to keep the commit history clean

---

## Testing requirements

### Unit tests

```bash
# Run all tests
go test ./...

# Run tests for a specific package
go test ./auth/...

# With coverage
go test -cover ./...

# Generate a coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Testing conventions

1. **Test file naming**: `xxx_test.go`
2. **Test function naming**: `TestFunctionName`
3. **Use table-driven tests**

```go
func TestCalculateScore(t *testing.T) {
    tests := []struct {
        name     string
        account  *Account
        expected float64
    }{
        {
            name:     "healthy account",
            account:  &Account{HealthTier: Healthy},
            expected: 100,
        },
        {
            name:     "banned account",
            account:  &Account{HealthTier: Banned},
            expected: 0,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := CalculateScore(tt.account)
            if result != tt.expected {
                t.Errorf("got %v, want %v", result, tt.expected)
            }
        })
    }
}
```

### Integration tests

The repository has no separate integration suite. To exercise the whole gateway, start it with `docker compose -f docker-compose.full.yml up -d` (bundled PostgreSQL and Redis) and call it as described in [API.md](API.md).

---

## Documentation updates

### Documentation locations

| Document | Path | Description |
|------|------|------|
| API documentation | `docs/API.md` | API endpoint reference |
| Deployment documentation | `docs/DEPLOYMENT.md` | Deployment guide |
| Configuration documentation | `docs/CONFIGURATION.md` | Configuration reference |
| Architecture documentation | `docs/ARCHITECTURE.md` | Architecture design |
| Troubleshooting | `docs/TROUBLESHOOTING.md` | Problem diagnosis |
| Contributing guide | `docs/CONTRIBUTING.md` | This file |

### Documentation conventions

1. Use Markdown format
2. Add a table of contents for navigation
3. Specify the language of code blocks
4. Use tables to present parameters

### API documentation updates

When adding a new API, update:

1. `docs/API.md` - endpoint reference
2. `admin/handler.go` or `proxy/handler.go` - code comments
3. README.md, if necessary

---

## Release process

### Versioning

Use [Semantic Versioning](https://semver.org/):

- `MAJOR.MINOR.PATCH`
- MAJOR: incompatible API changes
- MINOR: backward-compatible feature additions
- PATCH: backward-compatible bug fixes

### Release steps

Releases are automatic: every push to `main` that changes more than `docs/` or Markdown runs `.github/workflows/fork-release.yml`, which tags the next patch version, publishes the public image `ghcr.io/rbonweb/codex2api` (tags `X.Y.Z`, `X.Y`, `vX.Y.Z`, `latest`) and creates a GitHub release with the binaries.

1. Add the release notes to `CHANGELOG.md`
2. Merge to `main`
3. For a minor or major bump, add a higher `## vX.Y.Z` heading to `CHANGELOG.md` first: the next patch is counted from the higher of the newest tag and the newest changelog version

---

## Getting help

- **GitHub Issues**: [Report an issue](https://github.com/rbonweb/codex2api/issues)
- **Discussions**: [Join the discussion](https://github.com/rbonweb/codex2api/discussions)

---

## Code of conduct

- Respect all participants
- Welcome newcomers and answer questions patiently
- Focus on constructive technical discussion
- Follow open-source community norms

Thank you for your contribution!
