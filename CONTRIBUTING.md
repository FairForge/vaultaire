# Contributing to Vaultaire

First off, thank you for considering contributing to Vaultaire! It's people like you that make Vaultaire such a great tool.

## Code of Conduct

This project and everyone participating in it is governed by the [Vaultaire Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code.

## How Can I Contribute?

### Reporting Bugs

Before creating bug reports, please check existing issues as you might find that you don't need to create one. When you are creating a bug report, please include as many details as possible:

* Use a clear and descriptive title
* Describe the exact steps to reproduce the problem
* Provide specific examples to demonstrate the steps
* Describe the behavior you observed and what behavior you expected
* Include logs and error messages

### Suggesting Enhancements

* Use a clear and descriptive title
* Provide a step-by-step description of the suggested enhancement
* Provide specific examples to demonstrate the use case
* Explain why this enhancement would be useful

### Pull Requests

* Fill in the required template
* Do not include issue numbers in the PR title
* Follow the Go style guidelines
* Include thoughtfully-worded, comprehensive commit messages
* Add tests for new functionality
* Update documentation as needed

## Development Setup

```bash
git clone https://github.com/FairForge/vaultaire
cd vaultaire
go mod download
make test-db        # creates + migrates the local vaultaire_test database (needed before any DB-backed test)
make test
make build
pre-commit install && pre-commit install --hook-type pre-push  # fmt + lint on commit, go test ./... -short on push
```

## Style Guidelines

### Go Style

* Run `gofmt` before committing (`make fmt`)
* Follow [Effective Go](https://go.dev/doc/effective_go)
* Write clear comments explaining WHY, not WHAT

### Commit Messages

* Format: `type(scope): description [Phase NNN]` or `[Review RNN]` — type is one of feat/fix/refactor/test/docs
* Use present tense ("Add feature" not "Added feature")
* Use imperative mood ("Move cursor to..." not "Moves cursor to...")
* Limit first line to 72 characters
* Reference issues and pull requests after the first line

Example:

```
feat(api): add S3 multipart upload support [Phase 5.10]

- Implements resumable uploads for files >100MB
- Adds retry logic for failed parts
- Updates documentation

Fixes #123
```

## Community

* Questions and support: support@stored.ge
* Bugs and feature requests: [GitHub issues](https://github.com/FairForge/vaultaire/issues)

## Recognition

Contributors will be recognized in our README.md and release notes. We value every contribution, no matter how small!
