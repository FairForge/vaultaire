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
git clone https://github.com/fairforge/vaultaire
cd vaultaire
make deps
make test
make build
Style Guidelines
Go Style

Run gofmt before committing
Follow Effective Go
Write clear comments explaining WHY, not WHAT

Commit Messages

Use present tense ("Add feature" not "Added feature")
Use imperative mood ("Move cursor to..." not "Moves cursor to...")
Limit first line to 72 characters
Reference issues and pull requests after the first line

Example:
feat: add S3 multipart upload support

- Implements resumable uploads for files >100MB
- Adds retry logic for failed parts
- Updates documentation

Fixes #123
Community

Discord: Join our server (coming soon)
Twitter: @storedge
Blog: blog.stored.ge

Recognition
Contributors will be recognized in our README.md and release notes. We value every contribution, no matter how small!
