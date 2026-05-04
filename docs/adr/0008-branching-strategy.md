# 8. Branching Strategy and Conventional Commits

Date: 2026-05-05

## Status

Accepted

## Context

The project needs a consistent workflow for implementing the planned steps without committing half-finished work to main, and commit messages should be machine-readable for changelog generation and navigation.

## Decision

Use a `main`-protected branching model with short-lived feature branches (`feat/<scope>`, `fix/<scope>`, `chore/<scope>`, `docs/<scope>`). Each implementation step from the plan gets its own branch. Commits follow the Conventional Commits standard: `<type>(<scope>): <description>`, with scopes matching internal package names.

## Consequences

main always reflects a stable, working state. Branch history is preserved via no-fast-forward merges. Conventional commit format enables automated changelog generation and makes git log navigable by component. The overhead is one branch-per-step discipline during implementation.
